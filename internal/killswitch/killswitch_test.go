package killswitch

import (
	"context"
	"sync"
	"testing"
)

func TestMemory_DefaultsToUnfrozen(t *testing.T) {
	m := NewMemory()
	frozen, err := m.IsFrozen(context.Background())
	if err != nil {
		t.Fatalf("IsFrozen: %v", err)
	}
	if frozen {
		t.Error("new Memory should default to unfrozen")
	}
}

func TestMemory_SetFrozenToggle(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	if err := m.SetFrozen(ctx, true); err != nil {
		t.Fatalf("SetFrozen(true): %v", err)
	}
	frozen, _ := m.IsFrozen(ctx)
	if !frozen {
		t.Error("after SetFrozen(true), IsFrozen should be true")
	}

	if err := m.SetFrozen(ctx, false); err != nil {
		t.Fatalf("SetFrozen(false): %v", err)
	}
	frozen, _ = m.IsFrozen(ctx)
	if frozen {
		t.Error("after SetFrozen(false), IsFrozen should be false")
	}
}

func TestMemory_ConcurrentSafe(t *testing.T) {
	// Run many goroutines setting and reading the state. The -race detector
	// catches any unsafe access.
	ctx := context.Background()
	m := NewMemory()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(flag bool) {
			defer wg.Done()
			_ = m.SetFrozen(ctx, flag)
		}(i%2 == 0)
		go func() {
			defer wg.Done()
			_, _ = m.IsFrozen(ctx)
		}()
	}
	wg.Wait()
}
