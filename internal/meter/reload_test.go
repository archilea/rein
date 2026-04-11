package meter

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonLogBuffer is a small helper that captures slog output in JSON
// format so tests can assert on both the level and the structured
// fields without string-matching a human-readable format.
type jsonLogBuffer struct {
	buf    bytes.Buffer
	logger *slog.Logger
}

func newJSONLogBuffer() *jsonLogBuffer {
	b := &jsonLogBuffer{}
	b.logger = slog.New(slog.NewJSONHandler(&b.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return b
}

// entries parses each JSONL line in the buffer into a map. Returns one
// map per log line in chronological order.
func (b *jsonLogBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0)
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// findEntry returns the first log entry whose msg field equals want.
// Fails the test if no such entry is present.
func (b *jsonLogBuffer) findEntry(t *testing.T, want string) map[string]any {
	t.Helper()
	for _, e := range b.entries(t) {
		if e["msg"] == want {
			return e
		}
	}
	t.Fatalf("no log entry with msg=%q in %q", want, b.buf.String())
	return nil
}

func writeReloadConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTryReload_SuccessSwapsHolder(t *testing.T) {
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)
	path := writeReloadConfig(t, `{
		"version": "1",
		"models": {
			"openai": {
				"unit-test-model": {"input_per_mtok": 7.5, "output_per_mtok": 9.5}
			}
		}
	}`)
	lb := newJSONLogBuffer()
	ok := TryReload(context.Background(), lb.logger, "sighup", path, base, holder)
	if !ok {
		t.Fatal("TryReload returned false on a valid config")
	}
	// Holder now has the new pricer.
	got := holder.Load()
	if got == nil {
		t.Fatal("holder has nil after successful reload")
	}
	if _, resolved := got.Cost("openai", "unit-test-model", 1, 1); !resolved {
		t.Error("override entry missing from swapped pricer")
	}
	// Success log line is present with the correct fields.
	entry := lb.findEntry(t, "config reload succeeded")
	if entry["trigger"] != "sighup" {
		t.Errorf("trigger: got %v want sighup", entry["trigger"])
	}
	if entry["path"] != path {
		t.Errorf("path: got %v want %s", entry["path"], path)
	}
	// JSON numbers unmarshal as float64.
	if got, ok := entry["models"].(float64); !ok || int(got) != base.Len()+1 {
		t.Errorf("models: got %v want %d", entry["models"], base.Len()+1)
	}
	if entry["level"] != "INFO" {
		t.Errorf("level: got %v want INFO", entry["level"])
	}
}

func TestTryReload_BadJSONLogsErrorKeepsHolder(t *testing.T) {
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)
	path := writeReloadConfig(t, "not valid json {{{")
	lb := newJSONLogBuffer()

	ok := TryReload(context.Background(), lb.logger, "poll", path, base, holder)
	if ok {
		t.Error("TryReload should return false on bad JSON")
	}
	if holder.Load() != base {
		t.Error("holder should still point at the base pricer after a failed reload")
	}
	entry := lb.findEntry(t, "config reload failed, keeping previous snapshot active")
	if entry["level"] != "ERROR" {
		t.Errorf("bad JSON should log at ERROR: got level=%v", entry["level"])
	}
	if entry["trigger"] != "poll" {
		t.Errorf("trigger: got %v want poll", entry["trigger"])
	}
	if count, ok := entry["active_snapshot_models"].(float64); !ok || int(count) != base.Len() {
		t.Errorf("active_snapshot_models: got %v want %d", entry["active_snapshot_models"], base.Len())
	}
}

