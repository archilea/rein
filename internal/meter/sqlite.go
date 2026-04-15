package meter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLite is a durable Meter backed by a local SQLite database. Totals survive
// a process restart, OOM, or kill -9. Safe for concurrent use; concurrent
// Record calls are serialized at the Go database/sql pool level via
// SetMaxOpenConns(1) in NewSQLite — see that function for the rationale.
//
// Schema: one row per (key_id, period), where period is "d:YYYY-MM-DD" or
// "m:YYYY-MM". WITHOUT ROWID makes the composite primary key a clustered
// index, so Check and Record walk one B-tree to the leaf page.
type SQLite struct {
	db *sql.DB
}

const sqliteMeterSchema = `
CREATE TABLE IF NOT EXISTS spend (
	key_id  TEXT NOT NULL,
	period  TEXT NOT NULL,
	amount  REAL NOT NULL,
	PRIMARY KEY (key_id, period)
) WITHOUT ROWID;`

// NewSQLite opens (or creates) a SQLite database at path and applies the
// spend schema. WAL mode and a 5s busy_timeout are set so the meter's handle
// coexists cleanly with the keystore's handle on the same file.
func NewSQLite(path string) (*SQLite, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite path is required")
	}
	// DSN mirrors the keystore's pragmas (internal/keys/sqlite.go) because
	// both handles run against the same file. foreign_keys(on) is a no-op
	// for the current spend schema but kept for consistency so any future
	// cross-table FK between spend and virtual_keys behaves identically on
	// either handle.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)",
		url.PathEscape(path),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc.org/sqlite (pure-Go, CGo-free) does not propagate busy_timeout
	// through the WAL write-lock path when multiple pool connections race to
	// call BeginTx simultaneously. Each goroutine gets its own connection from
	// the pool, they all try to acquire SQLite's single write lock at once, and
	// those that lose return SQLITE_BUSY immediately instead of waiting. The
	// 5s busy_timeout in the DSN covers external writers (e.g. the keystore
	// handle) but not intra-pool write-lock races.
	//
	// Capping the pool at one connection serializes Record calls at the Go
	// scheduler level (connection checkout blocks), which is equivalent to the
	// sync.Mutex used by the Memory implementation. Check reads share the
	// same single pool connection, so they queue behind any in-flight Record
	// transaction. This is acceptable for 0.2 because Record is called
	// post-response (off the client-critical path) and completes in well
	// under a millisecond on local SQLite. A future upgrade (if Check latency
	// under concurrent Record load becomes measurable) can add a second,
	// reader-only *sql.DB handle on the same file; modernc.org/sqlite + WAL
	// supports multi-handle read-during-write natively.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(sqliteMeterSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply meter schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

// Close releases the underlying database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// Check implements Meter. It reads the day and month totals for keyID in a
// single query (IN-clause) and returns ErrBudgetExceeded if either has already
// reached its cap. A cap of 0 means "no limit on this window"; if both are 0,
// Check skips the DB round trip entirely.
func (s *SQLite) Check(ctx context.Context, keyID string, dailyCap, monthCap float64) error {
	if dailyCap <= 0 && monthCap <= 0 {
		return nil
	}
	now := time.Now().UTC()
	dayKey := dayPeriodKey(now)
	monthKey := monthPeriodKey(now)

	rows, err := s.db.QueryContext(ctx,
		`SELECT period, amount FROM spend WHERE key_id = ? AND period IN (?, ?)`,
		keyID, dayKey, monthKey)
	if err != nil {
		return fmt.Errorf("query spend: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var daySpend, monthSpend float64
	for rows.Next() {
		var period string
		var amount float64
		if err := rows.Scan(&period, &amount); err != nil {
			return fmt.Errorf("scan spend: %w", err)
		}
		switch period {
		case dayKey:
			daySpend = amount
		case monthKey:
			monthSpend = amount
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate spend: %w", err)
	}

	if dailyCap > 0 && daySpend >= dailyCap {
		return ErrBudgetExceeded
	}
	if monthCap > 0 && monthSpend >= monthCap {
		return ErrBudgetExceeded
	}
	return nil
}

// Record implements Meter. cost is added to both the day and month buckets
// for keyID in a single transaction, so a crash between the two writes
// cannot leave them out of sync. Zero and negative costs are ignored to
// preserve the Memory implementation's contract.
func (s *SQLite) Record(ctx context.Context, keyID string, cost float64) error {
	if cost <= 0 {
		return nil
	}
	now := time.Now().UTC()
	dayKey := dayPeriodKey(now)
	monthKey := monthPeriodKey(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	const upsert = `
		INSERT INTO spend (key_id, period, amount)
		VALUES (?, ?, ?)
		ON CONFLICT(key_id, period) DO UPDATE SET amount = amount + excluded.amount`
	if _, err := tx.ExecContext(ctx, upsert, keyID, dayKey, cost); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("upsert day spend: %w", err)
	}
	if _, err := tx.ExecContext(ctx, upsert, keyID, monthKey, cost); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("upsert month spend: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
