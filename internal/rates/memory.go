package rates

import (
	"context"
	"sync"
	"time"
)

// Memory is the default in-memory rates.Store. Counters live in process
// memory and reset on restart. Suitable for single-replica deployments.
// In a multi-replica deployment, per-key limits are per-replica, not
// global. A globally-synchronized variant (Redis + Lua, #53) is out of
// scope for 0.2 but slots in behind the Store interface.
type Memory struct {
	counters sync.Map         // map[string]*keyState
	now      func() time.Time // injectable clock; defaults to time.Now
}

// NewMemory returns an empty in-memory rate limiter.
func NewMemory() *Memory {
	return &Memory{now: time.Now}
}

// newMemoryWithClock returns a Memory whose time source is now.
// Used in tests for deterministic time control.
func newMemoryWithClock(now func() time.Time) *Memory {
	return &Memory{now: now}
}

type keyState struct {
	mu  sync.Mutex
	sec windowCounter // 1-second window
	min windowCounter // 60-second window
}

type windowCounter struct {
	windowStart int64 // UTC unix nanos
	prevCount   int64
	currCount   int64
}

const (
	secWindowNS = int64(time.Second)
	minWindowNS = int64(time.Minute)
)

// Allow implements Store.
func (m *Memory) Allow(_ context.Context, keyID string, rpsLimit, rpmLimit int) error {
	if rpsLimit <= 0 && rpmLimit <= 0 {
		return nil // unlimited fast path, no map touch
	}

	v, _ := m.counters.LoadOrStore(keyID, &keyState{})
	ks := v.(*keyState)

	now := m.now().UTC().UnixNano()

	ks.mu.Lock()
	defer ks.mu.Unlock()

	// Check RPS.
	if rpsLimit > 0 {
		est := estimateCount(&ks.sec, now, secWindowNS)
		if est+1 > int64(rpsLimit) {
			remaining := secWindowNS - (now - ks.sec.windowStart)
			retryS := int(remaining/int64(time.Second)) + 1
			return newRetryAfterError(retryS)
		}
	}

	// Check RPM.
	if rpmLimit > 0 {
		est := estimateCount(&ks.min, now, minWindowNS)
		if est+1 > int64(rpmLimit) {
			remaining := minWindowNS - (now - ks.min.windowStart)
			retryS := int(remaining/int64(time.Second)) + 1
			return newRetryAfterError(retryS)
		}
	}

	// Both passed. Increment counters.
	if rpsLimit > 0 {
		ks.sec.currCount++
	}
	if rpmLimit > 0 {
		ks.min.currCount++
	}
	return nil
}

// estimateCount returns the sliding window estimate for the current moment.
// It also advances the window if time has elapsed past the current window.
func estimateCount(w *windowCounter, now, windowNS int64) int64 {
	elapsed := now - w.windowStart
	if elapsed >= windowNS {
		windows := elapsed / windowNS
		if windows == 1 {
			w.prevCount = w.currCount
		} else {
			w.prevCount = 0
		}
		w.currCount = 0
		w.windowStart = now - (elapsed % windowNS)
		elapsed = now - w.windowStart
	}
	// Integer math: prevCount * (windowNS - elapsed) / windowNS + currCount
	return w.prevCount*(windowNS-elapsed)/windowNS + w.currCount
}
