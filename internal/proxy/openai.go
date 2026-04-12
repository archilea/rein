package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/meter"
)

// OpenAI is a reverse-proxy handler for OpenAI-compatible endpoints.
//
// It forwards requests to the configured base URL (https://api.openai.com by default),
// and for non-streaming JSON responses it parses the `usage` field, computes
// the USD cost via the supplied Pricer, and records the spend via the Meter.
//
// Streaming responses (Content-Type: text/event-stream) are passed through
// unmodified. Streaming token extraction is tracked as a follow-up.
type OpenAI struct {
	base  *url.URL
	rp    *httputil.ReverseProxy
	meter meter.Meter
	// pricerHolder wraps the currently active *Pricer so operator-editable
	// pricing overrides (#25) can hot-swap the pricing table on SIGHUP
	// without taking a lock on the request hot path. Reads are a single
	// atomic pointer load. May be nil in tests that disable metering.
	pricerHolder *meter.PricerHolder
	// baseURLCache memoizes per-key upstream_base_url overrides as parsed
	// *url.URL pointers keyed by the raw string form. This keeps the hot
	// path to a single sync.Map.Load on cache hit and a one-time parse per
	// distinct override value on miss. Map entries are never evicted; the
	// set is bounded by the number of distinct per-key base URLs configured
	// on the process, which is operator-controlled and small.
	baseURLCache sync.Map
	// unknownModelLog is the dedupe logger for "model not in pricing table;
	// spend not recorded" WARN lines, so an operator pointing a key at a
	// provider whose models are outside the embedded table gets loud early
	// signals without log flooding under sustained traffic.
	unknownModelLog *unknownModelLogger
}

// NewOpenAI creates an OpenAI adapter that forwards to base (for example,
// "https://api.openai.com"). meter and pricerHolder may be nil to disable
// metering. The pricerHolder indirection replaces the previous direct
// *Pricer reference (#25) so operator-editable pricing overrides can swap
// the active pricing table at runtime without a restart.
func NewOpenAI(base string, m meter.Meter, pricerHolder *meter.PricerHolder) (*OpenAI, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse openai base url %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("openai base url must include scheme and host, got %q", base)
	}

	o := &OpenAI{
		base:            u,
		meter:           m,
		pricerHolder:    pricerHolder,
		unknownModelLog: newUnknownModelLogger(),
	}
	o.rp = &httputil.ReverseProxy{
		Rewrite:        o.rewrite,
		ModifyResponse: o.modifyResponse,
		ErrorHandler:   o.errorHandler,
		Transport:      upstreamTransport(),
		// Immediate flush for SSE streaming endpoints.
		FlushInterval: -1,
	}
	return o, nil
}

// ServeHTTP satisfies http.Handler.
func (o *OpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.rp.ServeHTTP(w, r)
}

// rewrite adapts the outbound request for the upstream.
// If a virtual key is present on the request context, its upstream key replaces
// the inbound Authorization header. Otherwise the inbound header is forwarded
// unchanged (pass-through mode, used in unit tests).
//
// For streaming requests we auto-inject stream_options.include_usage=true so
// that OpenAI returns a final usage chunk. Without this, streaming clients
// would silently bypass budget enforcement.
func (o *OpenAI) rewrite(r *httputil.ProxyRequest) {
	vk := VKeyFromContext(r.In.Context())
	target := o.base
	if vk != nil && vk.UpstreamBaseURL != "" {
		if override := o.lookupOverrideURL(vk.UpstreamBaseURL); override != nil {
			target = override
		}
	}
	r.SetURL(target)
	r.Out.Host = target.Host
	if vk != nil {
		r.Out.Header.Set("Authorization", "Bearer "+vk.UpstreamKey)
	}
	if o.meter != nil {
		injectStreamUsage(r.Out)
	}
}

// lookupOverrideURL returns a memoized *url.URL for a per-key upstream base
// URL override, parsing once per distinct value. A parse failure at hot-path
// time is cached as a typed-nil sentinel so repeat requests for the same
// malformed value do not re-parse and do not re-log. Callers treat a nil
// return as "fall back to the global default". In practice this path is
// unreachable because the admin handler validates the URL on create, but
// defense in depth keeps a future schema-change or manual DB edit from
// flooding logs or spiking CPU on the hot path.
func (o *OpenAI) lookupOverrideURL(raw string) *url.URL {
	if cached, ok := o.baseURLCache.Load(raw); ok {
		u, _ := cached.(*url.URL)
		return u // may be a typed-nil sentinel (negative cache hit)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		// Cache the negative result. LoadOrStore so a concurrent first-miss
		// on the same bad value logs at most once per distinct raw string,
		// even if two goroutines race through the parse path.
		if _, loaded := o.baseURLCache.LoadOrStore(raw, (*url.URL)(nil)); !loaded {
			slog.Warn("openai per-key base URL parse failed; falling back to global default",
				"upstream_base_url", raw, "err", err)
		}
		return nil
	}
	// LoadOrStore so a concurrent first-miss on the same raw value does
	// not leak two parsed URLs — both goroutines return the same pointer.
	actual, _ := o.baseURLCache.LoadOrStore(raw, parsed)
	return actual.(*url.URL)
}

