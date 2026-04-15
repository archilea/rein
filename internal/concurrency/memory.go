package concurrency

import (
	"sync"
	"sync/atomic"
)

// paddedCounter is cache-line-aligned (64 bytes on common x86/ARM) to
// prevent false sharing between counters for different keys. Without
// this padding, Go's allocator can place multiple counters on the same
// cache line, and goroutines hitting distinct keys would invalidate each
// other's cache lines on multi-core machines, degrading high-parallelism
// multi-key workloads by 2-5x purely from cache-coherency traffic.
type paddedCounter struct {
	count atomic.Int64
	_     [56]byte // 64 byte cache line minus the 8 byte atomic int
}

// Memory is the default in-memory concurrency.Store. Counters live in
// process memory and reset when the process restarts, which is safe
// because in-flight requests cannot outlive the process. Suitable for
// single-replica deployments; multi-replica deployments where per-key
// limits must be globally synchronized should use a future shared-state
// Store that slots in behind this same interface.
type Memory struct {
	counters sync.Map // map[string]*paddedCounter
}

// NewMemory returns an empty in-memory concurrency store.
func NewMemory() *Memory { return &Memory{} }

// Acquire implements Store.
func (m *Memory) Acquire(keyID string, limit int) bool {
	if limit <= 0 {
		return true
	}
	ctr := m.loadOrCreate(keyID)
	if ctr.count.Add(1) > int64(limit) {
		ctr.count.Add(-1)
		return false
	}
	return true
}

// Release implements Store.
func (m *Memory) Release(keyID string, limit int) {
	if limit <= 0 {
		return
	}
	v, ok := m.counters.Load(keyID)
	if !ok {
		return
	}
	v.(*paddedCounter).count.Add(-1)
}

// InFlight returns the current in-flight count for keyID. Intended for
// tests and diagnostics; not on the hot path.
func (m *Memory) InFlight(keyID string) int64 {
	v, ok := m.counters.Load(keyID)
	if !ok {
		return 0
	}
	return v.(*paddedCounter).count.Load()
}

func (m *Memory) loadOrCreate(keyID string) *paddedCounter {
	if v, ok := m.counters.Load(keyID); ok {
		return v.(*paddedCounter)
	}
	v, _ := m.counters.LoadOrStore(keyID, &paddedCounter{})
	return v.(*paddedCounter)
}
