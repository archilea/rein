package keys

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSQLite(t *testing.T) *SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rein_test.db")
	s, err := NewSQLite(path, mustAESGCM(t))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCreate(t *testing.T, s Store, name, upstream, upstreamKey string) *VirtualKey {
	t.Helper()
	id, err := GenerateID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	vk := &VirtualKey{
		ID:          id,
		Token:       token,
		Name:        name,
		Upstream:    upstream,
		UpstreamKey: upstreamKey,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatalf("create: %v", err)
	}
	return vk
}

func TestSQLite_CreateAndLookup(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "prod", UpstreamOpenAI, "sk-real")

	got, err := s.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if got.ID != vk.ID || got.UpstreamKey != "sk-real" || got.Upstream != UpstreamOpenAI {
		t.Errorf("round-trip: got %+v", got)
	}
	if got.IsRevoked() {
		t.Errorf("new key should not be revoked")
	}

	byID, err := s.GetByID(context.Background(), vk.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if byID.Token != vk.Token {
		t.Errorf("byID token mismatch: got %q want %q", byID.Token, vk.Token)
	}
}

func TestSQLite_NotFound(t *testing.T) {
	s := newTestSQLite(t)
	if _, err := s.GetByToken(context.Background(), "rein_live_nope"); err != ErrNotFound {
		t.Errorf("got err=%v want ErrNotFound", err)
	}
	if _, err := s.GetByID(context.Background(), "key_nope"); err != ErrNotFound {
		t.Errorf("got err=%v want ErrNotFound", err)
	}
}

func TestSQLite_UnsupportedUpstream(t *testing.T) {
	s := newTestSQLite(t)
	err := s.Create(context.Background(), &VirtualKey{
		ID: "key_x", Token: "rein_live_x", Upstream: "cohere", UpstreamKey: "sk-x",
	})
	if err == nil {
		t.Fatal("expected error for unsupported upstream")
	}
}

func TestSQLite_RevokeAndList(t *testing.T) {
	s := newTestSQLite(t)
	a := mustCreate(t, s, "a", UpstreamOpenAI, "sk-a")
	b := mustCreate(t, s, "b", UpstreamAnthropic, "sk-ant-b")

	if err := s.Revoke(context.Background(), a.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got, err := s.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsRevoked() {
		t.Errorf("a should be revoked")
	}

	all, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("list: got %d want 2", len(all))
	}

	stillThere, err := s.GetByID(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillThere.IsRevoked() {
		t.Errorf("b should not be revoked")
	}
}

func TestSQLite_RevokeNotFound(t *testing.T) {
	s := newTestSQLite(t)
	if err := s.Revoke(context.Background(), "key_missing"); err != ErrNotFound {
		t.Errorf("got %v want ErrNotFound", err)
	}
}

func TestSQLite_DuplicateTokenRejected(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "a", UpstreamOpenAI, "sk-a")

	dup := &VirtualKey{
		ID: "key_other", Token: vk.Token, Name: "b",
		Upstream: UpstreamOpenAI, UpstreamKey: "sk-b", CreatedAt: time.Now().UTC(),
	}
	if err := s.Create(context.Background(), dup); err == nil {
		t.Fatal("expected duplicate token error")
	}
}

func TestSQLite_Persistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")
	// Use a fixed key so the second handle can decrypt rows written by the first.
	fixedKey := make([]byte, AESKeySize)
	for i := range fixedKey {
		fixedKey[i] = byte(i)
	}
	c, err := NewAESGCM(fixedKey)
	if err != nil {
		t.Fatal(err)
	}

	s1, err := NewSQLite(path, c)
	if err != nil {
		t.Fatal(err)
	}
	vk := mustCreate(t, s1, "persist", UpstreamOpenAI, "sk-persist")
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewSQLite(path, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.UpstreamKey != "sk-persist" {
		t.Errorf("upstream_key after reopen: got %q want sk-persist", got.UpstreamKey)
	}
}

func TestSQLite_UpstreamKeyEncryptedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enc.db")
	c := mustAESGCM(t)
	s, err := NewSQLite(path, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	vk := mustCreate(t, s, "enc", UpstreamOpenAI, "sk-very-secret-upstream")

	// Read the raw column directly via a bare SQL handle and confirm
	// the plaintext is NOT present and the v1: tag IS present.
	var raw string
	if err := s.db.QueryRow(`SELECT upstream_key FROM virtual_keys WHERE id = ?`, vk.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "sk-very-secret-upstream" {
		t.Fatal("upstream_key is stored in plaintext; encryption is not being applied")
	}
	if !strings.HasPrefix(raw, "v1:") {
		t.Errorf("expected v1: ciphertext prefix, got %q", raw)
	}
}

func TestSQLite_UpstreamBaseURLRoundTrip(t *testing.T) {
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
		ID:              id,
		Token:           token,
		Name:            "groq-shadow",
		Upstream:        UpstreamOpenAI,
		UpstreamKey:     "gsk-real",
		UpstreamBaseURL: "https://api.groq.com",
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if got.UpstreamBaseURL != "https://api.groq.com" {
		t.Errorf("upstream_base_url round-trip: got %q want https://api.groq.com", got.UpstreamBaseURL)
	}

	// Confirm the column survives list.
	all, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("list length: got %d want 1", len(all))
	}
	if all[0].UpstreamBaseURL != "https://api.groq.com" {
		t.Errorf("list upstream_base_url: got %q want https://api.groq.com", all[0].UpstreamBaseURL)
	}
}

func TestSQLite_UpstreamBaseURLDefaultsEmpty(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "default", UpstreamOpenAI, "sk-x")
	got, err := s.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpstreamBaseURL != "" {
		t.Errorf("default upstream_base_url: got %q want empty", got.UpstreamBaseURL)
	}
}

