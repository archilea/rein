// Package main is the entry point for the Rein binary.
//
// Rein is a control-first reverse proxy for LLM API calls. It enforces
// hard budget limits, exposes a kill-switch, and meters token spend in
// real time. See https://github.com/archilea/rein for details.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/archilea/rein/internal/admin"
	"github.com/archilea/rein/internal/config"
	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
	"github.com/archilea/rein/internal/proxy"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	keystore, err := openKeystore(cfg.DatabaseURL, cfg.EncryptionKey)
	if err != nil {
		logger.Error("failed to open keystore", "err", err, "url", cfg.DatabaseURL)
		os.Exit(1)
	}
	logger.Info("keystore ready", "url", cfg.DatabaseURL)

	killSwitch := killswitch.NewMemory()

	pricer, err := meter.LoadPricer()
	if err != nil {
		logger.Error("failed to load pricing table", "err", err)
		os.Exit(1)
	}
	spendMeter := meter.NewMemory()

	p, err := proxy.New(keystore, killSwitch, spendMeter, pricer, cfg.OpenAIBase, cfg.AnthropicBase)
	if err != nil {
		logger.Error("failed to init proxy", "err", err)
		os.Exit(1)
	}

	adminSrv := admin.NewServer(cfg.AdminToken, killSwitch, keystore)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"rein","version":"` + version + `"}`))
	})
	mux.Handle("/v1/", p)
	adminSrv.Mount(mux)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Info("rein listening", "addr", srv.Addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// openKeystore builds a keys.Store from the configured DB URL.
// Supported schemes:
//   - sqlite:<path>   durable on-disk store (default); requires REIN_ENCRYPTION_KEY
//   - memory          in-memory store, cleared on restart (tests / ephemeral use)
//
// For sqlite, upstream API keys are encrypted at rest with AES-256-GCM using
// REIN_ENCRYPTION_KEY (32 bytes, hex-encoded = 64 hex chars). The process
// refuses to start without a valid key to prevent silent plaintext storage.
func openKeystore(dbURL, encryptionKeyHex string) (keys.Store, error) {
	trimmed := strings.TrimSpace(dbURL)
	if trimmed == "" || trimmed == "memory" || trimmed == "memory:" {
		return keys.NewMemory(), nil
	}
	path, ok := strings.CutPrefix(trimmed, "sqlite:")
	if !ok {
		return nil, fmt.Errorf("unsupported REIN_DB_URL scheme %q (want sqlite:<path> or memory)", dbURL)
	}
	if strings.TrimSpace(encryptionKeyHex) == "" {
		return nil, errors.New("REIN_ENCRYPTION_KEY is required for sqlite keystore. Generate one with: openssl rand -hex 32")
	}
	rawKey, err := keys.DecodeHexKey(encryptionKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid REIN_ENCRYPTION_KEY: %w", err)
	}
	cipher, err := keys.NewAESGCM(rawKey)
	if err != nil {
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	return keys.NewSQLite(path, cipher)
}
