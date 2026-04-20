// Package proxy implements Rein's streaming-aware reverse proxy for upstream LLM providers.
//
// The dispatcher (Proxy) does four things on every /v1/* request:
//  1. Validate that the path is known and determine the expected upstream provider.
//  2. Check the kill-switch. If engaged, return 503 immediately.
//  3. Resolve the inbound "Authorization: Bearer rein_live_..." header via the keystore.
//  4. Enforce the key's daily/monthly USD caps against the meter, returning 402 on breach.
//  5. Enforce per-key request velocity limits (RPS/RPM) against the rate limiter, returning 429 on breach.
//  6. Enforce per-key concurrency caps against the concurrency store, returning 429 on breach.
//  7. Route to the provider-specific adapter, which swaps in the real upstream key.
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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/archilea/rein/internal/api"
	"github.com/archilea/rein/internal/concurrency"
	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
	"github.com/archilea/rein/internal/rates"
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
	store       keys.Store
	killSwitch  killswitch.Switch
	meter       meter.Meter
	rates       rates.Store
	concurrency concurrency.Store
	openai      http.Handler
	anthropic   http.Handler
	// draining is the shutdown-drain flag (#76). When true, the hot path
	// rejects new /v1/* requests with 503 + Retry-After: 5 + a structured
	// "draining" envelope before touching the kill-switch or keystore.
	// Flipped by cmd/rein on SIGTERM/SIGINT; read on every request with a
	// single atomic load so the off-state cost is indistinguishable from
	// the pre-#76 hot path.
	draining atomic.Bool
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
//
// c may be nil, in which case per-key concurrency caps are disabled.
// Production wiring always provides a concurrency.Memory.
func New(store keys.Store, killSwitch killswitch.Switch, m meter.Meter, r rates.Store, c concurrency.Store, pricerHolder *meter.PricerHolder, openaiBase, anthropicBase string) (*Proxy, error) {
	oai, err := NewOpenAI(openaiBase, m, pricerHolder)
	if err != nil {
		return nil, err
	}
	ant, err := NewAnthropic(anthropicBase, m, pricerHolder)
	if err != nil {
		return nil, err
	}
	return &Proxy{
		store:       store,
		killSwitch:  killSwitch,
		meter:       m,
		rates:       r,
		concurrency: c,
		openai:      oai,
		anthropic:   ant,
	}, nil
}

// SetDraining flips the shutdown-drain flag. Idempotent: calling it
// multiple times with the same value is a no-op. Callers (cmd/rein's
// signal handler) set true on SIGTERM/SIGINT; there is no reverse
// transition in production, but the setter accepts false for tests
// that want to reset between cases.
func (p *Proxy) SetDraining(v bool) {
	p.draining.Store(v)
}

