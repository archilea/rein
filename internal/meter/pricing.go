// Package meter tracks token spend and computes cost for upstream LLM calls.
package meter

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
)

//go:embed pricing.json
var pricingData []byte

// ModelPrice holds USD pricing per 1,000,000 tokens for a specific model.
type ModelPrice struct {
	InputPerMToken  float64 `json:"input_per_mtok"`
	OutputPerMToken float64 `json:"output_per_mtok"`
}

// Cost computes the USD cost for the given token usage.
func (p ModelPrice) Cost(inputTokens, outputTokens int) float64 {
	const perMillion = 1_000_000.0
	return (float64(inputTokens)/perMillion)*p.InputPerMToken +
		(float64(outputTokens)/perMillion)*p.OutputPerMToken
}

// Pricer resolves (upstream, model) pairs to USD cost using an embedded
// pricing table. Unknown models return ok=false so callers can log and skip
// rather than block traffic on a missing entry.
type Pricer struct {
	models map[string]map[string]ModelPrice
}

// LoadPricer parses the embedded pricing.json. Called once at startup.
func LoadPricer() (*Pricer, error) {
	var file struct {
		Models map[string]map[string]ModelPrice `json:"models"`
	}
	if err := json.Unmarshal(pricingData, &file); err != nil {
		return nil, fmt.Errorf("parse pricing.json: %w", err)
	}
	if len(file.Models) == 0 {
		return nil, fmt.Errorf("pricing.json contains no models")
	}
	return &Pricer{models: file.Models}, nil
}

// dateSuffix matches a trailing "-YYYYMMDD" or "-YYYY-MM-DD" on a model ID.
// Used to map dated API responses (e.g. "claude-opus-4-5-20251101") back to
// their undated table entry without maintaining an alias per snapshot.
var dateSuffix = regexp.MustCompile(`-\d{8}$|-\d{4}-\d{2}-\d{2}$`)

// Len returns the total number of (upstream, model) pairs in the pricing
// table. Used by the config reload logger so operators can tell at a
// glance whether a reload succeeded and how large the active snapshot is.
func (p *Pricer) Len() int {
	n := 0
	for _, table := range p.models {
		n += len(table)
	}
	return n
}

// Cost returns the USD cost for a response with the given token counts.
// ok is false if the (upstream, model) pair is not in the table.
func (p *Pricer) Cost(upstream, model string, inputTokens, outputTokens int) (float64, bool) {
	table, ok := p.models[upstream]
	if !ok {
		return 0, false
	}
	mp, ok := table[model]
	if !ok {
		base := dateSuffix.ReplaceAllString(model, "")
		if base == model {
			return 0, false
		}
		mp, ok = table[base]
		if !ok {
			return 0, false
		}
	}
	return mp.Cost(inputTokens, outputTokens), true
}