func TestTryReload_UnknownVersionLogsWarnKeepsHolder(t *testing.T) {
	// This is the asymmetric #25 Q6 policy in action: unknown config
	// version on reload must log at WARN level (not ERROR) and MUST
	// keep the previous snapshot active. Startup handles unknown
	// version differently — it fatal-exits — but that is the caller's
	// policy, not TryReload's.
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)
	path := writeReloadConfig(t, `{
		"version": "999",
		"models": {"openai": {}}
	}`)
	lb := newJSONLogBuffer()

	ok := TryReload(context.Background(), lb.logger, "sighup", path, base, holder)
	if ok {
		t.Error("TryReload should return false on unknown version")
	}
	if holder.Load() != base {
		t.Error("holder should still point at the base pricer after a failed reload")
	}
	entry := lb.findEntry(t, "config reload failed, keeping previous snapshot active")
	if entry["level"] != "WARN" {
		t.Errorf("unknown version should log at WARN (per Q6 policy): got level=%v", entry["level"])
	}
	if errStr, _ := entry["err"].(string); !strings.Contains(errStr, "999") || !strings.Contains(errStr, "expected") {
		t.Errorf("error message should name the unknown version and expected: %q", errStr)
	}
}

func TestTryReload_NegativePriceLogsErrorNotWarn(t *testing.T) {
	// Confirms the WARN policy is specifically keyed to
	// ErrUnknownConfigVersion and not to "any reload failure". A
	// negative price is a plain validation error and must log at
	// ERROR level — it is an operator typo they need to fix loudly.
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)
	path := writeReloadConfig(t, `{
		"version": "1",
		"models": {
			"openai": {
				"bad": {"input_per_mtok": -1.0, "output_per_mtok": 1.0}
			}
		}
	}`)
	lb := newJSONLogBuffer()

	ok := TryReload(context.Background(), lb.logger, "sighup", path, base, holder)
	if ok {
		t.Error("TryReload should return false on negative price")
	}
	entry := lb.findEntry(t, "config reload failed, keeping previous snapshot active")
	if entry["level"] != "ERROR" {
		t.Errorf("negative price should log at ERROR (not the WARN-only unknown-version carve-out): got level=%v", entry["level"])
	}
}

func TestTryReload_MissingFileLogsErrorKeepsHolder(t *testing.T) {
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)
	lb := newJSONLogBuffer()

	ok := TryReload(context.Background(), lb.logger, "sighup", "/tmp/definitely-not-there-rein-reload.json", base, holder)
	if ok {
		t.Error("TryReload should return false on missing file")
	}
	if holder.Load() != base {
		t.Error("holder should be unchanged after missing-file reload")
	}
	entry := lb.findEntry(t, "config reload failed, keeping previous snapshot active")
	if entry["level"] != "ERROR" {
		t.Errorf("missing file should log at ERROR: got level=%v", entry["level"])
	}
}

func TestTryReload_AtomicRenameWriteWorks(t *testing.T) {
	// Editor-safety claim from the RFC and docs: file editors and
	// atomic writers (vim, emacs, atomic-file libraries) write to a
	// temp file then rename it over the original. The inode changes.
	// LoadConfigFile uses os.ReadFile which opens the path each call,
	// so the rename is transparent. This test proves the chain
	// empirically against the real stdlib.
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)

	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	// Initial file via direct write.
	if err := os.WriteFile(path, []byte(`{"version":"1","models":{"openai":{"first":{"input_per_mtok":1,"output_per_mtok":1}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !TryReload(context.Background(), newJSONLogBuffer().logger, "initial", path, base, holder) {
		t.Fatal("initial reload failed")
	}
	if _, ok := holder.Load().Cost("openai", "first", 1, 1); !ok {
		t.Fatal("initial 'first' entry missing after first reload")
	}

	// Simulate the vim-style write: write to a sibling temp file, then
	// atomic rename over the original. The inode changes.
	tmp := filepath.Join(dir, "rein.json.tmp")
	if err := os.WriteFile(tmp, []byte(`{"version":"1","models":{"openai":{"second":{"input_per_mtok":2,"output_per_mtok":2}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	if !TryReload(context.Background(), newJSONLogBuffer().logger, "post-rename", path, base, holder) {
		t.Fatal("post-rename reload failed")
	}
	if _, ok := holder.Load().Cost("openai", "second", 1, 1); !ok {
		t.Error("post-rename 'second' entry missing; editor-safety claim in docs is wrong")
	}
	// And the 'first' entry should be gone (new file replaced the old,
	// not merged with it — the merge happens between file and base,
	// not between successive files).
	if _, ok := holder.Load().Cost("openai", "first", 1, 1); ok {
		t.Error("post-rename 'first' entry still present; reload didn't pick up the rename")
	}
}
