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

// Anthropic is a reverse-proxy handler for Anthropic's Messages API.
//
// It forwards requests to the configured base URL (https://api.anthropic.com
// by default), swaps the inbound rein bearer for the real upstream key
// (carried in the x-api-key header, per Anthropic's conventions), and for
// non-streaming JSON responses it parses the usage field, computes USD cost
// via the supplied Pricer, and records the spend via the Meter.
type Anthropic struct {
	base   *url.URL
	rp     *httputil.ReverseProxy
	meter  meter.Meter
	pricer *meter.Pricer
}

// NewAnthropic creates an Anthropic adapter that forwards to base
// (for example, "https://api.anthropic.com"). meter and pricer may be nil to
// disable metering.
func NewAnthropic(base string, m meter.Meter, pricer *meter.Pricer) (*Anthropic, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse anthropic base url %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("anthropic base url must include scheme and host, got %q", base)
	}

	a := &Anthropic{base: u, meter: m, pricer: pricer}
	a.rp = &httputil.ReverseProxy{
		Rewrite:        a.rewrite,
		ModifyResponse: a.modifyResponse,
		ErrorHandler:   a.errorHandler,
		Transport:      upstreamTransport(),
		FlushInterval:  -1,
	}
	return a, nil
}

// ServeHTTP satisfies http.Handler.
func (a *Anthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.rp.ServeHTTP(w, r)
}

// rewrite adapts the outbound request for Anthropic.
// Anthropic uses x-api-key (not Authorization Bearer) and requires the
// anthropic-version header. The inbound Authorization header is stripped
// regardless to avoid leaking rein tokens upstream.
func (a *Anthropic) rewrite(r *httputil.ProxyRequest) {
	r.SetURL(a.base)
	r.Out.Host = a.base.Host

	r.Out.Header.Del("Authorization")

	if vk := VKeyFromContext(r.In.Context()); vk != nil {
		r.Out.Header.Set("x-api-key", vk.UpstreamKey)
	}

	// Default the required version header if the client did not send one.
	if r.Out.Header.Get("anthropic-version") == "" {
		r.Out.Header.Set("anthropic-version", "2023-06-01")
	}
}

// modifyResponse inspects upstream responses for usage metadata.
func (a *Anthropic) modifyResponse(resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")

	if strings.HasPrefix(ct, "text/event-stream") {
		slog.Info("anthropic streaming response",
			"path", resp.Request.URL.Path,
			"status", resp.StatusCode,
		)
		a.wrapStream(resp)
		return nil
	}

	if !strings.HasPrefix(ct, "application/json") {
		return nil
	}

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
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	if payload.Usage.InputTokens == 0 && payload.Usage.OutputTokens == 0 {
		return nil
	}

	slog.Info("anthropic usage",
		"path", resp.Request.URL.Path,
		"model", payload.Model,
		"input_tokens", payload.Usage.InputTokens,
		"output_tokens", payload.Usage.OutputTokens,
		"total_tokens", payload.Usage.InputTokens+payload.Usage.OutputTokens,
	)
	if vk := VKeyFromContext(resp.Request.Context()); vk != nil {
		a.recordSpend(resp.Request.Context(), vk.ID, payload.Model,
			payload.Usage.InputTokens, payload.Usage.OutputTokens)
	}
	return nil
}

// wrapStream installs a streamMeter on resp.Body so SSE chunks are parsed for
// usage as the stream flows. Anthropic emits input_tokens in message_start
// and a running output_tokens in each message_delta; the last message_delta
// carries the final total.
func (a *Anthropic) wrapStream(resp *http.Response) {
	if a.meter == nil || a.pricer == nil {
		return
	}
	vk := VKeyFromContext(resp.Request.Context())
	if vk == nil {
		return
	}
	keyID := vk.ID
	resp.Body = newStreamMeter(resp.Body, keys.UpstreamAnthropic,
		func(model string, in, out int) {
			slog.Info("anthropic stream usage",
				"model", model, "input_tokens", in, "output_tokens", out)
			a.recordSpend(context.Background(), keyID, model, in, out)
		})
}

func (a *Anthropic) recordSpend(ctx context.Context, keyID, model string, inputTokens, outputTokens int) {
	if a.meter == nil || a.pricer == nil {
		return
	}
	cost, ok := a.pricer.Cost(keys.UpstreamAnthropic, model, inputTokens, outputTokens)
	if !ok {
		slog.Warn("anthropic model not in pricing table; spend not recorded", "model", model)
		return
	}
	if err := a.meter.Record(ctx, keyID, cost); err != nil {
		slog.Error("meter record failed", "err", err, "key_id", keyID)
	}
}

// errorHandler returns 502 with a logged reason when the upstream dial fails.
func (a *Anthropic) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("anthropic proxy error",
		"path", r.URL.Path,
		"err", err,
	)
	http.Error(w, "upstream error", http.StatusBadGateway)
}
