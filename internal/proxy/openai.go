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

	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/meter"
)

// OpenAI is a reverse-proxy handler for OpenAI-compatible endpoints.
//
// It forwards requests to the configured base URL (https://api.openai.com by default),
// and for both non-streaming JSON and streaming (SSE) responses it parses the
// usage/token counts, computes the USD cost via the Pricer, and records spend.
type OpenAI struct {
	base   *url.URL
	rp     *httputil.ReverseProxy
	meter  meter.Meter
	pricer *meter.Pricer
}

// NewOpenAI creates an OpenAI adapter that forwards to base (for example,
// "https://api.openai.com"). meter and pricer may be nil to disable metering.
func NewOpenAI(base string, m meter.Meter, pricer *meter.Pricer) (*OpenAI, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse openai base url %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("openai base url must include scheme and host, got %q", base)
	}

	o := &OpenAI{base: u, meter: m, pricer: pricer}
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
	r.SetURL(o.base)
	r.Out.Host = o.base.Host
	if vk := VKeyFromContext(r.In.Context()); vk != nil {
		r.Out.Header.Set("Authorization", "Bearer "+vk.UpstreamKey)
	}
	if o.meter != nil {
		injectStreamUsage(r.Out)
	}
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
	if o.meter == nil || o.pricer == nil {
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
// newly released model does not break the pipeline.
func (o *OpenAI) recordSpend(ctx context.Context, keyID, model string, inputTokens, outputTokens int) {
	if o.meter == nil || o.pricer == nil {
		return
	}
	cost, ok := o.pricer.Cost(keys.UpstreamOpenAI, model, inputTokens, outputTokens)
	if !ok {
		slog.Warn("openai model not in pricing table; spend not recorded", "model", model)
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
