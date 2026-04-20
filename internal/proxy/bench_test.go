package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/archilea/rein/internal/concurrency"
	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
	"github.com/archilea/rein/internal/rates"
)

// These benchmarks measure Rein's own code overhead using an in-process mock
// upstream. They are not a substitute for a production load test, but they
// bound Rein's per-request CPU cost and the kill-switch fast-path throughput.
//
// Run with:
//
//	go test ./internal/proxy -bench . -benchtime=3s -cpu=4
//
// Set REIN_BENCH_QUIET=1 to silence slog during long runs. See bench_init_test.go.

var mockResponse = []byte(`{"id":"chatcmpl-bench","model":"gpt-4o","object":"chat.completion","created":1775000000,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":1,"total_tokens":13}}`)

func mockUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mockResponse)
	}))
}

// mockUpstreamWithLatency simulates a real LLM upstream that takes `latency`
// to produce a response. Sleeps on the server goroutine so concurrent
// requests pile up through Rein's outbound pool as they would in production.
func mockUpstreamWithLatency(latency time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(latency)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mockResponse)
	}))
}

func buildStore(b *testing.B, useSQLite bool) keys.Store {
	b.Helper()
	if !useSQLite {
		return keys.NewMemory()
	}
	key := make([]byte, keys.AESKeySize)
	for i := range key {
		key[i] = byte(i * 7)
	}
	cipher, err := keys.NewAESGCM(key)
	if err != nil {
		b.Fatal(err)
	}
	s, err := keys.NewSQLite(filepath.Join(b.TempDir(), "bench.db"), cipher)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func benchSetup(b *testing.B, useSQLite, withBudget bool, upstream *httptest.Server) (string, string) {
	b.Helper()
	return benchSetupOverride(b, useSQLite, withBudget, upstream, "")
}

// benchSetupOverride variant lets a benchmark mint a test key with an
// explicit UpstreamBaseURL, so BenchmarkRein_PerKeyBaseURLOverride_*
// can isolate the hot-path cost of the #24 override branch against
// the default benchmarks.
func benchSetupOverride(b *testing.B, useSQLite, withBudget bool, upstream *httptest.Server, baseURLOverride string) (string, string) {
	b.Helper()
	store := buildStore(b, useSQLite)

	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "bench",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-fake",
		UpstreamBaseURL: baseURLOverride,
		CreatedAt:       time.Now().UTC(),
	}
	if withBudget {
		vk.DailyBudgetUSD = 1_000_000
		vk.MonthBudgetUSD = 1_000_000
	}
	if err := store.Create(context.Background(), vk); err != nil {
		b.Fatal(err)
	}

	// When the benchmark key carries an override, the proxy's "global"
	// upstream base URL is irrelevant for that key; pass a known-bad
	// placeholder so the benchmark fails loudly if the override is ever
	// silently ignored.
	proxyOpenAIBase := upstream.URL
	if baseURLOverride != "" {
		proxyOpenAIBase = "http://127.0.0.1:1"
	}

	pricer, err := meter.LoadPricer()
	if err != nil {
		b.Fatal(err)
	}
	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), nil, nil, meter.NewPricerHolder(pricer), proxyOpenAIBase, "https://api.anthropic.com")
	if err != nil {
		b.Fatal(err)
	}

	rein := httptest.NewServer(p)
	b.Cleanup(rein.Close)

	return rein.URL, token
}

