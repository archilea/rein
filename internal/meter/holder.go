package meter

import (
	"sync/atomic"
)

// PricerHolder is a lock-free, concurrency-safe wrapper around a *Pricer
// that supports atomic swap. It exists so operator-editable pricing
// overrides (#25) can hot-reload the pricing table without taking a lock
// on the request hot path.
//
// The hot path is a single atomic.Pointer[Pricer].Load(), which is an
// aligned 8-byte read on modern hardware and should be lost in the noise
// of the ~33 µs SQLite+budget hot path measured in bench_test.go. A
// background SIGHUP or poll goroutine calls Swap to install a new Pricer
// snapshot after a successful config reload; readers always see a
// complete snapshot, never a half-swapped state.
//
// Holder must be constructed via NewPricerHolder; a zero-value holder
// returns nil from Load and would panic a caller that dereferences the
// result without checking. Construction is one line in main.go so there
// is no reason to skip it.
type PricerHolder struct {
	ptr atomic.Pointer[Pricer]
}

// NewPricerHolder returns a holder wrapping initial. initial must not be
// nil; passing nil is a programming error that the caller should catch
// before main() returns.
func NewPricerHolder(initial *Pricer) *PricerHolder {
	h := &PricerHolder{}
	h.ptr.Store(initial)
	return h
}

// Load returns the current *Pricer snapshot. Safe for concurrent use;
// lock-free; no allocations.
func (h *PricerHolder) Load() *Pricer {
	return h.ptr.Load()
}

// Swap installs next as the new current snapshot and returns the
// previous snapshot. Subsequent Load calls return next. In-flight
// callers that have already captured the previous pointer continue to
// use it until they return; this is safe because Pricer is immutable
// once constructed.
//
// Swap is called from the SIGHUP handler and the optional poll
// goroutine on a successful reload. A failed reload does not call Swap,
// so the previous snapshot stays active and a bad config file cannot
// break a running process.
func (h *PricerHolder) Swap(next *Pricer) *Pricer {
	return h.ptr.Swap(next)
}
