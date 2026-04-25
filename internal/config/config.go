// Package config loads Rein's runtime configuration from environment variables.
// The operator-editable rein.json config file (#25) is orthogonal to this
// package; its path is read from REIN_CONFIG_FILE below, but the file itself
// is loaded by internal/meter.LoadConfigFile because it operates on the
// pricing table, not the env-var surface.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// ConfigPollIntervalMin / ConfigPollIntervalMax are the inclusive bounds on
// REIN_CONFIG_POLL_INTERVAL, per #25 Q2. Values outside this range are a
// fatal startup error. 1s is the practical floor because most filesystems
// do mtime with second resolution anyway; 1h is the "you probably meant
// 1m" ceiling.
const (
	ConfigPollIntervalMin = 1 * time.Second
	ConfigPollIntervalMax = 1 * time.Hour
)

// ExpirySweepInterval bounds: the sweeper runs once per tick to
// auto-revoke keys whose expires_at has passed. 10s is the floor to
// keep the per-tick scan cost negligible; 1h is the ceiling so operator
// misconfiguration ("1d") cannot silently delay audit-trail stamping
// for a whole day. The default matches the #77 design (60s); operators
// who want tighter audit drift set REIN_EXPIRY_SWEEP_INTERVAL=10s.
const (
	ExpirySweepIntervalMin     = 10 * time.Second
	ExpirySweepIntervalMax     = 1 * time.Hour
	ExpirySweepIntervalDefault = 60 * time.Second
)

// ShutdownGrace bounds (#76): the drain window gives in-flight LLM
// calls time to complete after SIGTERM/SIGINT before the HTTP server
// force-closes. 1s is the floor ("you really just wanted immediate
// shutdown" is honored by a second signal); 5m is the ceiling because
// anything longer than an LLM call timeout is pointless and most
// orchestrators will SIGKILL long before then. 30s default matches
// typical reasoning-model tail latency while still fitting inside a
// Kubernetes default terminationGracePeriodSeconds of 30.
const (
	ShutdownGraceMin     = 1 * time.Second
	ShutdownGraceMax     = 5 * time.Minute
	ShutdownGraceDefault = 30 * time.Second
)

// DefaultConfigFilePath is the well-known path Rein probes for an
// operator config file when REIN_CONFIG_FILE is unset. This matches
// the nginx / postgres / redis convention of "well-known path with
// env-var override" so Kubernetes operators can mount a ConfigMap at
// this path without also setting REIN_CONFIG_FILE in the pod spec.
//
// Exported as a variable (not a constant) so tests can point it at a
// temporary file without needing to write under /etc/ as root. It is
// never mutated in production.
var DefaultConfigFilePath = "/etc/rein/rein.json"

// Config source labels, set by Load based on which rule fired. Logged
// at startup so operators can see at a glance whether the running
// process picked up a config file from the env var, the default path,
// or neither.
const (
	ConfigSourceEnvVar      = "env_var"
	ConfigSourceDefaultPath = "default_path"
	ConfigSourceEmbedded    = "embedded_only"
)

// Config holds Rein's runtime settings.
type Config struct {
	Port                string
	AdminToken          string
	DatabaseURL         string
	EncryptionKey       string
	OpenAIBase          string
	AnthropicBase       string
	ConfigFile          string        // empty = use only the embedded pricing table
	ConfigFileSource    string        // one of ConfigSource* constants; set by Load
	ConfigPollInterval  time.Duration // zero = SIGHUP-only reload, no background poll
	ExpirySweepInterval time.Duration // tick cadence for the expiry auto-revocation sweeper
	ShutdownGrace       time.Duration // max drain window between SIGTERM and force-close (#76)
}