func drive200(b *testing.B, reinURL, token string) {
	payload := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"b"}]}`)
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 500,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", reinURL+"/v1/chat/completions", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				b.Fatalf("status %d", resp.StatusCode)
			}
		}
	})
}

// --- zero-latency benchmarks (isolate Rein's own per-request overhead) ---

// Floor on Rein's per-request CPU cost (in-memory keystore, no budget).
func BenchmarkRein_MemStore_NoBudget_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetup(b, false, false, up)
	drive200(b, url, tok)
}

// Isolates the cost of meter.Check + meter.Record on the in-memory meter.
func BenchmarkRein_MemStore_WithBudget_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetup(b, false, true, up)
	drive200(b, url, tok)
}

// Real production keystore: SELECT by token, AES-256-GCM decrypt of
// upstream_key per request, row scan.
func BenchmarkRein_SQLite_NoBudget_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetup(b, true, false, up)
	drive200(b, url, tok)
}

// Full production hot path: SQLite keystore + encryption + budget enforcement.
// This is the number to quote for "how fast is Rein in production config".
func BenchmarkRein_SQLite_WithBudget_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetup(b, true, true, up)
	drive200(b, url, tok)
}

// Full production hot path with a per-key upstream_base_url override set
// (#24). The extra work vs BenchmarkRein_SQLite_WithBudget_ZeroLatency is
// one sync.Map.Load on the cached *url.URL pointer, expected to be in the
// single-digit nanoseconds range. A regression here means the override
// cache is not doing its job.
func BenchmarkRein_SQLite_WithBudget_PerKeyOverride_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetupOverride(b, true, true, up, up.URL)
	drive200(b, url, tok)
}

// benchSetupExpires mirrors benchSetup but mints a key with a future
// expires_at so the hot-path IsExpired branch runs on every request.
// The extra cost vs BenchmarkRein_SQLite_WithBudget_ZeroLatency is one
// nil check + one time comparison (+ a UTC conversion). A regression
// here would mean the IsExpired helper picked up an allocation or map
// lookup.
func benchSetupExpires(b *testing.B, upstream *httptest.Server) (string, string) {
	b.Helper()
	store := buildStore(b, true)
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	future := time.Now().UTC().Add(24 * time.Hour)
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "bench-exp",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-fake",
		DailyBudgetUSD: 1_000_000, MonthBudgetUSD: 1_000_000,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: &future,
	}
	if err := store.Create(context.Background(), vk); err != nil {
		b.Fatal(err)
	}
	pricer, err := meter.LoadPricer()
	if err != nil {
		b.Fatal(err)
	}
	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), nil, nil,
		meter.NewPricerHolder(pricer), upstream.URL, "https://api.anthropic.com")
	if err != nil {
		b.Fatal(err)
	}
	rein := httptest.NewServer(p)
	b.Cleanup(rein.Close)
	return rein.URL, token
}

// BenchmarkRein_SQLite_WithBudget_WithExpiresAt_ZeroLatency measures
// the hot-path cost of the #77 IsExpired check when the key does carry
// a future expires_at. Compare against BenchmarkRein_SQLite_WithBudget_ZeroLatency
// (same setup, ExpiresAt == nil): the delta must be within noise.
func BenchmarkRein_SQLite_WithBudget_WithExpiresAt_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetupExpires(b, up)
	drive200(b, url, tok)
}

// --- realistic-latency benchmarks (throughput is bounded by upstream) ---

// 500ms upstream: typical gpt-4o short response. Throughput is
// concurrency / upstream_latency; Rein's overhead is rounding error.
func BenchmarkRein_SQLite_WithBudget_500msLatency(b *testing.B) {
	up := mockUpstreamWithLatency(500 * time.Millisecond)
	b.Cleanup(up.Close)
	url, tok := benchSetup(b, true, true, up)
	drive200(b, url, tok)
}

// 2000ms upstream: slow-model or long-context request.
func BenchmarkRein_SQLite_WithBudget_2sLatency(b *testing.B) {
	up := mockUpstreamWithLatency(2 * time.Second)
	b.Cleanup(up.Close)
	url, tok := benchSetup(b, true, true, up)
	drive200(b, url, tok)
}

// --- kill-switch fast-path benchmark ---

// Kill-switch engaged: every request is rejected at the first check. No
// keystore lookup, no upstream fetch, no budget evaluation. Atomic bool
// read + 503 write. This is how fast Rein can shred traffic during an
// incident.
func BenchmarkRein_Frozen(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)

	store := buildStore(b, true)
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	_ = store.Create(context.Background(), &keys.VirtualKey{
		ID: id, Token: token, Name: "bench",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-fake",
		CreatedAt: time.Now().UTC(),
	})

	ks := killswitch.NewMemory()
	_ = ks.SetFrozen(context.Background(), true)

	pricer, _ := meter.LoadPricer()
	p, _ := New(store, ks, meter.NewMemory(), nil, nil, meter.NewPricerHolder(pricer), up.URL, "https://api.anthropic.com")
	rein := httptest.NewServer(p)
	b.Cleanup(rein.Close)

	payload := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"b"}]}`)
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 500,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", rein.URL+"/v1/chat/completions", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 503 {
				b.Fatalf("status %d", resp.StatusCode)
			}
		}
	})
}