// IsDraining reports whether the drain flag is currently set. Lock-free
// single atomic load. Used by the /readyz handler so readiness probes
// read exactly the same state the proxy hot path reads.
func (p *Proxy) IsDraining() bool {
	return p.draining.Load()
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
		api.WriteError(w, http.StatusNotFound, CodeUnknownRoute, "unknown upstream route")
		return
	}

	// Drain fast-path (#76). Placed before the kill-switch so a replica in
	// drain mode during a rolling deploy sheds load with one atomic load
	// and one structured 503, cheaper than the kill-switch path.
	// Retry-After: 5 nudges clients to retry against a sibling replica via
	// the load balancer; drain is a planned signal, not an incident, so the
	// code is "draining" (distinct from "kill_switch_engaged").
	if p.draining.Load() {
		w.Header().Set("Retry-After", "5")
		api.WriteError(w, http.StatusServiceUnavailable, CodeDraining, "rein replica is draining; retry on another replica")
		return
	}

	// Fast-path: reject everything if the kill-switch is engaged.
	if p.killSwitch != nil {
		frozen, ksErr := p.killSwitch.IsFrozen(r.Context())
		if ksErr != nil {
			slog.Error("kill-switch read failed", "err", ksErr)
			api.WriteError(w, http.StatusInternalServerError, CodeInternalError, "kill-switch check failed")
			return
		}
		if frozen {
			w.Header().Set("Retry-After", "60")
			api.WriteError(w, http.StatusServiceUnavailable, CodeKillSwitchEngaged, "rein is frozen: kill-switch engaged")
			return
		}
	}

	vkey, err := p.resolveKey(r)
	if err != nil {
		switch {
		case errors.Is(err, errMissingKey):
			api.WriteError(w, http.StatusUnauthorized, CodeMissingKey, "missing rein key")
		case errors.Is(err, errInvalidKey):
			api.WriteError(w, http.StatusUnauthorized, CodeInvalidKey, "invalid rein key")
		default:
			api.WriteError(w, http.StatusInternalServerError, CodeInternalError, "key resolution failed")
		}
		return
	}

	if vkey != nil {
		if vkey.IsRevoked() {
			api.WriteError(w, http.StatusUnauthorized, CodeKeyRevoked, "virtual key revoked")
			return
		}
		// Belt-and-suspenders: a key whose expires_at has passed must
		// fail closed on every request even if the sweeper has not
		// yet stamped revoked_at. The response is deliberately
		// indistinguishable from manual revocation (same 401, same
		// code) so clients cannot enumerate which operator keys
		// expire when. For keys with ExpiresAt == nil this is a single
		// branch and short-circuits on the hot path.
		if vkey.IsExpired(time.Now().UTC()) {
			api.WriteError(w, http.StatusUnauthorized, CodeKeyRevoked, "virtual key revoked")
			return
		}
		if vkey.Upstream != wantUpstream {
			api.WriteError(w, http.StatusBadRequest, CodeUpstreamMismatch, "rein key upstream does not match request path")
			return
		}
		if p.meter != nil && (vkey.DailyBudgetUSD > 0 || vkey.MonthBudgetUSD > 0) {
			if err := p.meter.Check(r.Context(), vkey.ID, vkey.DailyBudgetUSD, vkey.MonthBudgetUSD); err != nil {
				if errors.Is(err, meter.ErrBudgetExceeded) {
					api.WriteError(w, http.StatusPaymentRequired, CodeBudgetExceeded, "budget exceeded for this virtual key")
					return
				}
				// Fail open on meter errors. The kill-switch is the independent hard stop.
				slog.Error("meter check failed, allowing request", "err", err, "key_id", vkey.ID)
			}
		}
		// Rate limit check. Runs after budget (cheaper) and before upstream dispatch.
		if p.rates != nil && (vkey.RPSLimit > 0 || vkey.RPMLimit > 0) {
			if err := p.rates.Allow(r.Context(), vkey.ID, vkey.RPSLimit, vkey.RPMLimit); err != nil {
				if errors.Is(err, rates.ErrRateLimited) {
					retryAfter := "1"
					if ra, ok := rates.RetryAfter(err); ok {
						retryAfter = strconv.Itoa(ra)
					}
					w.Header().Set("Retry-After", retryAfter)
					api.WriteError(w, http.StatusTooManyRequests, CodeRateLimited, "rate limit exceeded for this virtual key")
					return
				}
				// Fail open on rate limiter errors, same as meter.
				slog.Error("rate limit check failed, allowing request", "err", err, "key_id", vkey.ID)
			}
		}
		// Concurrency cap. Runs after rate limit (rejection there is cheaper)
		// and before upstream dispatch. defer Release fires on every exit
		// mode: happy path, upstream error, client disconnect mid-stream,
		// context cancel, and adapter panic (Go's defer runs during unwinding
		// before the net/http recovery handler). Zero cost when MaxConcurrent
		// is unlimited: Acquire returns true without touching the sync.Map.
		if p.concurrency != nil && vkey.MaxConcurrent > 0 {
			if !p.concurrency.Acquire(vkey.ID, vkey.MaxConcurrent) {
				w.Header().Set("Retry-After", "1")
				api.WriteError(w, http.StatusTooManyRequests, CodeConcurrencyExceeded, "concurrency limit exceeded for this virtual key")
				return
			}
			defer p.concurrency.Release(vkey.ID, vkey.MaxConcurrent)
		}
		r = r.WithContext(context.WithValue(r.Context(), vkeyContextKey{}, vkey))
		// Per-key upstream timeout. Only wraps the context when the key has
		// opted in (UpstreamTimeoutSeconds > 0); unlimited keys skip this
		// branch entirely and pay zero hot-path cost. defer cancel() releases
		// the timer goroutine on normal completion so the timer does not leak
		// waiting for the full deadline. The adapter's ErrorHandler translates
		// context.DeadlineExceeded into 504 on non-streaming responses; the
		// streaming tee reader handles it on SSE responses (see stream.go).
		if vkey.UpstreamTimeoutSeconds > 0 {
			ctx, cancel := context.WithTimeout(r.Context(),
				time.Duration(vkey.UpstreamTimeoutSeconds)*time.Second)
			defer cancel()
			r = r.WithContext(ctx)
		}
	}

	switch wantUpstream {
	case keys.UpstreamOpenAI:
		p.openai.ServeHTTP(w, r)
	case keys.UpstreamAnthropic:
		p.anthropic.ServeHTTP(w, r)
	default:
		api.WriteError(w, http.StatusInternalServerError, CodeInternalError, "no handler for upstream")
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
