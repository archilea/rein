package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/archilea/rein/internal/keys"
)

// TestOpenAI_PerKeyBaseURLOverride verifies the hot-path override: one
// virtual key points at the default upstream, a second virtual key points
// at a second (distinct) upstream via UpstreamBaseURL, and both resolve to
// the correct host. This is the core contract of #24.
func TestOpenAI_PerKeyBaseURLOverride(t *testing.T) {
	var (
		defaultHits int64
		overrideHits int64
	)
	defaultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&defaultHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer defaultUpstream.Close()

	overrideUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&overrideHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9}}`))
	}))
	defer overrideUpstream.Close()

	store := keys.NewMemory()
	defaultKey := mustCreateKey(t, store, "default", keys.UpstreamOpenAI, "sk-default", "")
	overrideKey := mustCreateKey(t, store, "override", keys.UpstreamOpenAI, "sk-override", overrideUpstream.URL)

	p := newTestProxy(t, store, defaultUpstream.URL, "https://api.anthropic.com")

	// Request 1: default key → default upstream
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req1.Header.Set("Authorization", "Bearer "+defaultKey.Token)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		body, _ := io.ReadAll(rec1.Body)
		t.Fatalf("default: status %d body %s", rec1.Code, body)
	}

	// Request 2: override key → override upstream
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req2.Header.Set("Authorization", "Bearer "+overrideKey.Token)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		body, _ := io.ReadAll(rec2.Body)
		t.Fatalf("override: status %d body %s", rec2.Code, body)
	}

	if atomic.LoadInt64(&defaultHits) != 1 {
		t.Errorf("default upstream hits: got %d want 1", defaultHits)
	}
	if atomic.LoadInt64(&overrideHits) != 1 {
		t.Errorf("override upstream hits: got %d want 1", overrideHits)
	}
}

// TestOpenAI_PerKeyBaseURLOverride_ConcurrentDistinctKeys drives both keys
// simultaneously from many goroutines so the sync.Map cache is exercised
// under contention and we catch any cross-key target-URL bleeding.
func TestOpenAI_PerKeyBaseURLOverride_ConcurrentDistinctKeys(t *testing.T) {
	var (
		defaultHits  int64
		overrideHits int64
	)
	defaultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&defaultHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer defaultUpstream.Close()
	overrideUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&overrideHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer overrideUpstream.Close()

	store := keys.NewMemory()
	defaultKey := mustCreateKey(t, store, "default", keys.UpstreamOpenAI, "sk-default", "")
	overrideKey := mustCreateKey(t, store, "override", keys.UpstreamOpenAI, "sk-override", overrideUpstream.URL)

	p := newTestProxy(t, store, defaultUpstream.URL, "https://api.anthropic.com")
	front := httptest.NewServer(p)
	defer front.Close()

	const perKey = 20
	var wg sync.WaitGroup
	fire := func(token string) {
		defer wg.Done()
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodPost, front.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o"}`))
		if err != nil {
			t.Errorf("new req: %v", err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("do: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status: got %d want 200", resp.StatusCode)
		}
	}
	wg.Add(perKey * 2)
	for i := 0; i < perKey; i++ {
		go fire(defaultKey.Token)
		go fire(overrideKey.Token)
	}
	wg.Wait()

	gotDefault := atomic.LoadInt64(&defaultHits)
	if gotDefault != perKey {
		t.Errorf("default upstream hits: got %d want %d", gotDefault, perKey)
	}
	gotOverride := atomic.LoadInt64(&overrideHits)
	if gotOverride != perKey {
		t.Errorf("override upstream hits: got %d want %d", gotOverride, perKey)
	}
}

// TestOpenAI_OverrideAnthropicAdapterNotAffected confirms that the
// per-key override is OpenAI-only; an Anthropic key with a base URL (which
// cannot happen via the admin API but could happen via direct store
// writes in the future) is safely ignored by the Anthropic adapter and
// falls back to the global default.
func TestOpenAI_OverrideAnthropicAdapterNotAffected(t *testing.T) {
	var defaultHits int64
	defaultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&defaultHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-sonnet-4-5","usage":{"input_tokens":1,"output_tokens":2}}`))
	}))
	defer defaultUpstream.Close()

	store := keys.NewMemory()
	// Bypass the admin handler and write directly to the store with a
	// non-empty UpstreamBaseURL on an anthropic key. The proxy must still
	// route to the global Anthropic base.
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID:              id,
		Token:           token,
		Name:            "anthropic-with-url",
		Upstream:        keys.UpstreamAnthropic,
		UpstreamKey:     "sk-ant-real",
		UpstreamBaseURL: "https://api.example.com", // ignored by Anthropic adapter
		CreatedAt:       time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, store, "https://api.openai.com", defaultUpstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status: got %d want 200 body %s", rec.Code, body)
	}
	if atomic.LoadInt64(&defaultHits) != 1 {
		t.Errorf("default anthropic upstream hits: got %d want 1", defaultHits)
	}
}

// TestOpenAI_LookupOverrideURL_Memoization confirms the sync.Map cache
// returns the same pointer on repeat calls, so every request after the
// first hit avoids the url.Parse allocation.
func TestOpenAI_LookupOverrideURL_Memoization(t *testing.T) {
	o, err := NewOpenAI("https://api.openai.com", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := o.lookupOverrideURL("https://api.groq.com")
	b := o.lookupOverrideURL("https://api.groq.com")
	if a == nil || b == nil {
		t.Fatalf("expected non-nil parsed URLs, got %v %v", a, b)
	}
	if a != b {
		t.Errorf("lookupOverrideURL should memoize; got distinct pointers")
	}
}

// TestOpenAI_LookupOverrideURL_MalformedFallsBack exercises the defensive
// branch where a stored upstream_base_url fails url.Parse or yields an empty
// scheme/host. The admin handler validates on create so this cannot happen
// in practice, but a future direct DB edit or a schema-level corruption
// must not crash the proxy — lookupOverrideURL must return nil so rewrite
// falls back to the global default.
func TestOpenAI_LookupOverrideURL_MalformedFallsBack(t *testing.T) {
	o, err := NewOpenAI("https://api.openai.com", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		// url.Parse returns an error on these:
		"http://[::1",       // unterminated IPv6 bracket
		"://no-scheme-host", // malformed
		// url.Parse accepts these but they have no scheme or host:
		"not a url",
		"",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			got := o.lookupOverrideURL(raw)
			if got != nil {
				t.Errorf("lookupOverrideURL(%q): got %v want nil (must fall back)", raw, got)
			}
		})
	}
}

// mustCreateKey is a proxy-package-local test helper that mints a virtual
// key with an optional upstream_base_url. Kept local rather than added to
// newTestKey so the existing helper's signature stays put.
func mustCreateKey(t *testing.T, store *keys.Memory, name, upstream, upstreamKey, baseURL string) *keys.VirtualKey {
	t.Helper()
	id, err := keys.GenerateID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := keys.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	vk := &keys.VirtualKey{
		ID:              id,
		Token:           token,
		Name:            name,
		Upstream:        upstream,
		UpstreamKey:     upstreamKey,
		UpstreamBaseURL: baseURL,
		CreatedAt:       time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return vk
}
