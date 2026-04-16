package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/archilea/rein/internal/concurrency"
	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
	"github.com/archilea/rein/internal/rates"
)

// errorEnvelope mirrors the api.errorEnvelope shape for test assertions.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// requireErrorCode unmarshals a structured error body and asserts its code.
func requireErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != wantCode {
		t.Errorf("error code: got %q want %q", env.Error.Code, wantCode)
	}
	if env.Error.Message == "" {
		t.Errorf("error message should be non-empty for code %q", wantCode)
	}
}

// newTestProxy builds a Proxy wired with a fresh in-memory kill-switch,
// in-memory meter, and the embedded pricing table.
func newTestProxy(t *testing.T, store keys.Store, openaiBase, anthropicBase string) *Proxy {
	t.Helper()
	pricer, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), nil, nil, meter.NewPricerHolder(pricer), openaiBase, anthropicBase)
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
	requireErrorCode(t, rec, CodeMissingKey)
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
	requireErrorCode(t, rec, CodeInvalidKey)
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
	requireErrorCode(t, rec, CodeUpstreamMismatch)
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
	requireErrorCode(t, rec, CodeKeyRevoked)
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
	requireErrorCode(t, rec, CodeUnknownRoute)
}

func TestProxy_FrozenReturns503(t *testing.T) {
	store, vk := newTestKey(t, keys.UpstreamOpenAI, "sk-real")
	ks := killswitch.NewMemory()
	if err := ks.SetFrozen(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	pricer, _ := meter.LoadPricer()
	p, err := New(store, ks, meter.NewMemory(), nil, nil, meter.NewPricerHolder(pricer), "https://api.openai.com", "https://api.anthropic.com")
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
	requireErrorCode(t, rec, CodeKillSwitchEngaged)
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

	p, err := New(store, killswitch.NewMemory(), m, nil, nil, meter.NewPricerHolder(pricer), "https://api.openai.com", "https://api.anthropic.com")
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
	requireErrorCode(t, rec, CodeBudgetExceeded)
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
	p, err := New(store, killswitch.NewMemory(), m, nil, nil, meter.NewPricerHolder(pricer), upstream.URL, "https://api.anthropic.com")
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
	requireErrorCode(t, rec3, CodeBudgetExceeded)
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
	p, err := New(store, killswitch.NewMemory(), m, nil, nil, meter.NewPricerHolder(pricer), upstream.URL, "https://api.anthropic.com")
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
	requireErrorCode(t, rec3, CodeBudgetExceeded)
}

// newRateLimitedKey creates a key with explicit rate limits.
func newRateLimitedKey(t *testing.T, upstream, upstreamKey string, rpsLimit, rpmLimit int) (*keys.Memory, *keys.VirtualKey) {
	t.Helper()
	store := keys.NewMemory()
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "rate-limited",
		Upstream: upstream, UpstreamKey: upstreamKey,
		RPSLimit: rpsLimit, RPMLimit: rpmLimit,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}
	return store, vk
}

func newTestProxyWithRates(t *testing.T, store keys.Store, r rates.Store, openaiBase, anthropicBase string) *Proxy {
	t.Helper()
	pricer, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), r, nil, meter.NewPricerHolder(pricer), openaiBase, anthropicBase)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProxy_RateLimitReturns429(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	store, vk := newRateLimitedKey(t, keys.UpstreamOpenAI, "sk-real", 1, 0)
	rl := rates.NewMemory()
	p := newTestProxyWithRates(t, store, rl, upstream.URL, "https://api.anthropic.com")

	// First request: should pass.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: got %d want 200", rec.Code)
	}

	// Second request: should 429.
	upstreamCalled = false
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req2.Header.Set("Authorization", "Bearer "+vk.Token)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d want 429", rec2.Code)
	}
	if got := rec2.Header().Get("Retry-After"); got == "" {
		t.Error("expected Retry-After header on 429")
	}
	requireErrorCode(t, rec2, CodeRateLimited)
	if upstreamCalled {
		t.Error("upstream should NOT be contacted on rate-limited request")
	}
}

func TestProxy_BudgetCheckBeforeRateLimit(t *testing.T) {
	store := keys.NewMemory()
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "budgeted-rate-limited",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-real",
		DailyBudgetUSD: 1.0, RPSLimit: 10,
		CreatedAt: time.Now().UTC(),
	}
	_ = store.Create(context.Background(), vk)

	pricer, _ := meter.LoadPricer()
	m := meter.NewMemory()
	_ = m.Record(context.Background(), vk.ID, 1.0) // exhaust budget
	rl := rates.NewMemory()
	p, _ := New(store, killswitch.NewMemory(), m, rl, nil, meter.NewPricerHolder(pricer), "https://api.openai.com", "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("budget+rate: got %d want 402 (budget should fire first)", rec.Code)
	}
	requireErrorCode(t, rec, CodeBudgetExceeded)
}

