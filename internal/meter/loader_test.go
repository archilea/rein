package meter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// embeddedForTests loads the real embedded pricing table so every loader
// test has the same base as the production binary. Catches the "my test
// base is empty and the merge path is accidentally no-op" class of bug.
func embeddedForTests(t *testing.T) *Pricer {
	t.Helper()
	base, err := LoadPricer()
	if err != nil {
		t.Fatalf("LoadPricer: %v", err)
	}
	return base
}

// writeTempConfig writes a JSON payload to a temp file and returns its
// path. The file is cleaned up by t.TempDir.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigFile_EmptyPathReturnsBase(t *testing.T) {
	// Zero-config default: if REIN_CONFIG_FILE is unset, LoadConfigFile
	// returns the base pricer unchanged. This is the bit-for-bit
	// identical path to pre-#25 behavior.
	base := embeddedForTests(t)
	got, err := LoadConfigFile("", base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != base {
		t.Errorf("empty path should return base unchanged; got a new pricer")
	}
}

func TestLoadConfigFile_MissingFileErrors(t *testing.T) {
	_, err := LoadConfigFile("/nonexistent/rein.json", embeddedForTests(t))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("error should mention read: %v", err)
	}
}

func TestLoadConfigFile_InvalidJSONRejected(t *testing.T) {
	path := writeTempConfig(t, "not valid json {")
	_, err := LoadConfigFile(path, embeddedForTests(t))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse: %v", err)
	}
}

func TestLoadConfigFile_NegativePriceRejected(t *testing.T) {
	path := writeTempConfig(t, `{
		"version": "1",
		"models": {
			"openai": {
				"bad-model": {"input_per_mtok": -1.0, "output_per_mtok": 2.0}
			}
		}
	}`)
	_, err := LoadConfigFile(path, embeddedForTests(t))
	if err == nil {
		t.Fatal("expected error for negative price")
	}
	if !strings.Contains(err.Error(), "bad-model") || !strings.Contains(err.Error(), ">= 0") {
		t.Errorf("error should name the bad entry and the rule: %v", err)
	}
}

func TestLoadConfigFile_NegativeOutputPriceRejected(t *testing.T) {
	path := writeTempConfig(t, `{
		"version": "1",
		"models": {
			"openai": {
				"bad-model": {"input_per_mtok": 1.0, "output_per_mtok": -2.0}
			}
		}
	}`)
	if _, err := LoadConfigFile(path, embeddedForTests(t)); err == nil {
		t.Fatal("expected error for negative output price")
	}
}

func TestLoadConfigFile_ZeroPricesAllowed(t *testing.T) {
	// Zero prices are valid (free tiers, local-hosted models).
	path := writeTempConfig(t, `{
		"version": "1",
		"models": {
			"openai": {
				"local-llama": {"input_per_mtok": 0, "output_per_mtok": 0}
			}
		}
	}`)
	p, err := LoadConfigFile(path, embeddedForTests(t))
	if err != nil {
		t.Fatalf("zero prices should be allowed: %v", err)
	}
	cost, ok := p.Cost("openai", "local-llama", 1_000_000, 1_000_000)
	if !ok {
		t.Error("zero-priced entry should be resolvable")
	}
	if cost != 0 {
		t.Errorf("zero-priced entry cost: got %v want 0", cost)
	}
}

func TestLoadConfigFile_UnknownVersionIsTypedError(t *testing.T) {
	path := writeTempConfig(t, `{
		"version": "999",
		"models": {"openai": {}}
	}`)
	_, err := LoadConfigFile(path, embeddedForTests(t))
	if err == nil {
		t.Fatal("expected error for unknown version")
	}
	if !IsUnknownConfigVersion(err) {
		t.Errorf("error should be *ErrUnknownConfigVersion, got %T: %v", err, err)
	}
	var vErr *ErrUnknownConfigVersion
	if !errors.As(err, &vErr) {
		t.Fatalf("errors.As *ErrUnknownConfigVersion failed: %v", err)
	}
	if vErr.Found != "999" || vErr.Expected != ConfigSchemaVersion {
		t.Errorf("found=%q expected=%q; want found=999 expected=%s",
			vErr.Found, vErr.Expected, ConfigSchemaVersion)
	}
}

