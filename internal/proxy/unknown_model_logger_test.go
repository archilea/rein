package proxy

import (
	"testing"
	"time"
)

func TestUnknownModelLogger_FirstSeenLogs(t *testing.T) {
	l := newUnknownModelLogger()
	if !l.shouldLog("key_a", "llama-3.3") {
		t.Errorf("first occurrence should log")
	}
}

func TestUnknownModelLogger_BurstWindow(t *testing.T) {
	// Inject a clock we control.
	base := time.Now()
	clock := base
	l := newUnknownModelLogger()
	l.now = func() time.Time { return clock }

	// t=0..10s: every call inside the burst window logs.
	for i := 0; i < 10; i++ {
		clock = base.Add(time.Duration(i) * time.Second)
		if !l.shouldLog("key_a", "llama-3.3") {
			t.Errorf("call %d in burst window should log", i)
		}
	}
	// After the loop, state.lastLogged is t=9s.

	// t=70s: past burstWindow (70-0=70 > 60) AND past steadyInterval since
	// the last burst log (70-9=61 >= 60), so the next line logs.
	clock = base.Add(70 * time.Second)
	if !l.shouldLog("key_a", "llama-3.3") {
		t.Errorf("t=70s (first post-burst tick) should log")
	}

	// t=90s: 20s since the previous post-burst log — suppressed.
	clock = base.Add(90 * time.Second)
	if l.shouldLog("key_a", "llama-3.3") {
		t.Errorf("t=90s within steady interval should be suppressed")
	}

	// t=131s: 61s since the previous post-burst log — logs again.
	clock = base.Add(131 * time.Second)
	if !l.shouldLog("key_a", "llama-3.3") {
		t.Errorf("t=131s after steady interval should log")
	}
}

func TestUnknownModelLogger_SuppressWithinBurstIsNotAThing(t *testing.T) {
	// Regression guard: during the burst window, EVERY call must log,
	// even if they arrive back-to-back in the same millisecond. This is
	// the "loud at the start so a smoke test sees the gap" property.
	base := time.Now()
	clock := base
	l := newUnknownModelLogger()
	l.now = func() time.Time { return clock }

	for i := 0; i < 1000; i++ {
		if !l.shouldLog("key_a", "m1") {
			t.Fatalf("call %d inside burst should log", i)
		}
	}
}

func TestUnknownModelLogger_DistinctPairsIndependent(t *testing.T) {
	l := newUnknownModelLogger()
	if !l.shouldLog("key_a", "m1") {
		t.Errorf("key_a m1 first should log")
	}
	if !l.shouldLog("key_b", "m1") {
		t.Errorf("key_b m1 first should log (distinct key)")
	}
	if !l.shouldLog("key_a", "m2") {
		t.Errorf("key_a m2 first should log (distinct model)")
	}
}

func TestUnknownModelLogger_NilReceiverNoOp(t *testing.T) {
	// A nil *unknownModelLogger must not panic. The fallback path emits a
	// plain slog.Warn (no dedupe) which is fine for callers that skip
	// construction — for example, in a benchmark where we do not care
	// about log volume.
	var l *unknownModelLogger
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil receiver panicked: %v", r)
		}
	}()
	l.Warn("openai", "key_a", "m1")
}