func TestProxy_NilRatesStoreIsNoop(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	store, vk := newRateLimitedKey(t, keys.UpstreamOpenAI, "sk-real", 10, 100)
	p := newTestProxyWithRates(t, store, nil, upstream.URL, "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("nil rates: got %d want 200", rec.Code)
	}
}

// --- concurrency cap tests (issue #27) ---

// newConcurrencyLimitedKey mints an in-memory store with a single key whose
// MaxConcurrent cap is set, so tests can exercise the hot-path concurrency
// check without touching other per-key features.
func newConcurrencyLimitedKey(t *testing.T, maxConcurrent int) (keys.Store, *keys.VirtualKey) {
	t.Helper()
	store := keys.NewMemory()
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "concurrency-limited",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-real",
		MaxConcurrent: maxConcurrent,
		CreatedAt:     time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}
	return store, vk
}

func newTestProxyWithConcurrency(t *testing.T, store keys.Store, c concurrency.Store, openaiBase, anthropicBase string) *Proxy {
	t.Helper()
	pricer, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), nil, c, meter.NewPricerHolder(pricer), openaiBase, anthropicBase)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// slowUpstream blocks each request until `unblock` is closed. Lets a test
// hold N requests in flight simultaneously so the (N+1)th races against
// the concurrency cap rather than resolving instantly.
func slowUpstream(unblock <-chan struct{}, inFlight *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		<-unblock
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
}

// TestProxy_ConcurrencyLimitReturns429 verifies that when a key's
// MaxConcurrent cap is at its limit, the next request receives 429 with
// Retry-After: 1 and the upstream is NOT contacted.
func TestProxy_ConcurrencyLimitReturns429(t *testing.T) {
	var upstreamHits atomic.Int64
	var inFlight atomic.Int64
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		inFlight.Add(1)
		defer inFlight.Add(-1)
		<-unblock
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	store, vk := newConcurrencyLimitedKey(t, 2)
	p := newTestProxyWithConcurrency(t, store, concurrency.NewMemory(), upstream.URL, "https://api.anthropic.com")
	srv := httptest.NewServer(p)
	defer srv.Close()

	fire := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o"}`))
		req.Header.Set("Authorization", "Bearer "+vk.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		return resp
	}

	// Launch 2 requests that will block at upstream, saturating the cap.
	var wg sync.WaitGroup
	wg.Add(2)
	var inflightResps [2]*http.Response
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			inflightResps[i] = fire()
		}()
	}

	// Wait until both are at upstream, i.e. both slots are held.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && inFlight.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if inFlight.Load() != 2 {
		close(unblock)
		wg.Wait()
		t.Fatalf("upstream did not see 2 in-flight; got %d", inFlight.Load())
	}

	// 3rd request: must be rejected with 429 Retry-After: 1, upstream unchanged.
	upstreamBefore := upstreamHits.Load()
	resp := fire()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("3rd request: got %d want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After: got %q want %q", got, "1")
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var env errorEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, respBody)
	}
	if env.Error.Code != CodeConcurrencyExceeded {
		t.Errorf("error code: got %q want %q", env.Error.Code, CodeConcurrencyExceeded)
	}
	if upstreamHits.Load() != upstreamBefore {
		t.Error("upstream was contacted on concurrency-rejected request")
	}

	// Release upstream. All in-flight complete successfully.
	close(unblock)
	wg.Wait()
	for _, r := range inflightResps {
		if r.StatusCode != http.StatusOK {
			t.Errorf("in-flight response: got %d want 200", r.StatusCode)
		}
		_ = r.Body.Close()
	}

	// After completion, the slot is free again. One more request must pass.
	unblock2 := make(chan struct{})
	close(unblock2) // pass-through immediately, upstream closure still uses original unblock which is already closed
	resp2 := fire()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("post-release request: got %d want 200", resp2.StatusCode)
	}
	_ = resp2.Body.Close()
}

// TestProxy_ConcurrencyCounterNeverLeaks fires 200 sequential requests
// through a key with MaxConcurrent=10 and verifies the counter returns to
// zero. Exercises the deferred Release on the happy path.
func TestProxy_ConcurrencyCounterNeverLeaks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	store, vk := newConcurrencyLimitedKey(t, 10)
	cs := concurrency.NewMemory()
	p := newTestProxyWithConcurrency(t, store, cs, upstream.URL, "https://api.anthropic.com")
	srv := httptest.NewServer(p)
	defer srv.Close()

	for i := 0; i < 200; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o"}`))
		req.Header.Set("Authorization", "Bearer "+vk.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}
	if got := cs.InFlight(vk.ID); got != 0 {
		t.Errorf("counter leak: InFlight=%d want 0", got)
	}
}

