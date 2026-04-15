package meter

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSQLite_OpenCreatesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meter.db")
	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Re-opening the same file must succeed (idempotent schema).
	s2, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite second open: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
}

func TestSQLite_OpenRejectsEmptyPath(t *testing.T) {
	for _, path := range []string{"", "   ", "\t"} {
		if _, err := NewSQLite(path); err == nil {
			t.Errorf("NewSQLite(%q): expected error, got nil", path)
		}
	}
}

func TestSQLite_CheckWithNoCapsPasses(t *testing.T) {
	s := newTestSQLiteMeter(t)
	if err := s.Check(context.Background(), "key_x", 0, 0); err != nil {
		t.Errorf("no caps should always pass, got %v", err)
	}
}

func TestSQLite_CheckAgainstEmptyDBPasses(t *testing.T) {
	s := newTestSQLiteMeter(t)
	if err := s.Check(context.Background(), "key_x", 10.0, 10.0); err != nil {
		t.Errorf("no spend recorded yet, caps should pass, got %v", err)
	}
}

func TestSQLite_CheckPerKeyIsolation(t *testing.T) {
	// Exercises the composite PK filter on key_id. Uses the Record stub
	// via direct SQL insert since Record is implemented in Task 4.
	s := newTestSQLiteMeter(t)
	if _, err := s.db.Exec(
		`INSERT INTO spend (key_id, period, amount) VALUES (?, ?, ?)`,
		"key_a", dayPeriodKey(time.Now().UTC()), 100.0,
	); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	if err := s.Check(context.Background(), "key_b", 10.0, 10.0); err != nil {
		t.Errorf("key_b unaffected by key_a spend, got %v", err)
	}

	// Round-trip assertion: the seeded spend must actually be readable,
	// so key_a under a $10 cap must be blocked.
	if err := s.Check(context.Background(), "key_a", 10.0, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("key_a at $100 spend against $10 cap should be blocked, got %v", err)
	}
}

func TestSQLite_CheckDailyCapBreaches(t *testing.T) {
	s := newTestSQLiteMeter(t)
	if _, err := s.db.Exec(
		`INSERT INTO spend (key_id, period, amount) VALUES (?, ?, ?)`,
		"key_x", dayPeriodKey(time.Now().UTC()), 10.0,
	); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	ctx := context.Background()
	if err := s.Check(ctx, "key_x", 10.0, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("10 >= 10 daily cap should fail, got %v", err)
	}
	if err := s.Check(ctx, "key_x", 10.01, 0); err != nil {
		t.Errorf("10 < 10.01 daily cap should pass, got %v", err)
	}
}

func TestSQLite_CheckMonthlyCapBreaches(t *testing.T) {
	s := newTestSQLiteMeter(t)
	if _, err := s.db.Exec(
		`INSERT INTO spend (key_id, period, amount) VALUES (?, ?, ?)`,
		"key_x", monthPeriodKey(time.Now().UTC()), 100.0,
	); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	if err := s.Check(context.Background(), "key_x", 0, 50.0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("100 >= 50 monthly cap should fail, got %v", err)
	}
}

// newTestSQLiteMeter returns a fresh SQLite meter on a tempdir DB and
// registers cleanup.
func newTestSQLiteMeter(t *testing.T) *SQLite {
	t.Helper()
	s, err := NewSQLite(filepath.Join(t.TempDir(), "meter.db"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLite_RecordAddsToDayAndMonth(t *testing.T) {
	s := newTestSQLiteMeter(t)
	ctx := context.Background()
	mustRecord(t, s, "key_x", 5.0)
	mustRecord(t, s, "key_x", 7.5)
	// Daily total = 12.5. 12.49 cap should fail.
	if err := s.Check(ctx, "key_x", 12.49, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("12.5 >= 12.49 should fail, got %v", err)
	}
	// 12.51 cap should pass.
	if err := s.Check(ctx, "key_x", 12.51, 0); err != nil {
		t.Errorf("12.5 < 12.51 should pass, got %v", err)
	}
	// Monthly total also = 12.5.
	if err := s.Check(ctx, "key_x", 0, 12.49); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("monthly: 12.5 >= 12.49 should fail, got %v", err)
	}
}

func TestSQLite_RecordZeroOrNegativeIgnored(t *testing.T) {
	s := newTestSQLiteMeter(t)
	ctx := context.Background()
	if err := s.Record(ctx, "k", 0); err != nil {
		t.Fatalf("zero Record: %v", err)
	}
	if err := s.Record(ctx, "k", -5); err != nil {
		t.Fatalf("negative Record: %v", err)
	}
	if err := s.Check(ctx, "k", 0.0001, 0); err != nil {
		t.Errorf("zero/negative records should not count, got %v", err)
	}
}

func TestSQLite_ConcurrentRecordTotalsCorrect(t *testing.T) {
	s := newTestSQLiteMeter(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	const n = 200
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := s.Record(ctx, "key_c", 1.0); err != nil {
				t.Errorf("Record: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := s.Check(ctx, "key_c", 200.0, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("expected cap breach after 200 concurrent records, got %v", err)
	}
	if err := s.Check(ctx, "key_c", 200.01, 0); err != nil {
		t.Errorf("200 < 200.01 should pass, got %v", err)
	}
}

// mustRecord records cost on s and fails the test on error.
func mustRecord(t *testing.T, s *SQLite, keyID string, cost float64) {
	t.Helper()
	if err := s.Record(context.Background(), keyID, cost); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestSQLite_TotalsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meter.db")
	ctx := context.Background()

	// First process: record $42 then "crash" (close).
	s1, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	if err := s1.Record(ctx, "key_x", 42.0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second process: same file, same totals.
	s2, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	// The $42 must still be there.
	if err := s2.Check(ctx, "key_x", 42.0, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("post-reopen: 42 >= 42 cap should fail, got %v", err)
	}
	if err := s2.Check(ctx, "key_x", 42.01, 0); err != nil {
		t.Errorf("post-reopen: 42 < 42.01 cap should pass, got %v", err)
	}
}

func TestSQLite_TwoHandlesOnSameFileAgree(t *testing.T) {
	// The keystore and the meter may both open the same SQLite file. Verify
	// that a record via one handle is visible to a Check via a separate
	// handle opened at the same path.
	path := filepath.Join(t.TempDir(), "meter.db")
	ctx := context.Background()

	writer, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	reader, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite reader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	if err := writer.Record(ctx, "shared_key", 7.5); err != nil {
		t.Fatalf("Record on writer: %v", err)
	}
	if err := reader.Check(ctx, "shared_key", 7.5, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("reader should see writer's spend; got %v", err)
	}
}
