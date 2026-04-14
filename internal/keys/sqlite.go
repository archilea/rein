package keys

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

// SQLite is a durable Store backed by a local SQLite database.
// Safe for concurrent use; serialization is handled by the driver.
// The upstream_key column is encrypted at rest using the supplied Cipher.
type SQLite struct {
	db     *sql.DB
	cipher Cipher
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS virtual_keys (
	id                 TEXT PRIMARY KEY,
	token              TEXT NOT NULL UNIQUE,
	name               TEXT NOT NULL,
	upstream           TEXT NOT NULL,
	upstream_key       TEXT NOT NULL,
	daily_budget_usd   REAL NOT NULL DEFAULT 0,
	month_budget_usd   REAL NOT NULL DEFAULT 0,
	upstream_base_url  TEXT NOT NULL DEFAULT '',
	rps_limit          INTEGER NOT NULL DEFAULT 0,
	rpm_limit          INTEGER NOT NULL DEFAULT 0,
	created_at         TEXT NOT NULL,
	revoked_at         TEXT
);`

// NewSQLite opens (or creates) a SQLite database at path and applies the schema.
// WAL mode is enabled so reads do not block writes.
// The cipher parameter is required and is used to encrypt/decrypt the
// upstream_key column at the storage boundary. Pass NoopCipher only in tests.
func NewSQLite(path string, c Cipher) (*SQLite, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite path is required")
	}
	if c == nil {
		return nil, errors.New("sqlite keystore requires a non-nil Cipher")
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)",
		url.PathEscape(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Forward migrations for databases created before the current schema.
	// Each entry must be idempotent and safe to run against a fresh DB.
	if err := applySQLiteMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return &SQLite{db: db, cipher: c}, nil
}

// applySQLiteMigrations adds any columns that the CREATE TABLE above has
// picked up since the oldest supported on-disk schema. We stay away from a
// migrations table for now: SQLite's PRAGMA table_info is authoritative, and
// all of these migrations are additive column adds so they commute.
func applySQLiteMigrations(db *sql.DB) error {
	has, err := sqliteColumnExists(db, "virtual_keys", "upstream_base_url")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(
			`ALTER TABLE virtual_keys ADD COLUMN upstream_base_url TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("add upstream_base_url column: %w", err)
		}
	}
	for _, col := range []struct {
		name string
		ddl  string
	}{
		{"rps_limit", `ALTER TABLE virtual_keys ADD COLUMN rps_limit INTEGER NOT NULL DEFAULT 0`},
		{"rpm_limit", `ALTER TABLE virtual_keys ADD COLUMN rpm_limit INTEGER NOT NULL DEFAULT 0`},
	} {
		has, err := sqliteColumnExists(db, "virtual_keys", col.name)
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(col.ddl); err != nil {
				return fmt.Errorf("add %s column: %w", col.name, err)
			}
		}
	}
	return nil
}

// sqliteColumnExists reports whether the named column exists on the named
// table, via PRAGMA table_info. Works on every SQLite version the pure-Go
// driver supports, unlike ADD COLUMN IF NOT EXISTS which landed in 3.35.
func sqliteColumnExists(db *sql.DB, table, column string) (bool, error) {
	// PRAGMA table_info takes a bareword identifier, not a placeholder,
	// so we validate the table name against a strict allow-list before
	// interpolating it to keep gosec happy and the surface trivially
	// auditable.
	if !validBareIdent(table) {
		return false, fmt.Errorf("pragma table_info: invalid table name %q", table)
	}
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, fmt.Errorf("pragma table_info: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan table_info: %w", err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// validBareIdent matches a conservative SQL identifier: lowercase letters,
// digits, and underscore, starting with a letter. Used to gate bareword
// interpolation in the one PRAGMA call that cannot take a placeholder.
func validBareIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case r == '_' && i > 0:
		default:
			return false
		}
	}
	return true
}

