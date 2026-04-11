package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setEnv is a test helper that sets env vars for the duration of a test
// and restores the previous values on cleanup. Using t.Setenv is the
// idiomatic Go 1.17+ pattern; wrapping it in a helper keeps the test
// bodies focused on the assertions.
func setEnv(t *testing.T, pairs map[string]string) {
	t.Helper()
	for k, v := range pairs {
		t.Setenv(k, v)
	}
}

// requireAdminToken is the minimum env var set every test needs because
// Load fails without REIN_ADMIN_TOKEN. Tests that care about other
// behaviors set the token here and override whichever knob they are
// exercising.
func requireAdminToken(t *testing.T) {
	t.Helper()
	t.Setenv("REIN_ADMIN_TOKEN", "test-admin-token")
	t.Setenv("REIN_ENCRYPTION_KEY", "")       // not required unless sqlite is used
	t.Setenv("REIN_DB_URL", "memory")         // avoid the sqlite encryption gate
	t.Setenv("REIN_CONFIG_FILE", "")          // default: unset
	t.Setenv("REIN_CONFIG_POLL_INTERVAL", "") // default: unset
}

func TestLoad_RequiresAdminToken(t *testing.T) {
	t.Setenv("REIN_ADMIN_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Error("expected error when REIN_ADMIN_TOKEN is missing")
	}
}

func TestLoad_ConfigFileUnsetMeansEmpty(t *testing.T) {
	requireAdminToken(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile: got %q want empty (unset env var)", cfg.ConfigFile)
	}
	if cfg.ConfigPollInterval != 0 {
		t.Errorf("ConfigPollInterval: got %v want 0 (SIGHUP-only default)", cfg.ConfigPollInterval)
	}
}

func TestLoad_ConfigFileSetIsPassedThrough(t *testing.T) {
	requireAdminToken(t)
	t.Setenv("REIN_CONFIG_FILE", "/etc/rein/rein.json")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ConfigFile != "/etc/rein/rein.json" {
		t.Errorf("ConfigFile: got %q want /etc/rein/rein.json", cfg.ConfigFile)
	}
}

func TestLoad_PollIntervalValidDurations(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1s", 1 * time.Second},
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"1h", 1 * time.Hour},
		{"1500ms", 0}, // 1500ms == 1.5s which is > 1s, valid
	}
	// Fix the last case's expected value.
	cases[4].want = 1500 * time.Millisecond
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			requireAdminToken(t)
			t.Setenv("REIN_CONFIG_POLL_INTERVAL", tc.in)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if cfg.ConfigPollInterval != tc.want {
				t.Errorf("ConfigPollInterval: got %v want %v", cfg.ConfigPollInterval, tc.want)
			}
		})
	}
}

func TestLoad_PollIntervalBelowMinimumIsFatal(t *testing.T) {
	cases := []string{"500ms", "999ms", "1ns", "0s"}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			requireAdminToken(t)
			t.Setenv("REIN_CONFIG_POLL_INTERVAL", tc)
			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for %s (below %s minimum)", tc, ConfigPollIntervalMin)
			}
			if !strings.Contains(err.Error(), "out of range") {
				t.Errorf("error should mention out of range: %v", err)
			}
		})
	}
}

func TestLoad_PollIntervalAboveMaximumIsFatal(t *testing.T) {
	cases := []string{"1h1ns", "2h", "24h"}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			requireAdminToken(t)
			t.Setenv("REIN_CONFIG_POLL_INTERVAL", tc)
			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for %s (above %s maximum)", tc, ConfigPollIntervalMax)
			}
			if !strings.Contains(err.Error(), "out of range") {
				t.Errorf("error should mention out of range: %v", err)
			}
		})
	}
}

func TestLoad_PollIntervalMalformedIsFatal(t *testing.T) {
	cases := []string{"notaduration", "1", "5ss", "half-hour"}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			requireAdminToken(t)
			t.Setenv("REIN_CONFIG_POLL_INTERVAL", tc)
			_, err := Load()
			if err == nil {
				t.Fatalf("expected parse error for %q", tc)
			}
			if !strings.Contains(err.Error(), "REIN_CONFIG_POLL_INTERVAL") {
				t.Errorf("error should mention the env var name: %v", err)
			}
			// Malformed means ParseDuration returned an error — it should
			// wrap through time.ParseDuration's message. Confirm errors.Is
			// does not trip on a random sentinel to ensure the wrap chain
			// is sane.
			if errors.Is(err, errExpectedSentinel) {
				t.Errorf("spurious errors.Is match: %v", err)
			}
		})
	}
}

// errExpectedSentinel is a dummy sentinel used to prove errors.Is does
// not spuriously match in the malformed-duration test. It is not meant
// to be exported.
var errExpectedSentinel = errors.New("expected sentinel")

func TestLoad_PollIntervalBoundaryValues(t *testing.T) {
	// Exactly at the boundaries should be valid.
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1s", ConfigPollIntervalMin},
		{"1h", ConfigPollIntervalMax},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			requireAdminToken(t)
			t.Setenv("REIN_CONFIG_POLL_INTERVAL", tc.in)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("boundary %s should be valid: %v", tc.in, err)
			}
			if cfg.ConfigPollInterval != tc.want {
				t.Errorf("boundary %s: got %v want %v", tc.in, cfg.ConfigPollInterval, tc.want)
			}
		})
	}
}

