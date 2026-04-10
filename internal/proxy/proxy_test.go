package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
)

// newTestProxy builds a Proxy wired with a fresh in-memory kill-switch,
// in-memory meter, and the embedded pricing table.
func newTestProxy(t *testing.T, store keys.Store, openaiBase, anthropicBase string) *Proxy {
	t.Helper()
	pricer, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), pricer, openaiBase, anthropicBase)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// newTestKey creates and persists a fresh virtual key in an in-memory store.
func newTestKey(t *testing.T, upstream, realUpstreamKey string) (*keys.Memory, *keys.VirtualKey) {
	t.Helper()
	store := keys.NewMemory()
	id, err := keys.GenerateID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := keys.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	vk := &keys.VirtualKey{
		ID:          id,
		Token:       token,
		Name:        "test",
		Upstream:    upstream,
		UpstreamKey: realUpstreamKey,
		CreatedAt:   time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return store, vk
}

func TestProxy_OpenAIKeyResolution(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer upstream.Close()

	store, vk := newTestKey(t, keys.UpstreamOpenAI, "sk-real-upstream-secret")
	p := newTestProxy(t, store, upstream.URL, "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status: got %d want 200; body: %s", rec.Code, body)
	}
	if gotAuth != "Bearer sk-real-upstream-secret" {
		t.Errorf("upstream auth: got %q want Bearer sk-real-upstream-secret", gotAuth)
	}
}

func TestProxy_AnthropicKeyResolution(t *testing.T) {
	var gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_x","model":"claude-sonnet-4","usage":{"input_tokens":5,"output_tokens":7}}`))
	}))
	defer upstream.Close()

	store, vk := newTestKey(t, keys.UpstreamAnthropic, "sk-ant-real")
	p := newTestProxy(t, store, "https://api.openai.com", upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status: got %d want 200; body: %s", rec.Code, body)
	}
	if gotAPIKey != "sk-ant-real" {
		t.Errorf("upstream x-api-key: got %q want sk-ant-real", gotAPIKey)
	}
}

func TestProxy_MissingKey(t *testing.T) {
	store := keys.NewMemory()
	p := newTestProxy(t, store, "https://api.openai.com", "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing key: got %d want 401", rec.Code)
	}
}

func TestProxy_InvalidKey(t *testing.T) {
	store := keys.NewMemory()
	p := newTestProxy(t, store, "https://api.openai.com", "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer rein_live_unknown")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid key: got %d want 401", rec.Code)
	}
}

func TestProxy_UpstreamMismatch(t *testing.T) {
	// An Anthropic key used against an OpenAI-shaped path should 400.
	store, vk := newTestKey(t, keys.UpstreamAnthropic, "sk-ant-real")
	p := newTestProxy(t, store, "https://api.openai.com", "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("mismatch: got %d want 400", rec.Code)
	}
}

func TestProxy_RevokedKey(t *testing.T) {
	store, vk := newTestKey(t, keys.UpstreamOpenAI, "sk-real")
	if err := store.Revoke(context.Background(), vk.ID); err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, store, "https://api.openai.com", "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked: got %d want 401", rec.Code)
	}
}

func TestProxy_UnknownPath(t *testing.T) {
	store := keys.NewMemory()
	p := newTestProxy(t, store, "https://api.openai.com", "https://api.anthropic.com")
	req := httptest.NewRequest(http.MethodPost, "/v1/unknown", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: got %d want 404", rec.Code)
	}
}

func TestProxy_FrozenReturns503(t *testing.T) {
	store, vk := newTestKey(t, keys.UpstreamOpenAI, "sk-real")
	ks := killswitch.NewMemory()
	if err := ks.SetFrozen(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	pricer, _ := meter.LoadPricer()
	p, err := New(store, ks, meter.NewMemory(), pricer, "https://api.openai.com", "https://api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("frozen: got %d want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Errorf("expected Retry-After header on 503")
	}
}

func TestProxy_PassthroughWhenStoreNil(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o"}`))
	}))
	defer upstream.Close()

	p := newTestProxy(t, nil, upstream.URL, "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer sk-raw-passthrough")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if gotAuth != "Bearer sk-raw-passthrough" {
		t.Errorf("passthrough auth: got %q want Bearer sk-raw-passthrough", gotAuth)
	}
}

