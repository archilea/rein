package rates

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock returns a clock function and an advance function.
func fakeClock(start time.Time) (func() time.Time, func(d time.Duration)) {
	var mu sync.Mutex
	now := start
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}, func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(d)
		}
}

func TestMemory_UnderLimitPasses(t *testing.T) {
	m := NewMemory()
	for i := 0; i < 10; i++ {
		if err := m.Allow(context.Background(), "k1", 10, 600); err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
	}
}

func TestMemory_RPSCapFires(t *testing.T) {
	now, _ := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newMemoryWithClock(now)
	for i := 0; i < 5; i++ {
		if err := m.Allow(context.Background(), "k1", 5, 0); err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
	}
	err := m.Allow(context.Background(), "k1", 5, 0)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("6th request: want ErrRateLimited, got %v", err)
	}
}

func TestMemory_RPMCapFires(t *testing.T) {
	now, _ := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newMemoryWithClock(now)
	for i := 0; i < 3; i++ {
		if err := m.Allow(context.Background(), "k1", 0, 3); err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
	}
	err := m.Allow(context.Background(), "k1", 0, 3)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("4th request: want ErrRateLimited, got %v", err)
	}
}

func TestMemory_BothSet_RPSBinds(t *testing.T) {
	now, _ := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newMemoryWithClock(now)
	// RPS=5, RPM=600. RPS should fire first.
	for i := 0; i < 5; i++ {
		if err := m.Allow(context.Background(), "k1", 5, 600); err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
	}
	err := m.Allow(context.Background(), "k1", 5, 600)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("6th request: want ErrRateLimited (RPS), got %v", err)
	}
}

func TestMemory_BothSet_RPMBinds(t *testing.T) {
	now, advance := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newMemoryWithClock(now)
	// RPS=100, RPM=10. RPM should fire after 10 requests spread over seconds.
	for i := 0; i < 10; i++ {
		if err := m.Allow(context.Background(), "k1", 100, 10); err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
		advance(200 * time.Millisecond) // spread over 2s total
	}
	err := m.Allow(context.Background(), "k1", 100, 10)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("11th request: want ErrRateLimited (RPM), got %v", err)
	}
}

func TestMemory_CounterResetsAcrossSecondBoundary(t *testing.T) {
	now, advance := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newMemoryWithClock(now)
	for i := 0; i < 10; i++ {
		if err := m.Allow(context.Background(), "k1", 10, 0); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if err := m.Allow(context.Background(), "k1", 10, 0); err == nil {
		t.Fatal("should be rate limited")
	}
	advance(1100 * time.Millisecond)
	if err := m.Allow(context.Background(), "k1", 10, 0); err != nil {
		t.Fatalf("after second boundary: %v", err)
	}
}

func TestMemory_CounterResetsAcrossMinuteBoundary(t *testing.T) {
	now, advance := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newMemoryWithClock(now)
	for i := 0; i < 5; i++ {
		if err := m.Allow(context.Background(), "k1", 0, 5); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if err := m.Allow(context.Background(), "k1", 0, 5); err == nil {
		t.Fatal("should be rate limited at RPM")
	}
	advance(61 * time.Second)
	if err := m.Allow(context.Background(), "k1", 0, 5); err != nil {
		t.Fatalf("after minute boundary: %v", err)
	}
}

func TestMemory_ZeroLimitsPayZeroCost(t *testing.T) {
	m := NewMemory()
	for i := 0; i < 1000; i++ {
		if err := m.Allow(context.Background(), "k1", 0, 0); err != nil {
			t.Fatalf("unlimited: %v", err)
		}
	}
	found := false
	m.counters.Range(func(_, _ any) bool { found = true; return false })
	if found {
		t.Error("unlimited key should not create a map entry")
	}
}

func TestMemory_BoundaryBurstBound(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 999_000_000, time.UTC)
	now, advance := fakeClock(start)
	m := newMemoryWithClock(now)

	allowed := 0
	for i := 0; i < 10; i++ {
		if err := m.Allow(context.Background(), "k1", 10, 0); err == nil {
			allowed++
		}
	}
	advance(1 * time.Millisecond)
	for i := 0; i < 10; i++ {
		if err := m.Allow(context.Background(), "k1", 10, 0); err == nil {
			allowed++
		}
	}
	if allowed > 11 {
		t.Errorf("boundary burst: %d allowed, want <= 11 (sliding window bound)", allowed)
	}
	t.Logf("boundary burst: %d/20 allowed with RPS=10", allowed)
}

func TestMemory_StaleWindow_MoreThanTwoWindowsElapsed(t *testing.T) {
	now, advance := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newMemoryWithClock(now)
	for i := 0; i < 10; i++ {
		_ = m.Allow(context.Background(), "k1", 10, 0)
	}
	advance(5 * time.Second)
	for i := 0; i < 10; i++ {
		if err := m.Allow(context.Background(), "k1", 10, 0); err != nil {
			t.Fatalf("after 5s gap, request %d: %v", i, err)
		}
	}
}

func TestMemory_RetryAfterValue(t *testing.T) {
	now, _ := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newMemoryWithClock(now)
	_ = m.Allow(context.Background(), "k1", 1, 0)
	err := m.Allow(context.Background(), "k1", 1, 0)
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	ra, ok := RetryAfter(err)
	if !ok {
		t.Fatal("RetryAfter should return true for rate limited error")
	}
	if ra < 1 {
		t.Errorf("Retry-After: got %d, want >= 1", ra)
	}
}

func TestMemory_ConcurrentSameKey(t *testing.T) {
	m := NewMemory()
	var wg sync.WaitGroup
	var allowed atomic.Int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if m.Allow(context.Background(), "k1", 50, 3000) == nil {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	a := allowed.Load()
	if a == 0 || a >= 100_000 {
		t.Errorf("concurrent: %d allowed, expected 0 < allowed < 100000", a)
	}
}

func TestMemory_DifferentKeysAreIndependent(t *testing.T) {
	now, _ := fakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newMemoryWithClock(now)
	for i := 0; i < 5; i++ {
		_ = m.Allow(context.Background(), "k1", 5, 0)
	}
	if err := m.Allow(context.Background(), "k1", 5, 0); err == nil {
		t.Fatal("k1 should be rate limited")
	}
	if err := m.Allow(context.Background(), "k2", 5, 0); err != nil {
		t.Fatalf("k2 should not be rate limited: %v", err)
	}
}
