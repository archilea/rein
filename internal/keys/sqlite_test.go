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

// TestSQLite_RateLimitFieldsRoundtrip verifies that a key's RPSLimit and
// RPMLimit fields persist through SQLite write and read, including both
// zero (unlimited) and non-zero values.
func TestSQLite_RateLimitFieldsRoundtrip(t *testing.T) {
	s := newTestSQLite(t)

	// Key with explicit rate limits set.
	id1, _ := GenerateID()
	token1, _ := GenerateToken()
	vk1 := &VirtualKey{
		ID:          id1,
		Token:       token1,
		Name:        "with-limits",
		Upstream:    UpstreamOpenAI,
		UpstreamKey: "sk-real",
		RPSLimit:    10,
		RPMLimit:    300,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk1); err != nil {
		t.Fatalf("create with limits: %v", err)
	}
	got1, err := s.GetByToken(context.Background(), token1)
	if err != nil {
		t.Fatalf("get with limits: %v", err)
	}
	if got1.RPSLimit != 10 {
		t.Errorf("RPSLimit roundtrip: got %d want 10", got1.RPSLimit)
	}
	if got1.RPMLimit != 300 {
		t.Errorf("RPMLimit roundtrip: got %d want 300", got1.RPMLimit)
	}

	// Key with zero limits (unlimited). Must also roundtrip as zero.
	id2, _ := GenerateID()
	token2, _ := GenerateToken()
	vk2 := &VirtualKey{
		ID:          id2,
		Token:       token2,
		Name:        "unlimited",
		Upstream:    UpstreamOpenAI,
		UpstreamKey: "sk-real",
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk2); err != nil {
		t.Fatalf("create unlimited: %v", err)
	}
	got2, err := s.GetByToken(context.Background(), token2)
	if err != nil {
		t.Fatalf("get unlimited: %v", err)
	}
	if got2.RPSLimit != 0 || got2.RPMLimit != 0 {
		t.Errorf("unlimited key: got RPS=%d RPM=%d want 0/0", got2.RPSLimit, got2.RPMLimit)
	}

	// List must return both keys with their rate limits intact.
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len: got %d want 2", len(list))
	}
	byID := map[string]*VirtualKey{}
	for _, k := range list {
		byID[k.ID] = k
	}
	if byID[id1].RPSLimit != 10 || byID[id1].RPMLimit != 300 {
		t.Errorf("list: key1 rate limits incorrect: RPS=%d RPM=%d",
			byID[id1].RPSLimit, byID[id1].RPMLimit)
	}
	if byID[id2].RPSLimit != 0 || byID[id2].RPMLimit != 0 {
		t.Errorf("list: key2 rate limits incorrect: RPS=%d RPM=%d",
			byID[id2].RPSLimit, byID[id2].RPMLimit)
	}
}

