package keys

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestKeyPatch_ApplyToUpstreamTimeoutSeconds exercises the branch in
// KeyPatch.ApplyTo that mutates the new field: nil leaves it alone,
// non-nil sets to the pointed value (including 0 to mean unlimited).
func TestKeyPatch_ApplyToUpstreamTimeoutSeconds(t *testing.T) {
	base := VirtualKey{UpstreamTimeoutSeconds: 30}

	// Nil patch: unchanged.
	vk := base
	(KeyPatch{}).ApplyTo(&vk)
	if vk.UpstreamTimeoutSeconds != 30 {
		t.Errorf("nil patch: got %d want 30", vk.UpstreamTimeoutSeconds)
	}

	// Non-nil zero patch: set to 0 (unlimited).
	vk = base
	zero := 0
	KeyPatch{UpstreamTimeoutSeconds: &zero}.ApplyTo(&vk)
	if vk.UpstreamTimeoutSeconds != 0 {
		t.Errorf("zero patch: got %d want 0", vk.UpstreamTimeoutSeconds)
	}

	// Non-nil positive patch: set to the new value.
	vk = base
	twelve := 12
	KeyPatch{UpstreamTimeoutSeconds: &twelve}.ApplyTo(&vk)
	if vk.UpstreamTimeoutSeconds != 12 {
		t.Errorf("positive patch: got %d want 12", vk.UpstreamTimeoutSeconds)
	}
}

// TestSQLite_UpstreamTimeoutRoundTrip drops a key with a set value
// through Create -> GetByToken -> GetByID -> List and confirms the
// column round-trips. Regression fence for INSERT / SELECT drift.
func TestSQLite_UpstreamTimeoutRoundTrip(t *testing.T) {
	s := newTestSQLite(t)

	id, err := GenerateID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	vk := &VirtualKey{
		ID:                     id,
		Token:                  token,
		Name:                   "rt-timeout",
		Upstream:               UpstreamOpenAI,
		UpstreamKey:            "sk-real",
		UpstreamTimeoutSeconds: 90,
		CreatedAt:              time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatalf("create: %v", err)
	}

	byTok, err := s.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatal(err)
	}
	if byTok.UpstreamTimeoutSeconds != 90 {
		t.Errorf("GetByToken: got %d want 90", byTok.UpstreamTimeoutSeconds)
	}

	byID, err := s.GetByID(context.Background(), vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if byID.UpstreamTimeoutSeconds != 90 {
		t.Errorf("GetByID: got %d want 90", byID.UpstreamTimeoutSeconds)
	}

	all, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].UpstreamTimeoutSeconds != 90 {
		t.Errorf("List: got %+v", all)
	}
}

// TestSQLite_UpdateChangesUpstreamTimeout confirms Update persists
// the new field. The existing Update path already covers UPDATE with
// zeroed fields; this test pins the timeout column specifically so
// future schema edits cannot regress silently.
func TestSQLite_UpdateChangesUpstreamTimeout(t *testing.T) {
	s := newTestSQLite(t)

	id, _ := GenerateID()
	token, _ := GenerateToken()
	vk := &VirtualKey{
		ID:          id,
		Token:       token,
		Name:        "update-timeout",
		Upstream:    UpstreamOpenAI,
		UpstreamKey: "sk-real",
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}

	newVal := 600
	updated, err := s.Update(context.Background(), id, KeyPatch{UpstreamTimeoutSeconds: &newVal})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.UpstreamTimeoutSeconds != 600 {
		t.Errorf("update returned: got %d want 600", updated.UpstreamTimeoutSeconds)
	}

	fresh, err := s.GetByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.UpstreamTimeoutSeconds != 600 {
		t.Errorf("refetch: got %d want 600", fresh.UpstreamTimeoutSeconds)
	}
}

// TestSQLite_UpstreamTimeoutColumnMigrationOnLegacyDB seeds a
// virtual_keys table that predates #30 (no upstream_timeout_seconds
// column) and asserts NewSQLite adds the column idempotently. Guards
// the forward-migration contract so upgrading an existing deployment
// does not require manual schema work.
func TestSQLite_UpstreamTimeoutColumnMigrationOnLegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Hand-craft a pre-#30 schema. Reassuringly close to the one
	// shipped in 0.2: no upstream_timeout_seconds column.
	legacyDDL := `CREATE TABLE virtual_keys (
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
		max_concurrent    INTEGER NOT NULL DEFAULT 0,
		created_at         TEXT NOT NULL,
		revoked_at         TEXT,
		expires_at         TEXT
	);`

	bootstrap, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Exec(legacyDDL); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	// Insert a legacy row so the migration is exercised on a
	// non-empty table.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := bootstrap.Exec(
		`INSERT INTO virtual_keys (id, token, name, upstream, upstream_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"key_0000000000000000", "rein_live_legacy", "legacy", "openai", "ciphertext", now,
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := bootstrap.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open via NewSQLite — this is where the migration fires.
	s, err := NewSQLite(path, mustAESGCM(t))
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	has, err := sqliteColumnExists(s.db, "virtual_keys", "upstream_timeout_seconds")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("migration did not add upstream_timeout_seconds column")
	}

	// Running the migration twice must be a no-op (idempotency).
	if err := applySQLiteMigrations(s.db); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
}