// Close releases the underlying database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// Create persists a new virtual key. Returns an error on ID/token collision or validation failure.
func (s *SQLite) Create(ctx context.Context, k *VirtualKey) error {
	if k == nil {
		return errors.New("nil virtual key")
	}
	if k.ID == "" || k.Token == "" {
		return errors.New("virtual key requires ID and Token")
	}
	if k.Upstream != UpstreamOpenAI && k.Upstream != UpstreamAnthropic {
		return fmt.Errorf("unsupported upstream %q", k.Upstream)
	}
	encUpstream, err := s.cipher.Encrypt(k.UpstreamKey)
	if err != nil {
		return fmt.Errorf("encrypt upstream key: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO virtual_keys (id, token, name, upstream, upstream_key, daily_budget_usd, month_budget_usd, upstream_base_url, rps_limit, rpm_limit, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Token, k.Name, k.Upstream, encUpstream,
		k.DailyBudgetUSD, k.MonthBudgetUSD, k.UpstreamBaseURL,
		k.RPSLimit, k.RPMLimit,
		k.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert virtual key: %w", err)
	}
	return nil
}

// GetByToken returns the virtual key matching the given secret bearer token.
func (s *SQLite) GetByToken(ctx context.Context, token string) (*VirtualKey, error) {
	return s.queryOne(ctx, "token", token)
}

// GetByID returns the virtual key matching the given public admin ID.
func (s *SQLite) GetByID(ctx context.Context, id string) (*VirtualKey, error) {
	return s.queryOne(ctx, "id", id)
}

func (s *SQLite) queryOne(ctx context.Context, column, value string) (*VirtualKey, error) {
	// Explicit allow-list of lookup columns. Callers inside this package are the
	// only producers; branching on the literal avoids fmt.Sprintf-into-SQL
	// (gosec G201) and makes the set of supported lookups obvious to a reader.
	var q string
	switch column {
	case "id":
		q = `SELECT id, token, name, upstream, upstream_key, daily_budget_usd,
			month_budget_usd, upstream_base_url, rps_limit, rpm_limit, created_at, revoked_at
			FROM virtual_keys WHERE id = ?`
	case "token":
		q = `SELECT id, token, name, upstream, upstream_key, daily_budget_usd,
			month_budget_usd, upstream_base_url, rps_limit, rpm_limit, created_at, revoked_at
			FROM virtual_keys WHERE token = ?`
	default:
		return nil, fmt.Errorf("queryOne: unsupported lookup column %q", column)
	}
	row := s.db.QueryRowContext(ctx, q, value)
	return s.scanKey(row.Scan)
}

// List returns every virtual key in the store, ordered by creation time ascending.
func (s *SQLite) List(ctx context.Context) ([]*VirtualKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, token, name, upstream, upstream_key, daily_budget_usd,
		       month_budget_usd, upstream_base_url, rps_limit, rpm_limit, created_at, revoked_at
		FROM virtual_keys ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*VirtualKey
	for rows.Next() {
		k, err := s.scanKey(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate keys: %w", err)
	}
	return out, nil
}

// Revoke marks a key as revoked. Revoked keys still resolve but IsRevoked() is true.
func (s *SQLite) Revoke(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx,
		`UPDATE virtual_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, id)
	if err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke rows affected: %w", err)
	}
	if n == 0 {
		// Distinguish "already revoked" from "not found".
		if _, err := s.GetByID(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// scanKey reads a single virtual_keys row via the supplied Scan function
// (either *sql.Row.Scan or *sql.Rows.Scan) and decrypts the upstream_key column.
func (s *SQLite) scanKey(scan func(dest ...any) error) (*VirtualKey, error) {
	var (
		k           VirtualKey
		encUpstream string
		createdAt   string
		revokedAt   sql.NullString
	)
	err := scan(&k.ID, &k.Token, &k.Name, &k.Upstream, &encUpstream,
		&k.DailyBudgetUSD, &k.MonthBudgetUSD, &k.UpstreamBaseURL,
		&k.RPSLimit, &k.RPMLimit,
		&createdAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan key: %w", err)
	}
	plain, err := s.cipher.Decrypt(encUpstream)
	if err != nil {
		return nil, fmt.Errorf("decrypt upstream key: %w", err)
	}
	k.UpstreamKey = plain
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	k.CreatedAt = t.UTC()
	if revokedAt.Valid {
		rt, err := time.Parse(time.RFC3339Nano, revokedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse revoked_at: %w", err)
		}
		rt = rt.UTC()
		k.RevokedAt = &rt
	}
	return &k, nil
}