// TestSQLite_RateLimitMigrationIdempotent verifies that reopening a DB
// runs the rps_limit and rpm_limit migrations without error or duplicate
// columns.
func TestSQLite_RateLimitMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate-limit-migrate.db")
	c := mustAESGCM(t)
	s1, err := NewSQLite(path, c)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()

	// Reopen: migrations run again and must not fail.
	s2, err := NewSQLite(path, c)
	if err != nil {
		t.Fatalf("reopen after migration: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	// Confirm both columns are present by writing and reading values.
	id, _ := GenerateID()
	token, _ := GenerateToken()
	vk := &VirtualKey{
		ID:          id,
		Token:       token,
		Name:        "m",
		Upstream:    UpstreamOpenAI,
		UpstreamKey: "sk-m",
		RPSLimit:    5,
		RPMLimit:    250,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s2.Create(context.Background(), vk); err != nil {
		t.Fatalf("create after reopen: %v", err)
	}
	got, err := s2.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.RPSLimit != 5 || got.RPMLimit != 250 {
		t.Errorf("rate limits after reopen: got RPS=%d RPM=%d want 5/250",
			got.RPSLimit, got.RPMLimit)
	}
}

// TestSQLite_RateLimitMigrationFromPreExistingSchema exercises the
// !sqliteColumnExists branch of applySQLiteMigrations for rps_limit and
// rpm_limit by physically materializing a schema without those columns
// at a temp path, closing the raw handle, and then opening via NewSQLite.
// The migration must run ALTER TABLE ADD COLUMN for both columns and
// leave the DB in a state where a VirtualKey with rate limits round-trips
// correctly.
func TestSQLite_RateLimitMigrationFromPreExistingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-rate-limit.db")

	// Create a schema that has upstream_base_url but no rps_limit/rpm_limit.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		url.PathEscape(path))
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	const preRateLimitSchema = `
		CREATE TABLE virtual_keys (
			id                 TEXT PRIMARY KEY,
			token              TEXT NOT NULL UNIQUE,
			name               TEXT NOT NULL,
			upstream           TEXT NOT NULL,
			upstream_key       TEXT NOT NULL,
			daily_budget_usd   REAL NOT NULL DEFAULT 0,
			month_budget_usd   REAL NOT NULL DEFAULT 0,
			upstream_base_url  TEXT NOT NULL DEFAULT '',
			created_at         TEXT NOT NULL,
			revoked_at         TEXT
		);`
	if _, err := raw.Exec(preRateLimitSchema); err != nil {
		t.Fatalf("create pre-rate-limit schema: %v", err)
	}
	// Insert a legacy row using the column set that lacks rate limit columns.
	if _, err := raw.Exec(`INSERT INTO virtual_keys
		(id, token, name, upstream, upstream_key,
		 daily_budget_usd, month_budget_usd, upstream_base_url, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"key_0000000000000002", "rein_live_legacy_rl", "legacy-rl",
		UpstreamOpenAI, "legacy-ciphertext-placeholder",
		0.0, 0.0, "", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw handle: %v", err)
	}

	// Open via NewSQLite. Migration must add both columns.
	s, err := NewSQLite(path, mustAESGCM(t))
	if err != nil {
		t.Fatalf("open pre-rate-limit db via NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, col := range []string{"rps_limit", "rpm_limit"} {
		has, err := sqliteColumnExists(s.db, "virtual_keys", col)
		if err != nil {
			t.Fatalf("column exists check %s: %v", col, err)
		}
		if !has {
			t.Errorf("migration did not add %s column", col)
		}
	}

	// Legacy row must have default zero values for both new columns.
	var legacyRPS, legacyRPM int
	if err := s.db.QueryRow(
		`SELECT rps_limit, rpm_limit FROM virtual_keys WHERE id = ?`,
		"key_0000000000000002",
	).Scan(&legacyRPS, &legacyRPM); err != nil {
		t.Fatalf("select legacy row: %v", err)
	}
	if legacyRPS != 0 || legacyRPM != 0 {
		t.Errorf("legacy row rate limits: got RPS=%d RPM=%d want 0/0",
			legacyRPS, legacyRPM)
	}

	// A freshly created key on the migrated DB must persist non-zero limits.
	id, _ := GenerateID()
	token, _ := GenerateToken()
	vk := &VirtualKey{
		ID:          id,
		Token:       token,
		Name:        "post-migration-rl",
		Upstream:    UpstreamOpenAI,
		UpstreamKey: "sk-post",
		RPSLimit:    20,
		RPMLimit:    600,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatalf("create post-migration key: %v", err)
	}
	got, err := s.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatalf("get post-migration key: %v", err)
	}
	if got.RPSLimit != 20 || got.RPMLimit != 600 {
		t.Errorf("post-migration roundtrip: got RPS=%d RPM=%d want 20/600",
			got.RPSLimit, got.RPMLimit)
	}
}

// TestSQLite_MaxConcurrentRoundtrip verifies that MaxConcurrent persists
// through SQLite write and read for both unlimited and explicit caps.
func TestSQLite_MaxConcurrentRoundtrip(t *testing.T) {
	s := newTestSQLite(t)

	id1, _ := GenerateID()
	token1, _ := GenerateToken()
	vk1 := &VirtualKey{
		ID:            id1,
		Token:         token1,
		Name:          "capped",
		Upstream:      UpstreamOpenAI,
		UpstreamKey:   "sk-real",
		MaxConcurrent: 25,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk1); err != nil {
		t.Fatalf("create capped: %v", err)
	}
	got1, err := s.GetByToken(context.Background(), token1)
	if err != nil {
		t.Fatalf("get capped: %v", err)
	}
	if got1.MaxConcurrent != 25 {
		t.Errorf("MaxConcurrent roundtrip: got %d want 25", got1.MaxConcurrent)
	}

	id2, _ := GenerateID()
	token2, _ := GenerateToken()
	vk2 := &VirtualKey{
		ID:          id2,
		Token:       token2,
		Name:        "uncapped",
		Upstream:    UpstreamOpenAI,
		UpstreamKey: "sk-real",
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk2); err != nil {
		t.Fatalf("create uncapped: %v", err)
	}
	got2, err := s.GetByToken(context.Background(), token2)
	if err != nil {
		t.Fatalf("get uncapped: %v", err)
	}
	if got2.MaxConcurrent != 0 {
		t.Errorf("uncapped MaxConcurrent: got %d want 0", got2.MaxConcurrent)
	}

	list, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]*VirtualKey{}
	for _, k := range list {
		byID[k.ID] = k
	}
	if byID[id1].MaxConcurrent != 25 {
		t.Errorf("list: capped MaxConcurrent=%d want 25", byID[id1].MaxConcurrent)
	}
	if byID[id2].MaxConcurrent != 0 {
		t.Errorf("list: uncapped MaxConcurrent=%d want 0", byID[id2].MaxConcurrent)
	}
}

// TestSQLite_MaxConcurrentMigrationFromPreExistingSchema exercises the
// ADD COLUMN branch for a DB that existed before the max_concurrent
// column was introduced.
func TestSQLite_MaxConcurrentMigrationFromPreExistingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-max-concurrent.db")

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		url.PathEscape(path))
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	const preMaxConcurrentSchema = `
		CREATE TABLE virtual_keys (
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
	if _, err := raw.Exec(preMaxConcurrentSchema); err != nil {
		t.Fatalf("create pre-max-concurrent schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO virtual_keys
		(id, token, name, upstream, upstream_key,
		 daily_budget_usd, month_budget_usd, upstream_base_url,
		 rps_limit, rpm_limit, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"key_0000000000000003", "rein_live_legacy_mc", "legacy-mc",
		UpstreamOpenAI, "legacy-ciphertext-placeholder",
		0.0, 0.0, "", 0, 0, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw handle: %v", err)
	}

	s, err := NewSQLite(path, mustAESGCM(t))
	if err != nil {
		t.Fatalf("open pre-max-concurrent db via NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	has, err := sqliteColumnExists(s.db, "virtual_keys", "max_concurrent")
	if err != nil {
		t.Fatalf("column exists check: %v", err)
	}
	if !has {
		t.Fatal("migration did not add max_concurrent column")
	}

	var legacyMC int
	if err := s.db.QueryRow(
		`SELECT max_concurrent FROM virtual_keys WHERE id = ?`,
		"key_0000000000000003",
	).Scan(&legacyMC); err != nil {
		t.Fatalf("select legacy row: %v", err)
	}
	if legacyMC != 0 {
		t.Errorf("legacy row max_concurrent: got %d want 0", legacyMC)
	}

	// New key on migrated DB must persist a non-zero cap.
	id, _ := GenerateID()
	token, _ := GenerateToken()
	vk := &VirtualKey{
		ID:            id,
		Token:         token,
		Name:          "post-migration-mc",
		Upstream:      UpstreamOpenAI,
		UpstreamKey:   "sk-post",
		MaxConcurrent: 7,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatalf("create post-migration key: %v", err)
	}
	got, err := s.GetByToken(context.Background(), vk.Token)
	if err != nil {
		t.Fatalf("get post-migration key: %v", err)
	}
	if got.MaxConcurrent != 7 {
		t.Errorf("post-migration roundtrip: got %d want 7", got.MaxConcurrent)
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

// --- Update tests ---

func TestSQLite_UpdatePartial(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "original", UpstreamOpenAI, "sk-x")
	vk.DailyBudgetUSD = 100
	// mustCreate doesn't set budgets, so patch them in directly via SQL for setup.
	_, err := s.db.Exec(`UPDATE virtual_keys SET daily_budget_usd=100, month_budget_usd=500, rps_limit=10, max_concurrent=20 WHERE id=?`, vk.ID)
	if err != nil {
		t.Fatal(err)
	}

	newName := "renamed"
	newBudget := 200.0
	updated, err := s.Update(context.Background(), vk.ID, KeyPatch{
		Name:           &newName,
		DailyBudgetUSD: &newBudget,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "renamed" {
		t.Errorf("name: got %q want renamed", updated.Name)
	}
	if updated.DailyBudgetUSD != 200 {
		t.Errorf("daily_budget_usd: got %v want 200", updated.DailyBudgetUSD)
	}
	// Unpatched fields preserved.
	if updated.MonthBudgetUSD != 500 {
		t.Errorf("month_budget_usd: got %v want 500 (preserved)", updated.MonthBudgetUSD)
	}
	if updated.RPSLimit != 10 {
		t.Errorf("rps_limit: got %d want 10 (preserved)", updated.RPSLimit)
	}
	if updated.MaxConcurrent != 20 {
		t.Errorf("max_concurrent: got %d want 20 (preserved)", updated.MaxConcurrent)
	}

	// Verify it persisted by re-reading.
	got, err := s.GetByID(context.Background(), vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.DailyBudgetUSD != 200 {
		t.Errorf("persisted: name=%q daily=%v", got.Name, got.DailyBudgetUSD)
	}
}

func TestSQLite_UpdateZeroMeansUnlimited(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "capped", UpstreamOpenAI, "sk-x")
	_, err := s.db.Exec(`UPDATE virtual_keys SET daily_budget_usd=100, rps_limit=10 WHERE id=?`, vk.ID)
	if err != nil {
		t.Fatal(err)
	}

	zeroBudget := 0.0
	zeroRPS := 0
	updated, err := s.Update(context.Background(), vk.ID, KeyPatch{
		DailyBudgetUSD: &zeroBudget,
		RPSLimit:       &zeroRPS,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DailyBudgetUSD != 0 {
		t.Errorf("daily_budget_usd: got %v want 0", updated.DailyBudgetUSD)
	}
	if updated.RPSLimit != 0 {
		t.Errorf("rps_limit: got %d want 0", updated.RPSLimit)
	}
}

func TestSQLite_UpdateNotFound(t *testing.T) {
	s := newTestSQLite(t)
	name := "new"
	_, err := s.Update(context.Background(), "key_0000000000000000", KeyPatch{Name: &name})
	if err != ErrNotFound {
		t.Errorf("got %v want ErrNotFound", err)
	}
}

func TestSQLite_UpdateRevokedKeyRejected(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "soon-revoked", UpstreamOpenAI, "sk-x")
	if err := s.Revoke(context.Background(), vk.ID); err != nil {
		t.Fatal(err)
	}

	name := "new-name"
	_, err := s.Update(context.Background(), vk.ID, KeyPatch{Name: &name})
	if err != ErrRevoked {
		t.Errorf("got %v want ErrRevoked", err)
	}
}

func TestSQLite_UpdateEmptyPatchNoOp(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "stable", UpstreamOpenAI, "sk-x")

	updated, err := s.Update(context.Background(), vk.ID, KeyPatch{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "stable" {
		t.Errorf("name changed on empty patch: got %q", updated.Name)
	}
}

func TestSQLite_UpdateUpstreamBaseURL(t *testing.T) {
	s := newTestSQLite(t)
	id, _ := GenerateID()
	token, _ := GenerateToken()
	vk := &VirtualKey{
		ID:              id,
		Token:           token,
		Name:            "groq",
		Upstream:        UpstreamOpenAI,
		UpstreamKey:     "gsk-x",
		UpstreamBaseURL: "https://api.groq.com",
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}

	newURL := "https://api.fireworks.ai"
	updated, err := s.Update(context.Background(), id, KeyPatch{UpstreamBaseURL: &newURL})
	if err != nil {
		t.Fatal(err)
	}
	if updated.UpstreamBaseURL != "https://api.fireworks.ai" {
		t.Errorf("upstream_base_url: got %q", updated.UpstreamBaseURL)
	}

	// Verify persistence.
	got, err := s.GetByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpstreamBaseURL != "https://api.fireworks.ai" {
		t.Errorf("persisted upstream_base_url: got %q", got.UpstreamBaseURL)
	}
}

func TestMemory_UpdatePartial(t *testing.T) {
	m := NewMemory()
	vk := mustCreate(t, m, "original", UpstreamOpenAI, "sk-x")

	newName := "renamed"
	updated, err := m.Update(context.Background(), vk.ID, KeyPatch{Name: &newName})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "renamed" {
		t.Errorf("name: got %q", updated.Name)
	}

	// Verify the returned copy is independent.
	updated.Name = "tampered"
	got, _ := m.GetByID(context.Background(), vk.ID)
	if got.Name != "renamed" {
		t.Errorf("store was mutated by caller: got %q", got.Name)
	}
}

func TestMemory_UpdateAllFields(t *testing.T) {
	m := NewMemory()
	vk := mustCreate(t, m, "original", UpstreamOpenAI, "sk-x")

	newName := "new-name"
	newDaily := 10.0
	newMonth := 200.0
	newRPS := 5
	newRPM := 100
	newMC := 3
	newURL := "https://api.groq.com"

	updated, err := m.Update(context.Background(), vk.ID, KeyPatch{
		Name:            &newName,
		DailyBudgetUSD:  &newDaily,
		MonthBudgetUSD:  &newMonth,
		RPSLimit:        &newRPS,
		RPMLimit:        &newRPM,
		MaxConcurrent:   &newMC,
		UpstreamBaseURL: &newURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "new-name" {
		t.Errorf("Name: got %q", updated.Name)
	}
	if updated.DailyBudgetUSD != 10 {
		t.Errorf("DailyBudgetUSD: got %v", updated.DailyBudgetUSD)
	}
	if updated.MonthBudgetUSD != 200 {
		t.Errorf("MonthBudgetUSD: got %v", updated.MonthBudgetUSD)
	}
	if updated.RPSLimit != 5 {
		t.Errorf("RPSLimit: got %d", updated.RPSLimit)
	}
	if updated.RPMLimit != 100 {
		t.Errorf("RPMLimit: got %d", updated.RPMLimit)
	}
	if updated.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent: got %d", updated.MaxConcurrent)
	}
	if updated.UpstreamBaseURL != "https://api.groq.com" {
		t.Errorf("UpstreamBaseURL: got %q", updated.UpstreamBaseURL)
	}
}

func TestMemory_UpdateNotFound(t *testing.T) {
	m := NewMemory()
	name := "x"
	_, err := m.Update(context.Background(), "key_0000000000000000", KeyPatch{Name: &name})
	if err != ErrNotFound {
		t.Errorf("got %v want ErrNotFound", err)
	}
}

func TestMemory_UpdateRevokedKeyRejected(t *testing.T) {
	m := NewMemory()
	vk := mustCreate(t, m, "soon-revoked", UpstreamOpenAI, "sk-x")
	if err := m.Revoke(context.Background(), vk.ID); err != nil {
		t.Fatal(err)
	}
	name := "new"
	_, err := m.Update(context.Background(), vk.ID, KeyPatch{Name: &name})
	if err != ErrRevoked {
		t.Errorf("got %v want ErrRevoked", err)
	}
}
