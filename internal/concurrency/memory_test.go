package concurrency

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestMemory_UnlimitedAlwaysAcquires(t *testing.T) {
	m := NewMemory()
	for i := 0; i < 1000; i++ {
		if !m.Acquire("k1", 0) {
			t.Fatalf("unlimited Acquire should always succeed; failed at %d", i)
		}
	}
	// Unlimited must not touch the map at all.
	found := false
	m.counters.Range(func(_, _ any) bool { found = true; return false })
	if found {
		t.Error("unlimited key created a map entry; unlimited path must be allocation-free")
	}
}

func TestMemory_AcquireRejectsOverLimit(t *testing.T) {
	m := NewMemory()
	const limit = 3
	for i := 0; i < limit; i++ {
		if !m.Acquire("k1", limit) {
			t.Fatalf("under limit Acquire #%d should succeed", i)
		}
	}
	if m.Acquire("k1", limit) {
		t.Error("over-limit Acquire should return false")
	}
	if got := m.InFlight("k1"); got != limit {
		t.Errorf("InFlight: got %d want %d", got, limit)
	}
}

func TestMemory_ReleaseFreesSlot(t *testing.T) {
	m := NewMemory()
	if !m.Acquire("k1", 2) {
		t.Fatal("first Acquire failed")
	}
	if !m.Acquire("k1", 2) {
		t.Fatal("second Acquire failed")
	}
	if m.Acquire("k1", 2) {
		t.Fatal("3rd Acquire should be rejected")
	}
	m.Release("k1", 2)
	if !m.Acquire("k1", 2) {
		t.Error("Acquire after Release should succeed")
	}
}

func TestMemory_ReleaseUnlimitedNoOp(t *testing.T) {
	m := NewMemory()
	m.Release("never-acquired", 0) // must not panic
	m.Release("never-acquired", 5) // must not panic on unknown key
}

func TestMemory_DifferentKeysIndependent(t *testing.T) {
	m := NewMemory()
	for i := 0; i < 5; i++ {
		if !m.Acquire("k1", 5) {
			t.Fatalf("k1 #%d: unexpected reject", i)
		}
	}
	if m.Acquire("k1", 5) {
		t.Fatal("k1 should be at cap")
	}
	if !m.Acquire("k2", 5) {
		t.Error("k2 should not be affected by k1")
	}
}

func TestMemory_CounterNeverLeaks(t *testing.T) {
	m := NewMemory()
	const limit = 10
	const requests = 1000
	for i := 0; i < requests; i++ {
		if m.Acquire("k1", limit) {
			m.Release("k1", limit)
		}
	}
	if got := m.InFlight("k1"); got != 0 {
		t.Errorf("after balanced Acquire/Release: InFlight=%d want 0", got)
	}
}

// TestMemory_NoRace exercises high-concurrency Acquire/Release churn under
// -race to catch torn state, leaks, or counter underflows.
func TestMemory_NoRace(t *testing.T) {
	m := NewMemory()
	const limit = 50
	const goroutines = 100
	const iters = 500

	var peak atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if !m.Acquire("k1", limit) {
					continue
				}
				if n := m.InFlight("k1"); n > peak.Load() {
					peak.Store(n)
				}
				m.Release("k1", limit)
			}
		}()
	}
	wg.Wait()

	if p := peak.Load(); p > limit {
		t.Errorf("peak in-flight %d exceeded limit %d", p, limit)
	}
	if got := m.InFlight("k1"); got != 0 {
		t.Errorf("final InFlight=%d want 0", got)
	}
}

// TestMemory_SlotsHeldInvariant verifies that when many goroutines hold
// the slot simultaneously, the number of simultaneously-held slots (i.e.
// successful acquirers that have not yet released) never exceeds the
// limit, and the counter returns to zero after all release.
//
// Note: the underlying atomic counter (InFlight) may briefly read above
// the limit during a contended Add(1) / decrement-on-reject window. That
// is fine as long as no caller that observes a true return causes the
// "actually held" count to exceed the cap, which is what `held` measures.
func TestMemory_SlotsHeldInvariant(t *testing.T) {
	m := NewMemory()
	const limit = 50
	const goroutines = 200
	const holdFor = 2 * time.Millisecond

	start := make(chan struct{})
	var acquired atomic.Int64
	var rejected atomic.Int64
	var held atomic.Int64
	var peakHeld atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if m.Acquire("k1", limit) {
				acquired.Add(1)
				now := held.Add(1)
				for {
					p := peakHeld.Load()
					if now <= p || peakHeld.CompareAndSwap(p, now) {
						break
					}
				}
				time.Sleep(holdFor)
				held.Add(-1)
				m.Release("k1", limit)
			} else {
				rejected.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := peakHeld.Load(); got > int64(limit) {
		t.Errorf("peak held=%d exceeded limit %d", got, limit)
	}
	if got := acquired.Load() + rejected.Load(); got != int64(goroutines) {
		t.Errorf("acquired+rejected=%d want %d", got, goroutines)
	}
	if got := m.InFlight("k1"); got != 0 {
		t.Errorf("final InFlight=%d want 0", got)
	}
}

// TestMemory_PaddedCounterSize guards against a future edit accidentally
// dropping the cache-line padding. The padding is the reason multi-key
// workloads do not degrade from false sharing; losing it silently would be
// a hot-path regression that benchmarks alone might not catch on every
// hardware class.
func TestMemory_PaddedCounterSize(t *testing.T) {
	const cacheLine = 64
	got := unsafe.Sizeof(paddedCounter{})
	if got < cacheLine {
		t.Errorf("paddedCounter size=%d; must be >= %d bytes to prevent false sharing", got, cacheLine)
	}
}

func TestMemory_StoreInterfaceSatisfied(t *testing.T) {
	var _ Store = (*Memory)(nil)
	var _ Store = NewMemory()
}