func TestSQLite_UpstreamBaseURLMigrationIdempotent(t *testing.T) {
	// Open a fresh DB then reopen. The migration runs twice and must not fail
	// or duplicate the column.
	path := filepath.Join(t.TempDir(), "migrate.db")
	c := mustAESGCM(t)
	s1, err := NewSQLite(path, c)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()

	s2, err := NewSQLite(path, c)
	if err != nil {
		t.Fatalf("reopen after migration: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	// Confirm the column is present by writing and reading a value.
	id, _ := GenerateID()
	token, _ := GenerateToken()
	vk := &VirtualKey{
		ID:              id,
		Token:           token,
		Name:            "m",
		Upstream:        UpstreamOpenAI,
		UpstreamKey:     "sk-m",
		UpstreamBaseURL: "https://api.fireworks.ai",
		CreatedAt:       time.Now().UTC(),
	}
	if err := s2.Create(context.Background(), vk); err != nil {
		t.Fatalf("create after reopen: %v", err)
	}
	got, err := s2.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpstreamBaseURL != "https://api.fireworks.ai" {
		t.Errorf("upstream_base_url after reopen: got %q want https://api.fireworks.ai", got.UpstreamBaseURL)
	}
}

// TestSQLite_UpstreamBaseURLMigrationFromPre02Schema exercises the
// !sqliteColumnExists branch of applySQLiteMigrations by physically
// materializing a pre-0.2 virtual_keys schema (no upstream_base_url column)
// at a temp path, closing the raw handle, and then opening via NewSQLite.
// The migration must run ALTER TABLE ADD COLUMN and leave the DB in a state
// where a VirtualKey with UpstreamBaseURL round-trips correctly.
func TestSQLite_UpstreamBaseURLMigrationFromPre02Schema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre02.db")

	// Open a raw handle and create the pre-0.2 virtual_keys schema without
	// the upstream_base_url column. We deliberately do NOT go through
	// NewSQLite because that would apply the current schema + migrations.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		url.PathEscape(path))
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	const pre02Schema = `
		CREATE TABLE virtual_keys (
			id                 TEXT PRIMARY KEY,
			token              TEXT NOT NULL UNIQUE,
			name               TEXT NOT NULL,
			upstream           TEXT NOT NULL,
			upstream_key       TEXT NOT NULL,
			daily_budget_usd   REAL NOT NULL DEFAULT 0,
			month_budget_usd   REAL NOT NULL DEFAULT 0,
			created_at         TEXT NOT NULL,
			revoked_at         TEXT
		);`
	if _, err := raw.Exec(pre02Schema); err != nil {
		t.Fatalf("create pre-0.2 schema: %v", err)
	}
	// Insert a legacy row using the pre-0.2 column set. Encryption expects
	// v1:-prefixed ciphertext, but we can just use a real cipher via
	// NewAESGCM through a second SQLite handle after the migration.
	// For this test the legacy row body is irrelevant; what matters is
	// that the migration runs ALTER TABLE ADD COLUMN without error on a
	// table that was not created by the current CREATE TABLE statement.
	if _, err := raw.Exec(`INSERT INTO virtual_keys
		(id, token, name, upstream, upstream_key,
		 daily_budget_usd, month_budget_usd, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"key_0000000000000001", "rein_live_legacy", "legacy",
		UpstreamOpenAI, "legacy-ciphertext-placeholder",
		0.0, 0.0, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw handle: %v", err)
	}

	// Now open via NewSQLite. Expect the migration to run ADD COLUMN and
	// for the column to exist afterward with the default empty string on
	// the legacy row.
	s, err := NewSQLite(path, mustAESGCM(t))
	if err != nil {
		t.Fatalf("open pre-0.2 db via NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	has, err := sqliteColumnExists(s.db, "virtual_keys", "upstream_base_url")
	if err != nil {
		t.Fatalf("column exists check: %v", err)
	}
	if !has {
		t.Fatal("migration did not add upstream_base_url column")
	}

	// The legacy row must now have an empty string for the new column
	// (NOT NULL DEFAULT '').
	var legacyBaseURL string
	if err := s.db.QueryRow(
		`SELECT upstream_base_url FROM virtual_keys WHERE id = ?`,
		"key_0000000000000001",
	).Scan(&legacyBaseURL); err != nil {
		t.Fatalf("select legacy row: %v", err)
	}
	if legacyBaseURL != "" {
		t.Errorf("legacy row upstream_base_url: got %q want empty", legacyBaseURL)
	}

	// A freshly created key on the migrated DB must be able to persist and
	// read back a non-empty UpstreamBaseURL.
	id, _ := GenerateID()
	token, _ := GenerateToken()
	vk := &VirtualKey{
		ID:              id,
		Token:           token,
		Name:            "post-migration",
		Upstream:        UpstreamOpenAI,
		UpstreamKey:     "sk-post",
		UpstreamBaseURL: "https://api.together.xyz",
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatalf("create post-migration key: %v", err)
	}
	got, err := s.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatalf("get post-migration key: %v", err)
	}
	if got.UpstreamBaseURL != "https://api.together.xyz" {
		t.Errorf("post-migration round-trip: got %q want https://api.together.xyz",
			got.UpstreamBaseURL)
	}
}

func TestSQLite_ColumnExistsGuard(t *testing.T) {
	s := newTestSQLite(t)
	has, err := sqliteColumnExists(s.db, "virtual_keys", "upstream_base_url")
	if err != nil {
		t.Fatalf("sqliteColumnExists: %v", err)
	}
	if !has {
		t.Errorf("fresh DB should have upstream_base_url column")
	}
	has, err = sqliteColumnExists(s.db, "virtual_keys", "nope")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Errorf("nonexistent column should return false")
	}
	if _, err := sqliteColumnExists(s.db, "virtual_keys; DROP", "id"); err == nil {
		t.Errorf("expected validation failure on bad table identifier")
	}
}

func TestSQLite_WrongCipherCannotDecrypt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wrong.db")
	c1 := mustAESGCM(t)
	s1, err := NewSQLite(path, c1)
	if err != nil {
		t.Fatal(err)
	}
	vk := mustCreate(t, s1, "x", UpstreamOpenAI, "sk-x")
	_ = s1.Close()

	c2 := mustAESGCM(t) // different random key
	s2, err := NewSQLite(path, c2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	if _, err := s2.GetByToken(context.Background(), vk.Token); err == nil {
		t.Error("expected decrypt failure when opening with a different encryption key")
	}
}