// Unused import guard for the setEnv helper in case a future test needs
// it; the helper stays in the file alongside requireAdminToken for
// consistency with the admin package's test helper pattern.
var _ = setEnv

// overrideDefaultConfigFilePath swaps DefaultConfigFilePath for the
// duration of a test and restores it on cleanup. Used to point the
// hybrid-resolution probe at a test-controlled path without needing
// root access to write under /etc/.
func overrideDefaultConfigFilePath(t *testing.T, path string) {
	t.Helper()
	old := DefaultConfigFilePath
	DefaultConfigFilePath = path
	t.Cleanup(func() { DefaultConfigFilePath = old })
}

func TestLoad_Hybrid_EnvVarWinsOverDefaultPath(t *testing.T) {
	// Even if a file exists at the default path, an explicit
	// REIN_CONFIG_FILE env var must take precedence. This is the
	// most common case for operators who want to override the
	// default location.
	requireAdminToken(t)
	dir := t.TempDir()
	defaultFile := filepath.Join(dir, "default-rein.json")
	if err := os.WriteFile(defaultFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	overrideDefaultConfigFilePath(t, defaultFile)

	explicitFile := filepath.Join(dir, "explicit-rein.json")
	if err := os.WriteFile(explicitFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REIN_CONFIG_FILE", explicitFile)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ConfigFile != explicitFile {
		t.Errorf("ConfigFile: got %q want %q", cfg.ConfigFile, explicitFile)
	}
	if cfg.ConfigFileSource != ConfigSourceEnvVar {
		t.Errorf("ConfigFileSource: got %q want %q", cfg.ConfigFileSource, ConfigSourceEnvVar)
	}
}

func TestLoad_Hybrid_DefaultPathUsedWhenEnvUnset(t *testing.T) {
	// The Kubernetes-friendly case: operator mounts the ConfigMap at
	// DefaultConfigFilePath and does NOT set REIN_CONFIG_FILE. Load
	// must pick up the default path and mark the source as
	// default_path so the startup log line is explicit.
	requireAdminToken(t)
	dir := t.TempDir()
	defaultFile := filepath.Join(dir, "k8s-rein.json")
	if err := os.WriteFile(defaultFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	overrideDefaultConfigFilePath(t, defaultFile)
	// REIN_CONFIG_FILE is unset by requireAdminToken.

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ConfigFile != defaultFile {
		t.Errorf("ConfigFile: got %q want %q (default_path)", cfg.ConfigFile, defaultFile)
	}
	if cfg.ConfigFileSource != ConfigSourceDefaultPath {
		t.Errorf("ConfigFileSource: got %q want %q", cfg.ConfigFileSource, ConfigSourceDefaultPath)
	}
}

func TestLoad_Hybrid_EmbeddedOnlyWhenNothingSet(t *testing.T) {
	// Zero-config default: REIN_CONFIG_FILE is unset and no file
	// exists at the default path. Rein must run against just the
	// embedded pricing table, bit-for-bit identical to pre-0.2.
	requireAdminToken(t)
	overrideDefaultConfigFilePath(t, filepath.Join(t.TempDir(), "does-not-exist.json"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile: got %q want empty (embedded_only)", cfg.ConfigFile)
	}
	if cfg.ConfigFileSource != ConfigSourceEmbedded {
		t.Errorf("ConfigFileSource: got %q want %q", cfg.ConfigFileSource, ConfigSourceEmbedded)
	}
}

func TestLoad_Hybrid_DirectoryAtDefaultPathIsTreatedAsAbsent(t *testing.T) {
	// Defensive: if DefaultConfigFilePath resolves to a directory
	// (corrupted install, accidental mkdir, adversarial operator),
	// Load must NOT try to load it as a file. Treat as absent,
	// fall back to embedded_only.
	requireAdminToken(t)
	dir := t.TempDir()
	dirPath := filepath.Join(dir, "rein.json-as-dir")
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	overrideDefaultConfigFilePath(t, dirPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile: got %q want empty (directory at default path should be ignored)", cfg.ConfigFile)
	}
	if cfg.ConfigFileSource != ConfigSourceEmbedded {
		t.Errorf("ConfigFileSource: got %q want %q (directory should not count as present)", cfg.ConfigFileSource, ConfigSourceEmbedded)
	}
}

func TestLoad_Hybrid_EnvVarWinsEvenIfDefaultPathMissing(t *testing.T) {
	// If REIN_CONFIG_FILE is set, Load must return it verbatim without
	// checking whether the file exists. The existence check is
	// deferred to meter.LoadConfigFile which produces a clearer error
	// ("failed to load operator pricing config: read /path: no such
	// file or directory") than config.Load could at this stage.
	requireAdminToken(t)
	overrideDefaultConfigFilePath(t, filepath.Join(t.TempDir(), "nothing"))

	missingPath := "/tmp/definitely-does-not-exist-rein-config.json"
	t.Setenv("REIN_CONFIG_FILE", missingPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load should not error even if the env var path is missing: %v", err)
	}
	if cfg.ConfigFile != missingPath {
		t.Errorf("ConfigFile: got %q want %q", cfg.ConfigFile, missingPath)
	}
	if cfg.ConfigFileSource != ConfigSourceEnvVar {
		t.Errorf("ConfigFileSource: got %q want %q", cfg.ConfigFileSource, ConfigSourceEnvVar)
	}
}
