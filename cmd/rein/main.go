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
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/archilea/rein/internal/admin"
	"github.com/archilea/rein/internal/concurrency"
	"github.com/archilea/rein/internal/config"
	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
	"github.com/archilea/rein/internal/proxy"
	"github.com/archilea/rein/internal/rates"
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

	basePricer, err := meter.LoadPricer()
	if err != nil {
		logger.Error("failed to load pricing table", "err", err)
		os.Exit(1)
	}
	// Operator-editable pricing overrides (#25). The resolved config
	// file path and its source (env var, default path, or embedded
	// only) live on cfg after config.Load. A parse or validation
	// failure at startup is fatal — same posture as any other
	// required-config-missing path. The zero-config default (no env
	// var AND no file at DefaultConfigFilePath) is bit-for-bit
	// identical to pre-0.2 behavior.
	logger.Info("operator pricing config",
		"source", cfg.ConfigFileSource,
		"path", cfg.ConfigFile,
	)
	initialPricer, err := meter.LoadConfigFile(cfg.ConfigFile, basePricer)
	if err != nil {
		logger.Error("failed to load operator pricing config",
			"err", err, "path", cfg.ConfigFile, "source", cfg.ConfigFileSource)
		os.Exit(1)
	}
	pricerHolder := meter.NewPricerHolder(initialPricer)

	spendMeter, err := openSpendMeter(cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to open spend meter", "err", err, "url", cfg.DatabaseURL)
		os.Exit(1)
	}
	logger.Info("spend meter ready", "url", cfg.DatabaseURL)

	rateLimiter := rates.NewMemory()
	concurrencyStore := concurrency.NewMemory()
	p, err := proxy.New(keystore, killSwitch, spendMeter, rateLimiter, concurrencyStore, pricerHolder, cfg.OpenAIBase, cfg.AnthropicBase)
	if err != nil {
		logger.Error("failed to init proxy", "err", err)
		os.Exit(1)
	}

	adminSrv := admin.NewServer(cfg.AdminToken, killSwitch, keystore)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	// /readyz is the Kubernetes-style readiness probe (#76). Returns 200
	// normally and 503 once the drain flag is set, so orchestrators can
	// take this pod out of rotation before srv.Shutdown force-closes
	// anything. /healthz stays on the liveness semantic (process up) so a
	// drain does not trigger a restart-the-pod reaction.
	mux.HandleFunc("GET /readyz", readyzHandler(p))
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"rein","version":"` + version + `"}`))
	})
	mux.Handle("/v1/", p)
	adminSrv.Mount(mux)

	// inflight counts connections currently mid-request. Used only for
	// the "forcibly-closed on drain timeout" log line so operators can
	// see whether their REIN_SHUTDOWN_GRACE is too short. Tracked via
	// http.Server.ConnState transitions; see inflightConnTracker for
	// the exact idle/closed/hijacked handling.
	tracker := newInflightConnTracker()
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		ConnState:         tracker.observe,
	}

	go func() {
		logger.Info("rein listening", "addr", srv.Addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "err", err)
			os.Exit(1)
		}
	}()

	// Reload wiring (#25). Both triggers are set up only AFTER the initial
	// pricer is loaded and installed in the holder, so a SIGHUP arriving
	// during startup cannot race the initial load. The shutdownCtx is
	// cancelled by the SIGINT/SIGTERM handler below, which stops the poll
	// goroutine cleanly. SIGHUP has no cancellation equivalent; the
	// goroutine exits when the process does.
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	defer cancelShutdown()
	if cfg.ConfigFile != "" {
		startReloadHandlers(shutdownCtx, logger, cfg, basePricer, pricerHolder)
	}

	// Expiry sweeper (#77). Runs against the same shutdownCtx as the
	// reload handlers so SIGINT / SIGTERM cancels it cleanly before the
	// HTTP server drain; the sweeper never blocks graceful shutdown.
	go keys.RunExpirySweeper(shutdownCtx, keystore, cfg.ExpirySweepInterval)
	logger.Info("expiry sweeper started", "interval", cfg.ExpirySweepInterval)

	// Signal handling (#76). Buffer is 2 so a second signal arriving
	// during drain is captured instead of terminating the process via
	// the default handler. First signal → drain. Second signal within
	// the grace window → immediate force-close via srv.Close.
	stop := make(chan os.Signal, 2)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Info("shutdown signal received; entering drain", "grace", cfg.ShutdownGrace)
	p.SetDraining(true)
	cancelShutdown()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- srv.Shutdown(ctx) }()

	select {
	case err := <-shutdownErr:
		if err != nil {
			// Shutdown returns ctx.DeadlineExceeded when the grace
			// window expired with connections still in-flight. The
			// force-close count is a best-effort read of the tracker;
			// it is informative for tuning REIN_SHUTDOWN_GRACE but is
			// not a precise number because StateIdle / StateClosed
			// transitions can race the snapshot.
			logger.Warn("graceful shutdown timed out; in-flight requests force-closed",
				"err", err, "force_closed", tracker.inflight())
		}
	case <-stop:
		// Second signal during drain → immediate force-close. srv.Close
		// unblocks srv.Shutdown with http.ErrServerClosed, which we
		// drain from shutdownErr so the goroutine exits cleanly.
		logger.Warn("second shutdown signal received; force-closing",
			"force_closed", tracker.inflight())
		_ = srv.Close()
		<-shutdownErr
	}

	// Release the spend meter's DB handle, if any. The Meter interface does
	// not require Close, but the durable SQLite implementation does; without
	// this, the WAL sidecar files linger until the OS reclaims them.
	if closer, ok := spendMeter.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			logger.Error("close spend meter failed", "err", err)
		}
	}
}

// startReloadHandlers wires SIGHUP and (optionally) a background poller
// that re-read REIN_CONFIG_FILE and atomically swap the new Pricer into
// the holder. A failed reload is logged loudly but keeps the previous
// snapshot active — reload should never crash the process (#25 Q6). Both
// triggers share the same load-and-swap function so their failure and
// success behavior is identical by construction.
func startReloadHandlers(
	ctx context.Context,
	logger *slog.Logger,
	cfg *config.Config,
	basePricer *meter.Pricer,
	holder *meter.PricerHolder,
) {
	reload := func(trigger string) {
		// The log-level choice and asymmetric Q6 policy (unknown
		// version → WARN + keep, everything else → ERROR + keep) lives
		// in meter.TryReload so it can be unit-tested in isolation. The
		// SIGHUP goroutine and the poll goroutine both funnel through
		// this one call so a test of TryReload implicitly covers both.
		meter.TryReload(ctx, logger, trigger, cfg.ConfigFile, basePricer, holder)
	}

	// SIGHUP handler. Always on when REIN_CONFIG_FILE is set.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				reload("sighup")
			}
		}
	}()

	// Optional background poll. Opt-in via REIN_CONFIG_POLL_INTERVAL.
	// The loop lives in internal/meter.PollLoop so the mtime-skip,
	// stat-error, and success branches are unit-tested in isolation.
	if cfg.ConfigPollInterval > 0 {
		go meter.PollLoop(
			ctx,
			logger,
			cfg.ConfigFile,
			cfg.ConfigPollInterval,
			basePricer,
			holder,
		)
	}
}

// openSpendMeter builds a meter.Meter from the configured DB URL. The
// selection rule mirrors openKeystore:
//   - sqlite:<path>   meter.NewSQLite against the same file as the keystore;
//     totals are durable across restart.
//   - memory (or "")  meter.NewMemory; totals reset on restart.
//
// Any other scheme is a startup error so typos like "sqlite//..." do not
// silently downgrade to the in-memory meter.
func openSpendMeter(dbURL string) (meter.Meter, error) {
	trimmed := strings.TrimSpace(dbURL)
	if trimmed == "" || trimmed == "memory" || trimmed == "memory:" {
		return meter.NewMemory(), nil
	}
	path, ok := strings.CutPrefix(trimmed, "sqlite:")
	if !ok {
		return nil, fmt.Errorf("unsupported REIN_DB_URL scheme %q for meter (want sqlite:<path> or memory)", dbURL)
	}
	return meter.NewSQLite(path)
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// readyzHandler returns a handler that reports 200 + {"status":"ready"}
// when the proxy is serving new traffic and 503 + {"status":"draining"}
// once the proxy has been flipped into drain mode. The #76 contract is
// narrow on purpose: /readyz is strictly "should this replica be in the
// load balancer's pool?". It does not report keystore health, upstream
// reachability, or anything that would drift into observability scope.
//
// Kept in a small factory so the handler closes over the exact Proxy
// instance whose draining flag the signal handler flips. A test can
// build its own Proxy, flip SetDraining, and assert the response shape
// without bringing up the full http.Server.
func readyzHandler(p *proxy.Proxy) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if p.IsDraining() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"draining"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}
}

// inflightConnTracker reports the number of HTTP connections that are
// currently in the StateActive (mid-request) phase. Used only by the
// shutdown-drain path so the "connections force-closed" log line is
// informative for operators tuning REIN_SHUTDOWN_GRACE.
//
// StateActive increments inflight; every exit transition (StateIdle,
// StateClosed, StateHijacked) decrements, but only when we have a
// record of the connection entering StateActive. The sync.Map keys on
// the net.Conn pointer so a keep-alive connection that goes
// Active → Idle → Active → Idle does not double-count.
type inflightConnTracker struct {
	active sync.Map
	count  atomic.Int64
}

func newInflightConnTracker() *inflightConnTracker {
	return &inflightConnTracker{}
}

// observe is passed to http.Server.ConnState. It is invoked on every
// connection state transition the net/http package tracks. Safe for
// concurrent use: sync.Map and atomic.Int64 are both lock-free.
func (t *inflightConnTracker) observe(c net.Conn, state http.ConnState) {
	switch state {
	case http.StateActive:
		if _, loaded := t.active.LoadOrStore(c, struct{}{}); !loaded {
			t.count.Add(1)
		}
	case http.StateIdle, http.StateClosed, http.StateHijacked:
		if _, loaded := t.active.LoadAndDelete(c); loaded {
			t.count.Add(-1)
		}
	}
}

// inflight returns the current count. Single atomic load; the value
// can change the instant it returns, so callers should treat it as a
// best-effort snapshot rather than a transactional count.
func (t *inflightConnTracker) inflight() int64 {
	return t.count.Load()
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