func TestLoadConfigFile_EmptyVersionDefaultsToOne(t *testing.T) {
	// An operator pasting a pricing.json-shaped file verbatim should not
	// have to add a version field. Empty version is tolerated and maps
	// to ConfigSchemaVersion. Non-empty and wrong is a typed error.
	path := writeTempConfig(t, `{
		"models": {
			"openai": {
				"gpt-foo": {"input_per_mtok": 1, "output_per_mtok": 2}
			}
		}
	}`)
	p, err := LoadConfigFile(path, embeddedForTests(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.Cost("openai", "gpt-foo", 1_000_000, 1_000_000); !ok {
		t.Error("override entry should be resolvable after empty-version load")
	}
}

func TestLoadConfigFile_OverrideWinsForExistingPair(t *testing.T) {
	base := embeddedForTests(t)
	// Pick a real embedded entry to override. gpt-4o is guaranteed to be
	// in pricing.json per the 0.1.1 shipped list.
	originalCost, ok := base.Cost("openai", "gpt-4o", 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("test assumes gpt-4o is in the embedded table")
	}
	path := writeTempConfig(t, `{
		"version": "1",
		"models": {
			"openai": {
				"gpt-4o": {"input_per_mtok": 99.0, "output_per_mtok": 99.0}
			}
		}
	}`)
	merged, err := LoadConfigFile(path, base)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	overrideCost, ok := merged.Cost("openai", "gpt-4o", 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("override should still resolve")
	}
	if overrideCost == originalCost {
		t.Errorf("override cost should differ from base: got %v", overrideCost)
	}
	expected := 99.0 + 99.0
	if overrideCost != expected {
		t.Errorf("override cost: got %v want %v", overrideCost, expected)
	}
}

func TestLoadConfigFile_AddsNewPairWithoutDisturbingBase(t *testing.T) {
	base := embeddedForTests(t)
	baseLen := base.Len()
	path := writeTempConfig(t, `{
		"version": "1",
		"models": {
			"openai": {
				"llama-3.3-70b-versatile": {"input_per_mtok": 0.59, "output_per_mtok": 0.79}
			}
		}
	}`)
	merged, err := LoadConfigFile(path, base)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if merged.Len() != baseLen+1 {
		t.Errorf("merged Len: got %d want %d (base + 1)", merged.Len(), baseLen+1)
	}
	// Original base must NOT be mutated — the merged pricer is a fresh
	// copy. This is the contract PricerHolder.Swap relies on for the
	// lock-free publication semantics.
	if base.Len() != baseLen {
		t.Errorf("base pricer was mutated during merge: was %d, now %d", baseLen, base.Len())
	}
	if _, ok := base.Cost("openai", "llama-3.3-70b-versatile", 1, 1); ok {
		t.Error("base pricer leaked the override entry")
	}
	if _, ok := merged.Cost("openai", "llama-3.3-70b-versatile", 1, 1); !ok {
		t.Error("merged pricer should contain the new entry")
	}
}

func TestIsUnknownConfigVersion_NilIsFalse(t *testing.T) {
	// Trivial but pins the contract: IsUnknownConfigVersion on a nil
	// error must return false so callers can use it in an `if err != nil
	// && IsUnknownConfigVersion(err)` chain without a secondary nil check.
	if IsUnknownConfigVersion(nil) {
		t.Errorf("IsUnknownConfigVersion(nil): got true want false")
	}
}

func TestIsUnknownConfigVersion_UnrelatedErrorIsFalse(t *testing.T) {
	// A random error that happens not to be *ErrUnknownConfigVersion
	// must return false. If someone accidentally broadens the type
	// assertion to a value comparison on the Error() string, this
	// catches it.
	unrelated := os.ErrNotExist
	if IsUnknownConfigVersion(unrelated) {
		t.Errorf("IsUnknownConfigVersion(os.ErrNotExist): got true want false")
	}
}

func TestErrUnknownConfigVersion_ErrorString(t *testing.T) {
	// Pin the error message format so a future refactor that accidentally
	// renames fields or changes the message does not silently break log
	// parsers that grep on it.
	e := &ErrUnknownConfigVersion{Found: "999", Expected: "1"}
	want := `unknown config file version "999" (expected "1")`
	if got := e.Error(); got != want {
		t.Errorf("Error(): got %q want %q", got, want)
	}
}

func TestPricer_Len(t *testing.T) {
	// Len must count every (upstream, model) pair across all upstreams.
	p := &Pricer{models: map[string]map[string]ModelPrice{
		"openai":    {"gpt-4o": {}, "gpt-4o-mini": {}, "gpt-5": {}},
		"anthropic": {"claude-opus-4-6": {}, "claude-sonnet-4-6": {}},
	}}
	if got := p.Len(); got != 5 {
		t.Errorf("Len: got %d want 5", got)
	}

	// Empty pricer reports zero.
	empty := &Pricer{models: map[string]map[string]ModelPrice{}}
	if got := empty.Len(); got != 0 {
		t.Errorf("empty Len: got %d want 0", got)
	}

	// Upstream with no models is counted as zero (empty inner map).
	zero := &Pricer{models: map[string]map[string]ModelPrice{
		"openai": {},
	}}
	if got := zero.Len(); got != 0 {
		t.Errorf("upstream-with-no-models Len: got %d want 0", got)
	}
}

func TestLoadConfigFile_EmptyModelsIsValid(t *testing.T) {
	// An empty models block is a no-op override. Useful for validating
	// the file without actually overriding anything.
	path := writeTempConfig(t, `{"version": "1", "models": {}}`)
	_, err := LoadConfigFile(path, embeddedForTests(t))
	if err != nil {
		t.Errorf("empty models should be allowed: %v", err)
	}
}