// TestProxy_ConcurrencyReleaseOnUpstreamError verifies that a 5xx from the
// upstream does not leak the slot.
func TestProxy_ConcurrencyReleaseOnUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	store, vk := newConcurrencyLimitedKey(t, 2)
	cs := concurrency.NewMemory()
	p := newTestProxyWithConcurrency(t, store, cs, upstream.URL, "https://api.anthropic.com")
	srv := httptest.NewServer(p)
	defer srv.Close()

	for i := 0; i < 10; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o"}`))
		req.Header.Set("Authorization", "Bearer "+vk.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if got := cs.InFlight(vk.ID); got != 0 {
		t.Errorf("slot leaked on upstream 5xx: InFlight=%d want 0", got)
	}
}

// TestProxy_ConcurrencyReleaseOnClientDisconnect verifies that a client
// canceling before the upstream responds does not leak the slot.
func TestProxy_ConcurrencyReleaseOnClientDisconnect(t *testing.T) {
	unblock := make(chan struct{})
	var inFlight atomic.Int64
	upstream := slowUpstream(unblock, &inFlight)
	// Order matters: unblock the upstream handler FIRST, then Close the
	// server. Close blocks until all in-flight handlers return.
	t.Cleanup(func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
		upstream.Close()
	})

	store, vk := newConcurrencyLimitedKey(t, 3)
	cs := concurrency.NewMemory()
	p := newTestProxyWithConcurrency(t, store, cs, upstream.URL, "https://api.anthropic.com")
	srv := httptest.NewServer(p)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	// Wait for upstream to register the in-flight request.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && inFlight.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if inFlight.Load() == 0 {
		cancel()
		<-done
		t.Fatal("upstream never saw request")
	}

	cancel()
	<-done

	// Give the proxy a moment to run its deferred Release after the adapter
	// unwinds. The deferred Release is synchronous with ServeHTTP returning,
	// so a short bounded wait is enough in practice.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && cs.InFlight(vk.ID) > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := cs.InFlight(vk.ID); got != 0 {
		t.Errorf("slot leaked on client disconnect: InFlight=%d want 0", got)
	}
}

// TestProxy_NilConcurrencyStoreIsNoop verifies that wiring a nil
// concurrency.Store leaves the proxy functional and the check disabled.
func TestProxy_NilConcurrencyStoreIsNoop(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	store, vk := newConcurrencyLimitedKey(t, 1) // tight cap, but no store to enforce it.
	p := newTestProxyWithConcurrency(t, store, nil, upstream.URL, "https://api.anthropic.com")
	srv := httptest.NewServer(p)
	defer srv.Close()

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o"}`))
		req.Header.Set("Authorization", "Bearer "+vk.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}
}

// TestProxy_UnlimitedKeyNeverTouchesConcurrencyStore ensures that a key
// with MaxConcurrent=0 does not create a map entry.
func TestProxy_UnlimitedKeyNeverTouchesConcurrencyStore(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	store := keys.NewMemory()
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "unlimited",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-real",
		MaxConcurrent: 0,
		CreatedAt:     time.Now().UTC(),
	}
	_ = store.Create(context.Background(), vk)

	cs := concurrency.NewMemory()
	p := newTestProxyWithConcurrency(t, store, cs, upstream.URL, "https://api.anthropic.com")
	srv := httptest.NewServer(p)
	defer srv.Close()

	for i := 0; i < 10; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o"}`))
		req.Header.Set("Authorization", "Bearer "+vk.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	if got := cs.InFlight(vk.ID); got != 0 {
		t.Errorf("unlimited key should never touch the store, got InFlight=%d", got)
	}
}

// --- internal error test (issue #75) ---

// failingSwitch is a kill-switch that always returns an error, used to
// exercise the CodeInternalError path in ServeHTTP.
type failingSwitch struct{}

func (failingSwitch) IsFrozen(_ context.Context) (bool, error) {
	return false, errors.New("injected kill-switch failure")
}

func (failingSwitch) SetFrozen(_ context.Context, _ bool) error {
	return errors.New("injected kill-switch failure")
}

func TestProxy_KillSwitchErrorReturnsInternalError(t *testing.T) {
	store, vk := newTestKey(t, keys.UpstreamOpenAI, "sk-real")
	pricer, _ := meter.LoadPricer()
	p, err := New(store, failingSwitch{}, meter.NewMemory(), nil, nil, meter.NewPricerHolder(pricer), "https://api.openai.com", "https://api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("kill-switch error: got %d want 500", rec.Code)
	}
	requireErrorCode(t, rec, CodeInternalError)
}