// benchSetupWithMeter is benchSetup with an injectable meter.Meter, so a
// benchmark can measure the full hot path with the durable SQLite meter
// instead of meter.Memory.
func benchSetupWithMeter(b *testing.B, useSQLite, withBudget bool, upstream *httptest.Server, m meter.Meter) (string, string) {
	b.Helper()
	store := buildStore(b, useSQLite)
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "bench-durable",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-fake",
		CreatedAt: time.Now().UTC(),
	}
	if withBudget {
		vk.DailyBudgetUSD = 1_000_000
		vk.MonthBudgetUSD = 1_000_000
	}
	if err := store.Create(context.Background(), vk); err != nil {
		b.Fatal(err)
	}
	pricer, err := meter.LoadPricer()
	if err != nil {
		b.Fatal(err)
	}
	p, err := New(store, killswitch.NewMemory(), m, nil, nil, meter.NewPricerHolder(pricer), upstream.URL, "https://api.anthropic.com")
	if err != nil {
		b.Fatal(err)
	}
	rein := httptest.NewServer(p)
	b.Cleanup(rein.Close)
	return rein.URL, token
}

// Full production hot path with the durable SQLite meter: SQLite keystore +
// AES-256-GCM decrypt + SQLite-backed Check + transactional Record per
// request.
//
// Baseline measured on Apple M5 (macOS 15, APFS, -cpu=4): ~72 us/op total,
// versus ~34 us/op for the in-memory meter. The ~38 us/op incremental cost
// comes from two SQLite round trips per request (Check SELECT + Record
// transaction with WAL append) plus SetMaxOpenConns(1)
// serialization at the Go pool. On Linux NVMe with cheaper fdatasync,
// expect the incremental cost to fall closer to the 15-30 us/op range.
func BenchmarkRein_SQLite_WithDurableMeter_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)

	m, err := meter.NewSQLite(filepath.Join(b.TempDir(), "meter.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = m.Close() })

	url, tok := benchSetupWithMeter(b, true, true, up, m)
	drive200(b, url, tok)
}

// benchSetupWithRates variant mints a key with rate limits set.
func benchSetupWithRates(b *testing.B, useSQLite bool, upstream *httptest.Server, rpsLimit, rpmLimit int) (string, string) {
	b.Helper()
	store := buildStore(b, useSQLite)
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "bench-rl",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-fake",
		DailyBudgetUSD: 1_000_000, MonthBudgetUSD: 1_000_000,
		RPSLimit: rpsLimit, RPMLimit: rpmLimit,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		b.Fatal(err)
	}
	pricer, err := meter.LoadPricer()
	if err != nil {
		b.Fatal(err)
	}
	rl := rates.NewMemory()
	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), rl, nil, meter.NewPricerHolder(pricer), upstream.URL, "https://api.anthropic.com")
	if err != nil {
		b.Fatal(err)
	}
	rein := httptest.NewServer(p)
	b.Cleanup(rein.Close)
	return rein.URL, token
}

// Full hot path with rate limiting enabled, high limits so no rejection.
func BenchmarkRein_SQLite_WithBudget_RateLimited_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetupWithRates(b, true, up, 1_000_000, 60_000_000)
	drive200(b, url, tok)
}

// In-memory keystore with rate limiting, isolates rate limiter cost.
func BenchmarkRein_MemStore_WithBudget_RateLimited_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetupWithRates(b, false, up, 1_000_000, 60_000_000)
	drive200(b, url, tok)
}