// Load reads configuration from environment variables.
// It returns an error if any required variable is missing or malformed.
func Load() (*Config, error) {
	rawPort := getenv("REIN_PORT", "8080")
	portNum, err := strconv.Atoi(rawPort)
	if err != nil || portNum < 1 || portNum > 65535 {
		return nil, fmt.Errorf("invalid REIN_PORT %q: must be an integer between 1 and 65535", rawPort)
	}

	cfg := &Config{
		Port:          rawPort,
		AdminToken:    os.Getenv("REIN_ADMIN_TOKEN"),
		DatabaseURL:   getenv("REIN_DB_URL", "sqlite:./rein.db"),
		EncryptionKey: os.Getenv("REIN_ENCRYPTION_KEY"),
		OpenAIBase:    getenv("REIN_OPENAI_BASE", "https://api.openai.com"),
		AnthropicBase: getenv("REIN_ANTHROPIC_BASE", "https://api.anthropic.com"),
		ConfigFile:    os.Getenv("REIN_CONFIG_FILE"),
	}

	if cfg.AdminToken == "" {
		return nil, errors.New("REIN_ADMIN_TOKEN is required")
	}

	// Resolve the operator config file path via the hybrid rule:
	//  1. If REIN_CONFIG_FILE is set, it wins (env_var).
	//  2. Otherwise probe DefaultConfigFilePath and use it if present
	//     (default_path). This is the Kubernetes-friendly shape: mount
	//     a ConfigMap at /etc/rein/rein.json and you do not need to
	//     also set an env var in the pod spec.
	//  3. Otherwise Rein runs zero-config against just the embedded
	//     pricing table (embedded_only). Bit-for-bit identical to
	//     pre-0.2 behavior.
	switch {
	case cfg.ConfigFile != "":
		cfg.ConfigFileSource = ConfigSourceEnvVar
	case defaultConfigFileExists():
		cfg.ConfigFile = DefaultConfigFilePath
		cfg.ConfigFileSource = ConfigSourceDefaultPath
	default:
		cfg.ConfigFileSource = ConfigSourceEmbedded
	}

	// REIN_CONFIG_POLL_INTERVAL is optional. Unset means SIGHUP-only
	// reload (the default). Set values are parsed via time.ParseDuration
	// and bounded to [ConfigPollIntervalMin, ConfigPollIntervalMax].
	if raw := os.Getenv("REIN_CONFIG_POLL_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("REIN_CONFIG_POLL_INTERVAL: %w", err)
		}
		if d < ConfigPollIntervalMin || d > ConfigPollIntervalMax {
			return nil, fmt.Errorf(
				"REIN_CONFIG_POLL_INTERVAL %s out of range; must be between %s and %s",
				d, ConfigPollIntervalMin, ConfigPollIntervalMax,
			)
		}
		cfg.ConfigPollInterval = d
	}

	// REIN_EXPIRY_SWEEP_INTERVAL is optional. Unset means the default
	// 60s tick. Set values are parsed via time.ParseDuration and bounded
	// to [ExpirySweepIntervalMin, ExpirySweepIntervalMax].
	cfg.ExpirySweepInterval = ExpirySweepIntervalDefault
	if raw := os.Getenv("REIN_EXPIRY_SWEEP_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("REIN_EXPIRY_SWEEP_INTERVAL: %w", err)
		}
		if d < ExpirySweepIntervalMin || d > ExpirySweepIntervalMax {
			return nil, fmt.Errorf(
				"REIN_EXPIRY_SWEEP_INTERVAL %s out of range; must be between %s and %s",
				d, ExpirySweepIntervalMin, ExpirySweepIntervalMax,
			)
		}
		cfg.ExpirySweepInterval = d
	}

	// REIN_SHUTDOWN_GRACE is optional. Unset means ShutdownGraceDefault
	// (30s). Set values are parsed via time.ParseDuration and bounded to
	// [ShutdownGraceMin, ShutdownGraceMax] so "0s" / "0" do not silently
	// disable the drain window and "24h" cannot make a pod unroll-able.
	cfg.ShutdownGrace = ShutdownGraceDefault
	if raw := os.Getenv("REIN_SHUTDOWN_GRACE"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("REIN_SHUTDOWN_GRACE: %w", err)
		}
		if d < ShutdownGraceMin || d > ShutdownGraceMax {
			return nil, fmt.Errorf(
				"REIN_SHUTDOWN_GRACE %s out of range; must be between %s and %s",
				d, ShutdownGraceMin, ShutdownGraceMax,
			)
		}
		cfg.ShutdownGrace = d
	}

	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// defaultConfigFileExists reports whether DefaultConfigFilePath is a
// regular file readable enough that os.Stat does not return an error.
// Any error from os.Stat — not-found, permission denied, I/O — is
// treated as "no default file", so Rein falls back cleanly to the
// embedded-only path and the real LoadConfigFile call never runs
// against a broken path. The exception is a permission-denied error,
// which is unusual enough in practice (operators control the container
// filesystem) that silently ignoring it is acceptable for 0.2.
func defaultConfigFileExists() bool {
	info, err := os.Stat(DefaultConfigFilePath)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
