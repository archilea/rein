// Package proxy implements Rein's streaming-aware reverse proxy for upstream LLM providers.
//
// The dispatcher (Proxy) does four things on every /v1/* request:
//  1. Validate that the path is known and determine the expected upstream provider.
//  2. Check the kill-switch. If engaged, return 503 immediately.
//  3. Resolve the inbound "Authorization: Bearer rein_live_..." header via the keystore.
//  4. Enforce the key's daily/monthly USD caps against the meter, returning 402 on breach.
//  5. Route to the provider-specific adapter, which swaps in the real upstream key.
//
// After the upstream responds, the provider adapter parses `usage` and records
// the USD cost with the meter so subsequent Check calls see the new total.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
)

// upstreamTransport is the shared http.RoundTripper used by every upstream
// adapter's httputil.ReverseProxy. It overrides Go's DefaultTransport in two
// ways that matter for a reverse proxy under load:
//
//  1. MaxIdleConnsPerHost is 200 (default 2). For a proxy, nearly all traffic
//     is to the same upstream host, so the default throttles connection reuse
//     and forces tens of thousands of short-lived TCP connections per minute,
//     exhausting ephemeral port ranges and adding latency.
//  2. DialContext has explicit timeouts so upstream network issues bound the
//     proxy's tail latency.
//
// Everything else is borrowed from DefaultTransport.
func upstreamTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// Proxy is the top-level HTTP handler for /v1/*. It checks the global kill-switch,
// validates virtual keys, enforces per-key budgets, routes to the correct
// provider adapter, and injects the real upstream key.
type Proxy struct {
	store      keys.Store
	killSwitch killswitch.Switch
	meter      meter.Meter
	openai     http.Handler
	anthropic  http.Handler
}

// New constructs a Proxy with handlers for each supported upstream.
//
// store may be nil for pass-through testing scenarios, in which case the
// inbound Authorization header is forwarded unchanged.
//
// killSwitch may be nil, in which case kill-switch enforcement is skipped.
// Production wiring in cmd/rein/main.go always provides a real switch.
//
// meter and pricer may be nil, in which case per-key budgets and spend
// recording are disabled. Production wiring always provides both.
func New(store keys.Store, killSwitch killswitch.Switch, m meter.Meter, pricerHolder *meter.PricerHolder, openaiBase, anthropicBase string) (*Proxy, error) {
	oai, err := NewOpenAI(openaiBase, m, pricerHolder)
	if err != nil {
		return nil, err
	}
	ant, err := NewAnthropic(anthropicBase, m, pricerHolder)
	if err != nil {
		return nil, err
	}
	return &Proxy{
		store:      store,
		killSwitch: killSwitch,
		meter:      m,
		openai:     oai,
		anthropic:  ant,
	}, nil
}

// vkeyContextKey is the key used to stash a resolved VirtualKey on a request context.
type vkeyContextKey struct{}

// VKeyFromContext returns the resolved virtual key stashed on ctx, or nil if none.
func VKeyFromContext(ctx context.Context) *keys.VirtualKey {
	v, _ := ctx.Value(vkeyContextKey{}).(*keys.VirtualKey)
	return v
}

var (
	errMissingKey = errors.New("missing rein key")
	errInvalidKey = errors.New("invalid rein key")
)

// expectedUpstreamForPath returns the provider the given path is shaped for,
// or "" if the path is unknown to Rein.
func expectedUpstreamForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/chat/"),
		strings.HasPrefix(path, "/v1/completions"),
		strings.HasPrefix(path, "/v1/embeddings"),
		strings.HasPrefix(path, "/v1/models"),
		strings.HasPrefix(path, "/v1/audio/"),
		strings.HasPrefix(path, "/v1/images/"):
		return keys.UpstreamOpenAI
	case strings.HasPrefix(path, "/v1/messages"):
		return keys.UpstreamAnthropic
	}
	return ""
}

// ServeHTTP dispatches a request to the right upstream adapter after checking
// the kill-switch and validating the rein virtual key.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wantUpstream := expectedUpstreamForPath(r.URL.Path)
	if wantUpstream == "" {
		http.Error(w, "unknown upstream route", http.StatusNotFound)
		return
	}

	// Fast-path: reject everything if the kill-switch is engaged.
	if p.killSwitch != nil {
		frozen, ksErr := p.killSwitch.IsFrozen(r.Context())
		if ksErr != nil {
			slog.Error("kill-switch read failed", "err", ksErr)
			http.Error(w, "kill-switch check failed", http.StatusInternalServerError)
			return
		}
		if frozen {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rein is frozen: kill-switch engaged", http.StatusServiceUnavailable)
			return
		}
	}

	vkey, err := p.resolveKey(r)
	if err != nil {
		switch {
		case errors.Is(err, errMissingKey):
			http.Error(w, "missing rein key", http.StatusUnauthorized)
		case errors.Is(err, errInvalidKey):
			http.Error(w, "invalid rein key", http.StatusUnauthorized)
		default:
			http.Error(w, "key resolution failed", http.StatusInternalServerError)
		}
		return
	}

	if vkey != nil {
		if vkey.IsRevoked() {
			http.Error(w, "virtual key revoked", http.StatusUnauthorized)
			return
		}
		if vkey.Upstream != wantUpstream {
			http.Error(w, "rein key upstream does not match request path", http.StatusBadRequest)
			return
		}
		if p.meter != nil && (vkey.DailyBudgetUSD > 0 || vkey.MonthBudgetUSD > 0) {
			if err := p.meter.Check(r.Context(), vkey.ID, vkey.DailyBudgetUSD, vkey.MonthBudgetUSD); err != nil {
				if errors.Is(err, meter.ErrBudgetExceeded) {
					http.Error(w, "budget exceeded for this virtual key", http.StatusPaymentRequired)
					return
				}
				// Fail open on meter errors. The kill-switch is the independent hard stop.
				slog.Error("meter check failed, allowing request", "err", err, "key_id", vkey.ID)
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), vkeyContextKey{}, vkey))
	}

	switch wantUpstream {
	case keys.UpstreamOpenAI:
		p.openai.ServeHTTP(w, r)
	case keys.UpstreamAnthropic:
		p.anthropic.ServeHTTP(w, r)
	default:
		http.Error(w, "no handler for upstream", http.StatusInternalServerError)
	}
}

// resolveKey extracts the inbound Bearer token and looks it up in the keystore.
// Returns (nil, nil) if the store is unconfigured (pass-through mode).
func (p *Proxy) resolveKey(r *http.Request) (*keys.VirtualKey, error) {
	if p.store == nil {
		return nil, nil
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, errMissingKey
	}
	const bearer = "Bearer "
	if !strings.HasPrefix(auth, bearer) {
		return nil, errMissingKey
	}
	token := strings.TrimPrefix(auth, bearer)
	if !keys.IsReinToken(token) {
		return nil, errInvalidKey
	}
	vk, err := p.store.GetByToken(r.Context(), token)
	if err != nil {
		return nil, errInvalidKey
	}
	return vk, nil
}