// benchSetupWithConcurrency mints a key with MaxConcurrent set, wires the
// proxy with a concurrency.Memory store, and returns the test rein URL +
// token. driveParallel is the caller's goroutine count for -cpu sweeps.
func benchSetupWithConcurrency(b *testing.B, useSQLite bool, upstream *httptest.Server, maxConcurrent int) (string, string) {
	b.Helper()
	store := buildStore(b, useSQLite)
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "bench-cc",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-fake",
		DailyBudgetUSD: 1_000_000, MonthBudgetUSD: 1_000_000,
		MaxConcurrent: maxConcurrent,
		CreatedAt:     time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		b.Fatal(err)
	}
	pricer, err := meter.LoadPricer()
	if err != nil {
		b.Fatal(err)
	}
	cs := concurrency.NewMemory()
	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), nil, cs, meter.NewPricerHolder(pricer), upstream.URL, "https://api.anthropic.com")
	if err != nil {
		b.Fatal(err)
	}
	rein := httptest.NewServer(p)
	b.Cleanup(rein.Close)
	return rein.URL, token
}

// BenchmarkRein_SQLite_WithBudget_ConcurrencyUnlimited_ZeroLatency: a key
// with MaxConcurrent=0 must pay zero hot-path cost compared to
// BenchmarkRein_SQLite_WithBudget_ZeroLatency. The concurrency.Memory
// store's Acquire short-circuits on limit==0 without touching the
// sync.Map.
func BenchmarkRein_SQLite_WithBudget_ConcurrencyUnlimited_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetupWithConcurrency(b, true, up, 0)
	drive200(b, url, tok)
}

// BenchmarkRein_SQLite_WithBudget_ConcurrencyLimited_ZeroLatency: a key
// with a high concurrency cap (no rejection) measures the per-request
// overhead of one sync.Map lookup, one atomic Add(1), and the deferred
// Release. The acceptance bar is under 1 µs of additional overhead vs
// BenchmarkRein_SQLite_WithBudget_ZeroLatency.
func BenchmarkRein_SQLite_WithBudget_ConcurrencyLimited_ZeroLatency(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)
	url, tok := benchSetupWithConcurrency(b, true, up, 1_000_000)
	drive200(b, url, tok)
}

// BenchmarkRein_ConcurrencyMultiKeyParallel hammers 100 distinct keys
// concurrently, each with its own MaxConcurrent cap. This is the
// false-sharing test: without paddedCounter, the per-request overhead
// degrades 2-5x as -cpu rises because per-key counters share a cache
// line. With padding, the line should stay roughly flat across
// -cpu=1,2,4,8,16 sweeps.
//
// Run with:
//
//	go test ./internal/proxy -bench BenchmarkRein_ConcurrencyMultiKeyParallel \
//	       -benchtime=3s -cpu=1,2,4,8,16
func BenchmarkRein_ConcurrencyMultiKeyParallel(b *testing.B) {
	up := mockUpstream()
	b.Cleanup(up.Close)

	store := buildStore(b, true)
	const numKeys = 100
	tokens := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		id, _ := keys.GenerateID()
		token, _ := keys.GenerateToken()
		vk := &keys.VirtualKey{
			ID: id, Token: token, Name: "bench-multi",
			Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-fake",
			DailyBudgetUSD: 1_000_000, MonthBudgetUSD: 1_000_000,
			MaxConcurrent: 50,
			CreatedAt:     time.Now().UTC(),
		}
		if err := store.Create(context.Background(), vk); err != nil {
			b.Fatal(err)
		}
		tokens[i] = token
	}
	pricer, err := meter.LoadPricer()
	if err != nil {
		b.Fatal(err)
	}
	cs := concurrency.NewMemory()
	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), nil, cs, meter.NewPricerHolder(pricer), up.URL, "https://api.anthropic.com")
	if err != nil {
		b.Fatal(err)
	}
	rein := httptest.NewServer(p)
	b.Cleanup(rein.Close)

	payload := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"b"}]}`)
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 500,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			tok := tokens[i%numKeys]
			i++
			req, _ := http.NewRequest("POST", rein.URL+"/v1/chat/completions", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				b.Fatalf("status %d", resp.StatusCode)
			}
		}
	})
}
