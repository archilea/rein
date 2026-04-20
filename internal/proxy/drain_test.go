package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
)

// TestProxy_DrainFlagSetDrainingAndIsDraining pins the happy-path
// behaviour of the #76 drain flag: SetDraining toggles the atomic
// and IsDraining reflects it, both idempotent.
func TestProxy_DrainFlagSetDrainingAndIsDraining(t *testing.T) {
	p := newTestProxy(t, nil, "https://api.openai.com", "https://api.anthropic.com")
	if p.IsDraining() {
		t.Fatal("fresh Proxy must start with draining=false")
	}

	p.SetDraining(true)
	if !p.IsDraining() {
		t.Errorf("after SetDraining(true): got false, want true")
	}
	// Idempotent: second call stays true.
	p.SetDraining(true)
	if !p.IsDraining() {
		t.Errorf("double SetDraining(true): want true")
	}
	p.SetDraining(false)
	if p.IsDraining() {
		t.Errorf("after SetDraining(false): want false")
	}
}

// TestProxy_DrainReturns503WithRetryAfter covers the hot-path 503
// envelope emitted during drain. The shape is fixed by the #76 AC:
// 503 Service Unavailable, Retry-After: 5, structured JSON envelope
// with code="draining".
func TestProxy_DrainReturns503WithRetryAfter(t *testing.T) {
	p := newTestProxy(t, nil, "https://api.openai.com", "https://api.anthropic.com")
	p.SetDraining(true)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After: got %q want %q", got, "5")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", ct)
	}
	requireErrorCode(t, rec, CodeDraining)
}

// TestProxy_DrainShortCircuitsBeforeKillSwitch is the ordering contract
// spelled out in the issue: drain is placed before the kill-switch so
// a drain-state 503 is cheaper than a frozen-state 503. If both flags
// are set the response must be "draining", not "kill_switch_engaged".
// A regression here would flip the perceived reason for the 503 and
// point operators at the wrong control surface during a rolling deploy
// that happened to coincide with a kill-switch event.
func TestProxy_DrainShortCircuitsBeforeKillSwitch(t *testing.T) {
	p := newTestProxy(t, nil, "https://api.openai.com", "https://api.anthropic.com")
	p.SetDraining(true)
	// Also freeze the kill-switch. A broken implementation that checks
	// the kill-switch first would report that code instead of draining.
	if p.killSwitch == nil {
		t.Fatal("newTestProxy must wire a kill-switch for this test")
	}
	// Reach the switch's frozen state via its public API. The test
	// helper casts through killswitch.Memory which is what newTestProxy
	// installs; if someone swaps the default this test will fail fast.
	ks, ok := p.killSwitch.(*killswitch.Memory)
	if !ok {
		t.Fatalf("expected *killswitch.Memory, got %T", p.killSwitch)
	}
	if err := ks.SetFrozen(context.Background(), true); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After from drain path: got %q want 5", got)
	}
	requireErrorCode(t, rec, CodeDraining)
}

// TestProxy_DrainStillHonorsUnknownRoute404 confirms unknown /v1/*
// paths still fast-fail with 404 / unknown_route during drain. A
// 404-able request is a client error, not a shutdown event, so the
// useful error code wins over the generic drain 503.
func TestProxy_DrainStillHonorsUnknownRoute404(t *testing.T) {
	p := newTestProxy(t, nil, "https://api.openai.com", "https://api.anthropic.com")
	p.SetDraining(true)

	req := httptest.NewRequest(http.MethodGet, "/v1/not-a-real-route", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d want 404", rec.Code)
	}
	requireErrorCode(t, rec, CodeUnknownRoute)
}

// TestProxy_DrainOffPassesThroughNormally is the regression fence for
// the "zero hot-path overhead when not draining" constraint. A fresh
// proxy (draining==false) must serve requests identically to pre-#76
// behaviour. We verify indirectly: send a request, expect anything
// except the draining envelope.
func TestProxy_DrainOffPassesThroughNormally(t *testing.T) {
	store, vk := newTestKey(t, keys.UpstreamOpenAI, "sk-real")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	p := newTestProxy(t, store, upstream.URL, "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("draining off: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err == nil && env.Error.Code == CodeDraining {
		t.Errorf("draining envelope leaked into normal path")
	}
}
