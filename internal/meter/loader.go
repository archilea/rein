package meter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// ConfigSchemaVersion is the single supported version of the operator
// config file format. Files carrying a different value are rejected at
// startup (fatal) and logged-with-fallback on SIGHUP reload (see #25 Q6).
// Bumping this is a breaking change; readers on the old version warn
// and keep the previous snapshot.
const ConfigSchemaVersion = "1"

// configFile mirrors the rein.json schema. Top-level siblings of Models
// are reserved for future 0.3+ features that need operator-editable,
// non-secret, non-resource settings. Adding a new optional field is a
// compatible change and does not bump ConfigSchemaVersion; removing or
// renaming a field is a breaking change and does.
type configFile struct {
	Version   string                           `json:"version"`
	Source    string                           `json:"source,omitempty"`
	FetchedAt string                           `json:"fetched_at,omitempty"`
	Models    map[string]map[string]ModelPrice `json:"models"`
}

// ErrUnknownConfigVersion is returned when the config file's version
// field is non-empty and does not match ConfigSchemaVersion. Callers
// decide whether to treat it as fatal (startup) or as a warn-and-keep
// (reload) per issue #25 Q6.
type ErrUnknownConfigVersion struct {
	Found    string
	Expected string
}

func (e *ErrUnknownConfigVersion) Error() string {
	return fmt.Sprintf("unknown config file version %q (expected %q)", e.Found, e.Expected)
}

// IsUnknownConfigVersion reports whether err is an *ErrUnknownConfigVersion.
// Exposed so cmd/rein can branch on this specific failure mode to apply the
// asymmetric startup-vs-reload policy without comparing strings.
func IsUnknownConfigVersion(err error) bool {
	_, ok := err.(*ErrUnknownConfigVersion)
	return ok
}

// LoadConfigFile reads an operator-editable pricing overrides file at
// path, validates it, and merges its entries on top of base (typically
// the embedded pricing table from LoadPricer). It returns a brand-new
// *Pricer that callers can Swap into a PricerHolder atomically.
//
// Validation is strict all-or-nothing (#25 Q4): a single bad entry
// rejects the whole file and the caller keeps the previous snapshot.
// Zero prices are ALLOWED (free tiers, local-hosted models) but log an
// INFO line so operators know what they shipped. Negative prices are a
// typo and reject the file.
//
// If path is empty, LoadConfigFile returns base unchanged — the holder
// then wraps just the embedded table, which is the zero-config default
// behavior.
func LoadConfigFile(path string, base *Pricer) (*Pricer, error) {
	if strings.TrimSpace(path) == "" {
		return base, nil
	}
	if base == nil {
		return nil, fmt.Errorf("LoadConfigFile: base pricer is required")
	}
	raw, err := os.ReadFile(path) // #nosec G304 — path is an operator env var, documented
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var file configFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Version check. Empty version is tolerated (defaults to "1") so
	// operators can paste a pricing.json-shaped file verbatim without
	// adding boilerplate. Any other non-empty value that is not "1" is
	// a hard error (startup: fatal, reload: warn + keep old).
	if file.Version == "" {
		file.Version = ConfigSchemaVersion
	}
	if file.Version != ConfigSchemaVersion {
		return nil, &ErrUnknownConfigVersion{
			Found:    file.Version,
			Expected: ConfigSchemaVersion,
		}
	}
	// Validate every model price before touching the merged table. A
	// single negative value rejects the whole file; this is the strict
	// all-or-nothing policy from #25 Q4.
	zeroCount := 0
	for upstream, table := range file.Models {
		for model, price := range table {
			if price.InputPerMToken < 0 || price.OutputPerMToken < 0 {
				return nil, fmt.Errorf(
					"invalid entry %s.%s: input_per_mtok=%g output_per_mtok=%g; both must be >= 0",
					upstream, model, price.InputPerMToken, price.OutputPerMToken,
				)
			}
			if price.InputPerMToken == 0 && price.OutputPerMToken == 0 {
				slog.Info("pricer override: zero-priced entry",
					"upstream", upstream, "model", model,
					"note", "allowed (free tier), logged for visibility")
				zeroCount++
			}
		}
	}
	// Merge. The base pricer is the embedded table; override entries
	// replace embedded entries for the same (upstream, model) pair, and
	// new (upstream, model) pairs are added. Build a brand-new models
	// map so the returned Pricer is immutable once constructed — that
	// is the invariant the PricerHolder.Swap contract relies on.
	merged := make(map[string]map[string]ModelPrice, len(base.models))
	for upstream, table := range base.models {
		dup := make(map[string]ModelPrice, len(table))
		for model, price := range table {
			dup[model] = price
		}
		merged[upstream] = dup
	}
	overrideCount := 0
	addCount := 0
	for upstream, table := range file.Models {
		if _, ok := merged[upstream]; !ok {
			merged[upstream] = make(map[string]ModelPrice, len(table))
		}
		for model, price := range table {
			if _, existed := merged[upstream][model]; existed {
				overrideCount++
			} else {
				addCount++
			}
			merged[upstream][model] = price
		}
	}
	slog.Info("pricer override: loaded",
		"path", path,
		"overrides_applied", overrideCount,
		"entries_added", addCount,
		"zero_priced_entries", zeroCount,
		"total_models", countModels(merged),
	)
	return &Pricer{models: merged}, nil
}

// countModels returns the total number of (upstream, model) pairs in a
// models map. Kept local to the loader so it does not become part of
// the public Pricer API.
func countModels(models map[string]map[string]ModelPrice) int {
	n := 0
	for _, table := range models {
		n += len(table)
	}
	return n
}

// TryReload attempts to re-read path, merge on top of base, and
// atomically swap the result into holder. On success it logs INFO
// "config reload succeeded" and returns true. On failure it logs
// either WARN (for ErrUnknownConfigVersion, matching the asymmetric
// Q6 policy where unknown versions at reload must never crash a
// running process) or ERROR (for every other failure), keeps the
// previous holder snapshot active, and returns false.
//
// Callers are the SIGHUP goroutine and the optional background poll
// goroutine in cmd/rein/main.go. Both call TryReload through a
// single helper so their success and failure behavior is identical
// by construction — if a test proves TryReload does the right
// thing, both triggers inherit that guarantee without needing
// separate integration coverage for each.
//
// The active_snapshot_models field on the error log line is the
// single most useful piece of operator-facing information when
// triaging a bad reload: it tells the operator exactly how many
// models are currently serving traffic, so they can confirm the
// old config is still active before investigating.
//
// ctx is used only to thread through to slog.Log so the call can
// be cancelled if the caller is mid-shutdown. LoadConfigFile itself
// does not take a context today because its work is stdlib-bounded
// and blocks for at most the duration of os.ReadFile on a small
// JSON blob.
func TryReload(
	ctx context.Context,
	logger *slog.Logger,
	trigger, path string,
	base *Pricer,
	holder *PricerHolder,
) bool {
	next, err := LoadConfigFile(path, base)
	if err != nil {
		activeCount := 0
		if active := holder.Load(); active != nil {
			activeCount = active.Len()
		}
		level := slog.LevelError
		if IsUnknownConfigVersion(err) {
			level = slog.LevelWarn
		}
		logger.Log(ctx, level,
			"config reload failed, keeping previous snapshot active",
			"trigger", trigger,
			"path", path,
			"err", err.Error(),
			"active_snapshot_models", activeCount,
		)
		return false
	}
	holder.Swap(next)
	logger.Info("config reload succeeded",
		"trigger", trigger,
		"path", path,
		"models", next.Len(),
	)
	return true
}
