package meter

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// PollLoop runs a mtime-triggered reload loop against path, calling
// TryReload whenever the file's mtime advances. It exits when ctx is
// cancelled. Errors from os.Stat (file temporarily missing, permission
// blip, I/O hiccup) are logged at ERROR level and the loop continues —
// the operator can fix the file and the next successful stat picks it
// up. The initial stat happens inside the loop body under the first
// ticker tick so that a file that is not present at PollLoop start
// does not cause the goroutine to immediately log an ERROR before the
// operator has had time to mount the volume.
//
// interval must be > 0. A zero or negative interval is a programming
// error in the caller (cmd/rein/main.go) and will panic on the ticker
// constructor; the config package rejects out-of-range values at
// startup so this path is only reached with a valid duration.
//
// Extracted from cmd/rein/main.go into the meter package so the
// mtime-skip, stat-error, and success branches can be unit-tested in
// isolation without spawning a subprocess or driving real signals.
func PollLoop(
	ctx context.Context,
	logger *slog.Logger,
	path string,
	interval time.Duration,
	base *Pricer,
	holder *PricerHolder,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Seed the lastModTime with whatever stat reports right now. If
	// the file does not exist at PollLoop start (common in Kubernetes
	// where the ConfigMap volume may mount slightly after the container
	// starts), lastModTime stays zero and the first successful stat
	// inside the loop will treat it as a change and trigger a reload.
	var lastModTime time.Time
	if info, err := os.Stat(path); err == nil {
		lastModTime = info.ModTime()
	}

	logger.Info("config poll active",
		"path", path,
		"interval", interval.String(),
	)

	for {
		select {
		case <-ctx.Done():
			logger.Info("config poll stopped", "path", path)
			return
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil {
				logger.Error("config poll stat failed",
					"path", path, "err", err.Error())
				continue
			}
			if info.ModTime().Equal(lastModTime) {
				continue
			}
			lastModTime = info.ModTime()
			TryReload(ctx, logger, "poll", path, base, holder)
		}
	}
}
