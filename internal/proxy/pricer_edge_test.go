package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
)

// TestOpenAI_SwapNilPricerIsSafe pins the defense-in-depth guard I
// added in recordSpend for #25: if a PricerHolder holds nil (only
// possible via Swap(nil) from an operator-controlled code path, never
// from normal config reload), the adapter must NOT panic or dereference
// the nil. Instead it silently skips the cost computation as if the
// adapter had no pricer at all.
//
// This is a contract guarantee: a future refactor that removes the
// nil-check for "simplification" will flip this test from PASS to
// panic, which is the loud failure we want.
func TestOpenAI_SwapNilPricerIsSafe(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer upstream.Close()

	pricer, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	holder := meter.NewPricerHolder(pricer)

	store := keys.NewMemory()
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "swap-nil",
		Upstream: keys.UpstreamOpenAI, UpstreamKey: "sk-fake",
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}

	p, err := New(store, killswitch.NewMemory(), meter.NewMemory(), nil, nil, holder,
		upstream.URL, "https://api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	// Before request: swap nil into the holder.
	holder.Swap(nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	// If the nil-check is missing, the response-path recordSpend will
	// dereference a nil Pricer inside modifyResponse and panic. The
	// deferred recover captures any panic so the test fails loudly.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("adapter panicked on Swap(nil) pricer: %v", r)
		}
	}()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, body)
	}
}

// TestOpenAI_DateStrippingFallbackAppliesToOverrideEntries proves that
// the date-stripping fallback at internal/meter/pricing.go works
// transparently against an override-added entry, not just the embedded
// table. The contract: if an operator adds pricing for the undated
// name "claude-opus-4-5" and the upstream returns the dated snapshot
// "claude-opus-4-5-20251101", the pricer's Cost call still resolves
// (via the dateSuffix regex strip) and the meter records spend.
//
// This is the "Pricer.Cost works the same on merged tables as it does
// on the embedded table" guarantee, which the loader's merge semantics
// rely on but no other test exercises against an override-only entry.
func TestOpenAI_DateStrippingFallbackAppliesToOverrideEntries(t *testing.T) {
	base, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}

	// Build a merged pricer by directly constructing it the way
	// LoadConfigFile would — an override entry for "llama-3.3-70b"
	// (undated). We then call Cost with a dated model name that
	// uses the same base, and expect the regex-strip to find the
	// override entry.
	//
	// We cannot call LoadConfigFile here because it reads from a
	// file, and the point of this test is to isolate the Cost()
	// date-strip behavior against override-constructed entries.
	// Instead we build the merged map by hand the same way the
	// loader does.
	dir := t.TempDir()
	loaderPath := filepath.Join(dir, "rein.json")
	if err := os.WriteFile(loaderPath, []byte(`{
		"version": "1",
		"models": {
			"openai": {
				"llama-3.3-70b": {"input_per_mtok": 0.50, "output_per_mtok": 0.80}
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	merged, err := meter.LoadConfigFile(loaderPath, base)
	if err != nil {
		t.Fatal(err)
	}

	// Dated variant of the override entry. The dateSuffix regex
	// strips "-20251101" and "-2025-11-01" suffixes, so these should
	// resolve to the undated override entry.
	cases := []struct {
		name         string
		model        string
		wantResolved bool
	}{
		{"exact match", "llama-3.3-70b", true},
		{"dated 20251101", "llama-3.3-70b-20251101", true},
		{"dated ISO", "llama-3.3-70b-2025-11-01", true},
		{"completely unrelated model", "llama-4.0-nope", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := merged.Cost("openai", tc.model, 1_000_000, 1_000_000)
			if ok != tc.wantResolved {
				t.Errorf("Cost(%q): resolved=%v want %v", tc.model, ok, tc.wantResolved)
			}
		})
	}
}
