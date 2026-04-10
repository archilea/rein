// Package config loads Rein's runtime configuration from environment variables.
// Future work: support a rein.yaml file for richer config.
package config

import (
	"errors"
	"os"
)

// Config holds Rein's runtime settings.
type Config struct {
	Port          string
	AdminToken    string
	DatabaseURL   string
	EncryptionKey string
	OpenAIBase    string
	AnthropicBase string
}

// Load reads configuration from environment variables.
// It returns an error if any required variable is missing.
func Load() (*Config, error) {
	cfg := &Config{
		Port:          getenv("REIN_PORT", "8080"),
		AdminToken:    os.Getenv("REIN_ADMIN_TOKEN"),
		DatabaseURL:   getenv("REIN_DB_URL", "sqlite:./rein.db"),
		EncryptionKey: os.Getenv("REIN_ENCRYPTION_KEY"),
		OpenAIBase:    getenv("REIN_OPENAI_BASE", "https://api.openai.com"),
		AnthropicBase: getenv("REIN_ANTHROPIC_BASE", "https://api.anthropic.com"),
	}

	if cfg.AdminToken == "" {
		return nil, errors.New("REIN_ADMIN_TOKEN is required")
	}

	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