// newBudgetedKey creates a key with explicit daily/monthly caps.
func newBudgetedKey(t *testing.T, upstream, upstreamKey string, dailyCap, monthCap float64) (*keys.Memory, *keys.VirtualKey) {
	t.Helper()
	store := keys.NewMemory()
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "budgeted",
		Upstream: upstream, UpstreamKey: upstreamKey,
		DailyBudgetUSD: dailyCap, MonthBudgetUSD: monthCap,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}
	return store, vk
}

func TestProxy_BudgetExceededReturns402(t *testing.T) {
	store, vk := newBudgetedKey(t, keys.UpstreamOpenAI, "sk-real", 5.00, 0)
	pricer, _ := meter.LoadPricer()
	m := meter.NewMemory()
	// Seed the meter so the key is already at its daily cap.
	_ = m.Record(context.Background(), vk.ID, 5.00)

	p, err := New(store, killswitch.NewMemory(), m, pricer, "https://api.openai.com", "https://api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("budget breach: got %d want 402", rec.Code)
	}
}

func TestProxy_SpendIsRecordedAfterUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 1,000,000 input + 1,000,000 output on gpt-4o = $12.50.
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1000000,"completion_tokens":1000000,"total_tokens":2000000}}`))
	}))
	defer upstream.Close()

	store, vk := newBudgetedKey(t, keys.UpstreamOpenAI, "sk-real", 20.00, 0)
	pricer, _ := meter.LoadPricer()
	m := meter.NewMemory()
	p, err := New(store, killswitch.NewMemory(), m, pricer, upstream.URL, "https://api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	// First request: under $20 cap, should pass and record $12.50.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: got %d want 200", rec.Code)
	}

	// Second request: another $12.50 would bring spend to $25 which is over $20 cap.
	// Check is called BEFORE the upstream fetch, so it sees $12.50 already recorded
	// and the cap is not yet breached; this second request should also succeed.
	// A THIRD request should then 402.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req2.Header.Set("Authorization", "Bearer "+vk.Token)
	req2.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request: got %d want 200 (spend=$12.50, under $20 cap)", rec2.Code)
	}

	// Third request: spend is now $25, over $20 cap. Should 402.
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req3.Header.Set("Authorization", "Bearer "+vk.Token)
	req3.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusPaymentRequired {
		t.Errorf("third request: got %d want 402 (spend=$25 > $20 cap)", rec3.Code)
	}
}

// TestProxy_OpenAIStreamRecordsSpend verifies that a streaming OpenAI request
// flows through a tee, the final usage chunk is parsed, and spend is recorded.
func TestProxy_OpenAIStreamRecordsSpend(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 1M input + 1M output on gpt-4o = $12.50.
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":1000000,\"completion_tokens\":1000000,\"total_tokens\":2000000}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	store, vk := newBudgetedKey(t, keys.UpstreamOpenAI, "sk-real", 20.00, 0)
	pricer, _ := meter.LoadPricer()
	m := meter.NewMemory()
	p, err := New(store, killswitch.NewMemory(), m, pricer, upstream.URL, "https://api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	// Stream request that does NOT set stream_options.include_usage.
	// Rein should auto-inject it before forwarding.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("first stream: got %d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("client did not receive full stream: %s", rec.Body.String())
	}
	if !strings.Contains(string(gotBody), `"include_usage":true`) {
		t.Errorf("rein did not inject stream_options.include_usage, upstream saw: %s", gotBody)
	}

	// Stream bodies are wrapped in a tee that reports on Close. httptest
	// records the full response and closes the body before ServeHTTP returns,
	// so by the time we inspect the meter the spend should already be recorded.
	// A second streaming request now pushes us over the $20 cap.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req2.Header.Set("Authorization", "Bearer "+vk.Token)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second stream: got %d want 200 (spend=$12.50 < $20 cap)", rec2.Code)
	}

	// Third streaming request: spend is now $25, over $20 cap. Must 402.
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req3.Header.Set("Authorization", "Bearer "+vk.Token)
	req3.Header.Set("Content-Type", "application/json")
	rec3 := httptest.NewRecorder()
	p.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusPaymentRequired {
		t.Errorf("third stream: got %d want 402 (spend=$25 > $20 cap)", rec3.Code)
	}
}
