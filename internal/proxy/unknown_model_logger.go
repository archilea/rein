package proxy

import (
	"log/slog"
	"sync"
	"time"
)

// unknownModelLogger rate-limits "model not in pricing table; spend not
// recorded" WARN lines per (keyID, model) pair so an operator pointing a
// key at a per-key upstream base URL (#24) whose models are outside the
// embedded pricing table gets loud signals without log flooding.
//
// Policy: for the first burstWindow after a pair is first seen, every
// occurrence is logged so that a smoke test against a new provider
// surfaces the gap within seconds. After burstWindow elapses, the pair is
// rate-limited to one log line per steadyInterval. The burst window cannot
// be re-entered; to unblock logging the operator must either add the
// model to the pricing table (issue #25) or restart the process.
//
// State is small and unbounded only by (keys × distinct unknown models),
// which in any realistic deployment is dozens at most. No eviction.
// The logger is safe for concurrent use.
type unknownModelLogger struct {
	mu             sync.Mutex
	seen           map[unknownModelKey]unknownModelState
	burstWindow    time.Duration
	steadyInterval time.Duration
	// now is injectable for tests; production callers leave it nil and
	// get time.Now.
	now func() time.Time
}

type unknownModelKey struct {
	keyID string
	model string
}

type unknownModelState struct {
	firstSeen  time.Time
	lastLogged time.Time
}

func newUnknownModelLogger() *unknownModelLogger {
	return &unknownModelLogger{
		seen:           make(map[unknownModelKey]unknownModelState),
		burstWindow:    60 * time.Second,
		steadyInterval: 60 * time.Second,
	}
}

// shouldLog returns true if the caller should emit a WARN for this
// (keyID, model) pair right now, updating the internal dedupe state as a
// side effect.
func (l *unknownModelLogger) shouldLog(keyID, model string) bool {
	k := unknownModelKey{keyID: keyID, model: model}
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.seen[k]
	if !ok {
		l.seen[k] = unknownModelState{firstSeen: now, lastLogged: now}
		return true
	}
	// Still in the burst window: log every occurrence so a smoke-test
	// burst is fully visible.
	if now.Sub(state.firstSeen) <= l.burstWindow {
		state.lastLogged = now
		l.seen[k] = state
		return true
	}
	// Outside the burst window: rate-limit to one line per steadyInterval.
	if now.Sub(state.lastLogged) >= l.steadyInterval {
		state.lastLogged = now
		l.seen[k] = state
		return true
	}
	return false
}

// Warn emits a WARN line with the standard fields for unknown-model
// events, subject to the dedupe policy above. adapter is "openai" or
// "anthropic" so log consumers can pivot by provider adapter. Safe with
// a nil receiver (no-op) for test builds that skip logger construction.
func (l *unknownModelLogger) Warn(adapter, keyID, model string) {
	if l == nil {
		slog.Warn(adapter+" model not in pricing table; spend not recorded",
			"key_id", keyID, "model", model)
		return
	}
	if !l.shouldLog(keyID, model) {
		return
	}
	slog.Warn(adapter+" model not in pricing table; spend not recorded",
		"key_id", keyID, "model", model)
}