// injectStreamUsage rewrites a JSON request body to set
// stream_options.include_usage=true if "stream":true is present and the
// client did not already configure stream_options.include_usage. A 1 MiB
// cap protects against degenerate bodies.
func injectStreamUsage(r *http.Request) {
	if r.Body == nil || r.Method != http.MethodPost {
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return
	}
	const maxBody = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	_ = r.Body.Close()
	if err != nil || len(raw) == 0 {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}

	replaceBody := func(b []byte) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		replaceBody(raw)
		return
	}
	stream, _ := body["stream"].(bool)
	if !stream {
		replaceBody(raw)
		return
	}
	opts, _ := body["stream_options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	if _, set := opts["include_usage"]; set {
		replaceBody(raw)
		return
	}
	opts["include_usage"] = true
	body["stream_options"] = opts
	rewritten, err := json.Marshal(body)
	if err != nil {
		replaceBody(raw)
		return
	}
	replaceBody(rewritten)
}

// modifyResponse inspects upstream responses for usage metadata.
func (o *OpenAI) modifyResponse(resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")

	if strings.HasPrefix(ct, "text/event-stream") {
		slog.Info("openai streaming response",
			"path", resp.Request.URL.Path,
			"status", resp.StatusCode,
		)
		o.wrapStream(resp)
		return nil
	}

	if !strings.HasPrefix(ct, "application/json") {
		return nil
	}

	// Buffer the body so we can both parse it and re-serve it.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read upstream body: %w", err)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		slog.Warn("close upstream body", "err", closeErr)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))

	if resp.StatusCode >= 400 {
		return nil
	}

	var payload struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// Not a usage-shaped response (for example, /v1/models list). Ignore.
		return nil
	}
	if payload.Usage.TotalTokens == 0 {
		return nil
	}

	slog.Info("openai usage",
		"path", resp.Request.URL.Path,
		"model", payload.Model,
		"input_tokens", payload.Usage.PromptTokens,
		"output_tokens", payload.Usage.CompletionTokens,
		"total_tokens", payload.Usage.TotalTokens,
	)
	if vk := VKeyFromContext(resp.Request.Context()); vk != nil {
		o.recordSpend(resp.Request.Context(), vk.ID, payload.Model,
			payload.Usage.PromptTokens, payload.Usage.CompletionTokens)
	}
	return nil
}

// wrapStream installs a streamMeter on resp.Body so that SSE chunks are
// parsed for usage as the stream flows to the client. Spend is recorded on
// a background context so an early client disconnect does not cancel the
// meter write.
func (o *OpenAI) wrapStream(resp *http.Response) {
	if o.meter == nil || o.pricerHolder == nil {
		return
	}
	vk := VKeyFromContext(resp.Request.Context())
	if vk == nil {
		return
	}
	keyID := vk.ID
	resp.Body = newStreamMeter(resp.Body, keys.UpstreamOpenAI,
		func(model string, in, out int) {
			slog.Info("openai stream usage",
				"model", model, "input_tokens", in, "output_tokens", out)
			o.recordSpend(context.Background(), keyID, model, in, out)
		})
}

// recordSpend looks up USD cost via the pricer and adds it to the key's
// spend bucket. Silent on unknown models (logged, but not an error) so a
// newly released model does not break the pipeline. The pricer is loaded
// via a single atomic pointer read from pricerHolder so operator-editable
// pricing overrides (#25) can hot-swap the active snapshot without any
// lock on the hot path.
func (o *OpenAI) recordSpend(ctx context.Context, keyID, model string, inputTokens, outputTokens int) {
	if o.meter == nil || o.pricerHolder == nil {
		return
	}
	pricer := o.pricerHolder.Load()
	if pricer == nil {
		return
	}
	cost, ok := pricer.Cost(keys.UpstreamOpenAI, model, inputTokens, outputTokens)
	if !ok {
		o.unknownModelLog.Warn("openai", keyID, model)
		return
	}
	if err := o.meter.Record(ctx, keyID, cost); err != nil {
		slog.Error("meter record failed", "err", err, "key_id", keyID)
	}
}

// errorHandler returns 502 with a logged reason when the upstream dial fails.
func (o *OpenAI) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("openai proxy error",
		"path", r.URL.Path,
		"err", err,
	)
	http.Error(w, "upstream error", http.StatusBadGateway)
}
