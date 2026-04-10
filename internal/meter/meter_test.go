package meter

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemory_CheckWithNoCapsPasses(t *testing.T) {
	m := NewMemory()
	if err := m.Check(context.Background(), "key_x", 0, 0); err != nil {
		t.Errorf("no caps should always pass, got %v", err)
	}
}

func TestMemory_DailyCapEnforced(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	_ = m.Record(ctx, "key_x", 5.00)
	if err := m.Check(ctx, "key_x", 10.00, 0); err != nil {
		t.Errorf("5 < 10 cap should pass, got %v", err)
	}
	_ = m.Record(ctx, "key_x", 5.00)
	if err := m.Check(ctx, "key_x", 10.00, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("10 >= 10 cap should fail, got %v", err)
	}
}

func TestMemory_MonthlyCapEnforced(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	_ = m.Record(ctx, "key_x", 100.00)
	if err := m.Check(ctx, "key_x", 0, 50.00); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("100 spent against $50 monthly cap should fail, got %v", err)
	}
}

func TestMemory_PerKeyIsolation(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	_ = m.Record(ctx, "key_a", 100.00)
	if err := m.Check(ctx, "key_b", 10.00, 10.00); err != nil {
		t.Errorf("key_b should be unaffected by key_a's spend, got %v", err)
	}
}

func TestMemory_RecordZeroOrNegativeIgnored(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	_ = m.Record(ctx, "k", 0)
	_ = m.Record(ctx, "k", -5)
	if err := m.Check(ctx, "k", 0.0001, 0); err != nil {
		t.Errorf("zero/negative records should not count, got %v", err)
	}
}

func TestMemory_ConcurrentRecord(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	var wg sync.WaitGroup
	const n = 200
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = m.Record(ctx, "key_c", 1.00)
		}()
	}
	wg.Wait()
	// After 200 records of $1, $200 cap should be exactly at the boundary.
	if err := m.Check(ctx, "key_c", 200.00, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("expected cap breach after 200 concurrent records, got %v", err)
	}
}
