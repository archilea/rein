package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
)

// TestPricingOverride_BudgetCapFiresOnBreach is the integration test
// that #25's acceptance criteria explicitly asked for and that my
// original PR #44 missed. It wires the full chain end-to-end with a
// Groq-shaped httptest mock upstream, a rein.json with
// llama-3.3-70b-versatile pricing, a virtual key with a tiny
// daily_budget_usd, and drives enough requests to breach the cap,
// asserting the breaching request returns 402 Payment Required from
// Rein without the upstream being contacted.
//
// This is the single test that proves the full pipeline works:
//
//	LoadConfigFile -> merged Pricer -> PricerHolder -> Adapter.recordSpend
//	    -> meter.Record -> meter.Check -> 402 on next request
//
// Unit tests cover each layer in isolation. Real-Groq smoke tests prove
// network connectivity. Neither exercises the breach-fires-cap chain
// deterministically, which is what the acceptance criteria asked for
// and what this test closes.
func TestPricingOverride_BudgetCapFiresOnBreach(t *testing.T) {
	// 1. Groq-shaped mock upstream. Path is /openai/v1/chat/completions
	//    because the virtual key's upstream_base_url will be
	//    http://<mock>/openai and Rein's OpenAI adapter composes the
	//    full path by joining with /v1/chat/completions. Returns a
	//    valid OpenAI-shaped JSON response with usage tokens.
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.URL.Path != "/openai/v1/chat/completions" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-integration-test",
			"object": "chat.completion",
			"created": 1775000000,
			"model": "llama-3.3-70b-versatile",
			"choices": [
				{"index": 0, "message": {"role": "assistant", "content": "test"}, "finish_reason": "stop"}
			],
			"usage": {"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}
		}`))
	}))
	defer upstream.Close()

	// 2. Write a rein.json that prices llama-3.3-70b-versatile at an
	//    absurdly high rate so the test hits the cap on the second
	//    request. At $1000 per million input tokens and $1000 per
	//    million output tokens, each request at 100+10 tokens costs
	//    (100/1e6)*1000 + (10/1e6)*1000 = 0.1 + 0.01 = $0.11. A daily
	//    cap of $0.05 is breached after the first Record, so the
	//    second request's Check returns 402.
	//
	//    This is intentional over-pricing to keep the test deterministic
	//    and fast. Real operator values would be sub-dollar per million
	//    tokens; the test's pricing bears no relationship to reality.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rein.json")
	if err := os.WriteFile(configPath, []byte(`{
		"version": "1",
		"source": "integration test",
		"models": {
			"openai": {
				"llama-3.3-70b-versatile": {
					"input_per_mtok": 1000.0,
					"output_per_mtok": 1000.0
				}
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Load + merge. Mirrors exactly what cmd/rein/main.go does at
	//    startup. The returned Pricer contains the embedded table plus
	//    the llama override. Wrap in a PricerHolder because the adapters
	//    take *PricerHolder (not *Pricer) after #25.
	base, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := meter.LoadConfigFile(configPath, base)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	holder := meter.NewPricerHolder(merged)

	// 4. Build the full proxy pipeline. Mint a virtual key with a tiny
	//    daily_budget_usd and the per-key upstream_base_url pointing at
	//    the mock upstream's /openai prefix — exactly the shape an
	//    operator would use against real Groq.
	const dailyCap = 0.05 // dollars
	store := keys.NewMemory()
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID:              id,
		Token:           token,
		Name:            "integration-test",
		Upstream:        keys.UpstreamOpenAI,
		UpstreamKey:     "gsk-fake-integration-test",
		UpstreamBaseURL: upstream.URL + "/openai",
		DailyBudgetUSD:  dailyCap,
		CreatedAt:       time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}

	spendMeter := meter.NewMemory()
	p, err := New(
		store,
		killswitch.NewMemory(),
		spendMeter,
		nil,
		nil,
		holder,
		// Deliberately-unreachable default upstream. If the override is
		// ever silently ignored, the test fails loudly with a dial
		// error instead of a subtle spend-recording regression.
		"http://127.0.0.1:1",
		"https://api.anthropic.com",
	)
	if err != nil {
		t.Fatal(err)
	}

	// 5. Drive requests until the cap fires. Each request should go
	//    200 until the cap is breached, then 402. With per-request
	//    cost ~$0.11 and cap $0.05, request 1 records $0.11 and
	//    request 2 fails the Check with ErrBudgetExceeded -> 402.
	doRequest := func(label string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader([]byte(`{"model":"llama-3.3-70b-versatile","messages":[{"role":"user","content":"t"}]}`)))
		req.Header.Set("Authorization", "Bearer "+vk.Token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	rec1 := doRequest("first")
	if rec1.Code != http.StatusOK {
		body, _ := io.ReadAll(rec1.Body)
		t.Fatalf("first request should be 200 (spend not yet recorded at Check time); got %d body=%s", rec1.Code, body)
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("upstream should have been contacted once after first request; got %d hits", upstreamHits.Load())
	}

	rec2 := doRequest("second")
	if rec2.Code != http.StatusPaymentRequired {
		body, _ := io.ReadAll(rec2.Body)
		t.Fatalf("second request should be 402 (budget cap breached by first request's recorded spend); got %d body=%s", rec2.Code, body)
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("upstream should NOT have been contacted for the 402 request; got %d hits (budget enforcement failed to block before upstream dial)", upstreamHits.Load())
	}

	// 6. Cross-check that the meter actually recorded the override
	//    cost (not zero, not the embedded default). This is the final
	//    proof that the override made it all the way through the
	//    LoadConfigFile -> PricerHolder -> Cost() -> meter.Record
	//    chain. Calling Check with a cap slightly above the expected
	//    recorded cost should succeed; calling with a cap below it
	//    should return ErrBudgetExceeded.
	ctx := context.Background()
	expectedFirstRequestCost := 110.0 / 1_000_000.0 * 1000.0 // 100 input + 10 output tokens at $1000/MTok
	// The cap after one request is $0.05; meter has recorded $0.11.
	// 0.05 < 0.11, so Check(daily=$0.05) returns ErrBudgetExceeded.
	if err := spendMeter.Check(ctx, vk.ID, dailyCap, 0); err == nil {
		t.Errorf("meter.Check with daily_cap=%.2f should be ErrBudgetExceeded; total spend was supposed to exceed the cap", dailyCap)
	}
	// And Check with a loose cap that is well above the recorded spend
	// should succeed, proving spend is non-zero but bounded.
	if err := spendMeter.Check(ctx, vk.ID, expectedFirstRequestCost*10, 0); err != nil {
		t.Errorf("meter.Check with generous cap should pass but got %v; recorded spend is out of expected range", err)
	}
}

// TestPricingOverride_MergedFromDefaultPath is the second integration
// test that pins the hybrid config file resolution end-to-end. It
// overrides meter's perspective of the default path (indirectly, via
// the config package) is not reachable from here, so instead this test
// exercises the LoadConfigFile -> PricerHolder -> Adapter pipeline with
// an explicit path (same code as the env_var branch in main.go) and
// confirms the merged pricer resolves the override entry correctly at
// request time. The default-path resolution itself is covered by
// internal/config/config_test.go.
func TestPricingOverride_MergedEntryResolvesAtRequestTime(t *testing.T) {
	// Mock upstream that records the Authorization header so we can
	// verify the per-key upstream_key was swapped in correctly.
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-merged-test",
			"model": "custom-provider-model",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 50, "completion_tokens": 5, "total_tokens": 55}
		}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "rein.json")
	if err := os.WriteFile(configPath, []byte(`{
		"version": "1",
		"models": {
			"openai": {
				"custom-provider-model": {"input_per_mtok": 2.5, "output_per_mtok": 3.5}
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	base, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := meter.LoadConfigFile(configPath, base)
	if err != nil {
		t.Fatal(err)
	}
	holder := meter.NewPricerHolder(merged)

	store := keys.NewMemory()
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "merged-test",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-custom-provider-real",
		UpstreamBaseURL: upstream.URL,
		CreatedAt:       time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}

	// Use a dedicated meter instance so we can inspect whether the
	// merged pricer's override entry resolved (non-zero recorded spend).
	spendMeter := meter.NewMemory()
	p, err := New(store, killswitch.NewMemory(), spendMeter, nil, nil, holder,
		"http://127.0.0.1:1", "https://api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"custom-provider-model","messages":[{"role":"user","content":"t"}]}`)))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, body)
	}
	if gotAuth != "Bearer sk-custom-provider-real" {
		t.Errorf("upstream auth: got %q want Bearer sk-custom-provider-real (per-key swap broken)", gotAuth)
	}

	// Verify the response body pipeline is intact — the response JSON
	// should include the usage block we asserted in the mock.
	body, _ := io.ReadAll(rec.Body)
	var payload struct {
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response body not JSON: %v; body=%s", err, body)
	}
	if payload.Usage.TotalTokens != 55 {
		t.Errorf("usage.total_tokens: got %d want 55", payload.Usage.TotalTokens)
	}

	// The recorded spend should be > 0 because custom-provider-model is
	// in the override table. With (50/1e6)*2.5 + (5/1e6)*3.5 = 0.000125
	// + 0.0000175 = ~0.00014, a cap of 0.001 should pass and a cap of
	// 0.00001 should not.
	ctx := context.Background()
	if err := spendMeter.Check(ctx, vk.ID, 0.001, 0); err != nil {
		t.Errorf("meter Check with generous cap should pass; got %v (merged override did not record spend)", err)
	}
	if err := spendMeter.Check(ctx, vk.ID, 0.00001, 0); err == nil {
		t.Errorf("meter Check with tight cap should have been ErrBudgetExceeded; spend appears to be 0 (override entry not priced)")
	}
}
