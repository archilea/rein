package proxy

import (
	"io"
	"log/slog"
	"os"
)

// init silences slog during benchmark runs when REIN_BENCH_QUIET=1, so the
// output isn't overwhelmed by per-request "openai usage" log lines. Tests
// that want to assert on log output should unset the variable.
func init() {
	if os.Getenv("REIN_BENCH_QUIET") == "1" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}
}
