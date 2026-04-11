// Package config loads Rein's runtime configuration from environment variables.
// Future work: support a rein.yaml file for richer config.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
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
// It returns an error if any required variable is missing or if REIN_PORT is invalid.
func Load() (*Config, error) {
	port := getenv("REIN_PORT", "8080")
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("invalid REIN_PORT %q: must be a numeric port between 1 and 65535", port)
	}

	cfg := &Config{
		Port:          port,
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
