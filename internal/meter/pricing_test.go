package meter

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLoadPricer_EmbeddedTableParses(t *testing.T) {
	p, err := LoadPricer()
	if err != nil {
		t.Fatalf("LoadPricer: %v", err)
	}
	if p == nil || len(p.models) == 0 {
		t.Fatal("expected non-empty pricing table")
	}
}

func TestPricer_OpenAICost(t *testing.T) {
	p, err := LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	// 1M input + 1M output on gpt-4o = $2.50 + $10.00 = $12.50
	cost, ok := p.Cost("openai", "gpt-4o", 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("gpt-4o not found")
	}
	if !almost(cost, 12.50) {
		t.Errorf("gpt-4o 1M+1M: got $%.4f want $12.50", cost)
	}
}

func TestPricer_AnthropicCost(t *testing.T) {
	p, _ := LoadPricer()
	// 1M input + 1M output on claude-sonnet-4-5 = $3 + $15 = $18
	cost, ok := p.Cost("anthropic", "claude-sonnet-4-5", 1_000_000, 1_000_000)
	if !ok || !almost(cost, 18.00) {
		t.Errorf("sonnet-4-5 1M+1M: got $%.4f ok=%v want $18.00", cost, ok)
	}
}

func TestPricer_DateSuffixStripping(t *testing.T) {
	p, _ := LoadPricer()
	cases := []struct {
		upstream, model string
		wantCost        float64
	}{
		{"anthropic", "claude-opus-4-5-20251101", 30.00}, // $5 + $25
		{"anthropic", "claude-haiku-4-5-20251001", 6.00}, // $1 + $5
		{"openai", "gpt-4o-2024-08-06", 12.50},           // $2.50 + $10
	}
	for _, tc := range cases {
		got, ok := p.Cost(tc.upstream, tc.model, 1_000_000, 1_000_000)
		if !ok {
			t.Errorf("%s: not found", tc.model)
			continue
		}
		if !almost(got, tc.wantCost) {
			t.Errorf("%s: got $%.4f want $%.4f", tc.model, got, tc.wantCost)
		}
	}
}

func TestPricer_UnknownModel(t *testing.T) {
	p, _ := LoadPricer()
	if _, ok := p.Cost("openai", "gpt-99-ultra", 100, 100); ok {
		t.Error("unknown model should return ok=false")
	}
	if _, ok := p.Cost("cohere", "command-r", 100, 100); ok {
		t.Error("unknown upstream should return ok=false")
	}
}

func TestPricer_ZeroTokens(t *testing.T) {
	p, _ := LoadPricer()
	cost, ok := p.Cost("openai", "gpt-4o", 0, 0)
	if !ok || cost != 0 {
		t.Errorf("zero tokens: got %v ok=%v want 0 true", cost, ok)
	}
}

// TestPricer_CurrentGenerationModels covers the current-generation model IDs
// from the OpenAI and Anthropic pricing pages that are new in this version of
// the table. Failures here usually mean a vendor price changed and the table
// needs to be refreshed against the primary source.
func TestPricer_CurrentGenerationModels(t *testing.T) {
	p, _ := LoadPricer()
	cases := []struct {
		upstream, model string
		wantCost        float64
	}{
		// OpenAI current generation (gpt-5.4 family).
		{"openai", "gpt-5.4", 17.50},             // $2.50 + $15.00
		{"openai", "gpt-5.4-mini", 5.25},         // $0.75 + $4.50
		{"openai", "gpt-5.4-nano", 1.45},         // $0.20 + $1.25
		{"openai", "gpt-5.4-pro", 210.00},        // $30.00 + $180.00
		{"openai", "gpt-5.3-chat-latest", 15.75}, // $1.75 + $14.00
		{"openai", "gpt-5.3-codex", 15.75},       // $1.75 + $14.00
		// Anthropic deprecated-but-callable models.
		{"anthropic", "claude-sonnet-3-7", 18.00}, // $3 + $15
		{"anthropic", "claude-opus-3", 90.00},     // $15 + $75
	}
	for _, tc := range cases {
		got, ok := p.Cost(tc.upstream, tc.model, 1_000_000, 1_000_000)
		if !ok {
			t.Errorf("%s: not found in pricing table", tc.model)
			continue
		}
		if !almost(got, tc.wantCost) {
			t.Errorf("%s: got $%.4f want $%.4f", tc.model, got, tc.wantCost)
		}
	}
}

// TestPricer_DateStrippingForOpenAIDatedSnapshots verifies that dated OpenAI
// snapshots like o4-mini-2025-04-16 resolve to their base entry via the
// pricer's trailing-date regex.
func TestPricer_DateStrippingForOpenAIDatedSnapshots(t *testing.T) {
	p, _ := LoadPricer()
	cases := []struct {
		model    string
		wantCost float64
	}{
		{"o4-mini-2025-04-16", 5.50},          // base o4-mini: $1.10 + $4.40
		{"gpt-5.4-mini-2026-03-01", 5.25},     // base gpt-5.4-mini: $0.75 + $4.50
		{"claude-sonnet-4-6-20260301", 18.00}, // base claude-sonnet-4-6: $3 + $15
	}
	for _, tc := range cases {
		upstream := "openai"
		if len(tc.model) > 7 && tc.model[:7] == "claude-" {
			upstream = "anthropic"
		}
		got, ok := p.Cost(upstream, tc.model, 1_000_000, 1_000_000)
		if !ok {
			t.Errorf("%s: not resolved via date-stripping", tc.model)
			continue
		}
		if !almost(got, tc.wantCost) {
			t.Errorf("%s: got $%.4f want $%.4f", tc.model, got, tc.wantCost)
		}
	}
}
