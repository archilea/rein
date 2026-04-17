package keys

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func TestVirtualKey_IsExpired(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	future := now.Add(time.Minute)

	cases := []struct {
		name string
		key  *VirtualKey
		want bool
	}{
		{"nil receiver", nil, false},
		{"no expiry", &VirtualKey{ExpiresAt: nil}, false},
		{"future expiry", &VirtualKey{ExpiresAt: &future}, false},
		{"exactly now", &VirtualKey{ExpiresAt: &now}, true},
		{"past expiry", &VirtualKey{ExpiresAt: &past}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.key.IsExpired(now); got != tc.want {
				t.Errorf("IsExpired: got %v want %v", got, tc.want)
			}
		})
	}
}

func TestKeyPatch_ApplyTo_ExpiresAt(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	newer := now.Add(2 * time.Hour)

	t.Run("unchanged when both zero", func(t *testing.T) {
		k := &VirtualKey{ExpiresAt: &future}
		KeyPatch{}.ApplyTo(k)
		if k.ExpiresAt == nil || !k.ExpiresAt.Equal(future) {
			t.Errorf("expected expires_at preserved, got %v", k.ExpiresAt)
		}
	})

	t.Run("set overwrites nil", func(t *testing.T) {
		k := &VirtualKey{ExpiresAt: nil}
		KeyPatch{ExpiresAt: &newer}.ApplyTo(k)
		if k.ExpiresAt == nil || !k.ExpiresAt.Equal(newer.UTC()) {
			t.Errorf("expected %v, got %v", newer, k.ExpiresAt)
		}
	})

	t.Run("set overwrites existing", func(t *testing.T) {
		k := &VirtualKey{ExpiresAt: &future}
		KeyPatch{ExpiresAt: &newer}.ApplyTo(k)
		if !k.ExpiresAt.Equal(newer.UTC()) {
			t.Errorf("expected overwrite to %v, got %v", newer, k.ExpiresAt)
		}
	})

	t.Run("clear removes existing", func(t *testing.T) {
		k := &VirtualKey{ExpiresAt: &future}
		KeyPatch{ClearExpiresAt: true}.ApplyTo(k)
		if k.ExpiresAt != nil {
			t.Errorf("expected nil after clear, got %v", k.ExpiresAt)
		}
	})

	t.Run("clear wins over set", func(t *testing.T) {
		k := &VirtualKey{ExpiresAt: &future}
		KeyPatch{ExpiresAt: &newer, ClearExpiresAt: true}.ApplyTo(k)
		if k.ExpiresAt != nil {
			t.Errorf("clear must take precedence, got %v", k.ExpiresAt)
		}
	})
}

func TestMemory_ExpiresAtRoundTrip(t *testing.T) {
	m := NewMemory()
	id, _ := GenerateID()
	token, _ := GenerateToken()
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Nanosecond)
	vk := &VirtualKey{
		ID: id, Token: token, Name: "temp",
		Upstream: UpstreamOpenAI, UpstreamKey: "sk-x",
		CreatedAt: time.Now().UTC(), ExpiresAt: &expires,
	}
	if err := m.Create(context.Background(), vk); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := m.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt roundtrip: got %v want %v", got.ExpiresAt, expires)
	}
}

func TestMemory_ListExpiring(t *testing.T) {
	m := NewMemory()
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)

	// Fixture: A expired+unrevoked, B expired+revoked, C future, D no expiry.
	mustInsert := func(name string, expiresAt, revokedAt *time.Time) string {
		id, _ := GenerateID()
		token, _ := GenerateToken()
		vk := &VirtualKey{
			ID: id, Token: token, Name: name,
			Upstream: UpstreamOpenAI, UpstreamKey: "sk-x",
			CreatedAt: now, ExpiresAt: expiresAt, RevokedAt: revokedAt,
		}
		if err := m.Create(context.Background(), vk); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return id
	}
	aID := mustInsert("A", &past, nil)
	_ = mustInsert("B", &past, &past)
	_ = mustInsert("C", &future, nil)
	_ = mustInsert("D", nil, nil)

	got, err := m.ListExpiring(context.Background(), now)
	if err != nil {
		t.Fatalf("ListExpiring: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListExpiring: got %d want 1", len(got))
	}
	if got[0].ID != aID {
		t.Errorf("expected %q (expired+unrevoked), got %q", aID, got[0].ID)
	}
}

func TestMemory_RevokeAt(t *testing.T) {
	m := NewMemory()
	vk := mustCreate(t, m, "soon", UpstreamOpenAI, "sk-x")
	expiry := time.Date(2026, 4, 17, 10, 0, 0, 0, time.UTC)

	if err := m.RevokeAt(context.Background(), vk.ID, expiry); err != nil {
		t.Fatalf("RevokeAt: %v", err)
	}
	got, _ := m.GetByID(context.Background(), vk.ID)
	if got.RevokedAt == nil || !got.RevokedAt.Equal(expiry) {
		t.Errorf("RevokedAt: got %v want %v", got.RevokedAt, expiry)
	}

	// Second RevokeAt is idempotent; must not overwrite the first stamp.
	laterExpiry := expiry.Add(time.Hour)
	if err := m.RevokeAt(context.Background(), vk.ID, laterExpiry); err != nil {
		t.Fatalf("second RevokeAt: %v", err)
	}
	got, _ = m.GetByID(context.Background(), vk.ID)
	if !got.RevokedAt.Equal(expiry) {
		t.Errorf("idempotent RevokeAt: got %v, existing stamp must win", got.RevokedAt)
	}
}

func TestMemory_RevokeAt_NotFound(t *testing.T) {
	m := NewMemory()
	err := m.RevokeAt(context.Background(), "key_missing", time.Now().UTC())
	if err != ErrNotFound {
		t.Errorf("got %v want ErrNotFound", err)
	}
}

func TestSQLite_ExpiresAtRoundTrip(t *testing.T) {
	s := newTestSQLite(t)
	id, _ := GenerateID()
	token, _ := GenerateToken()
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	vk := &VirtualKey{
		ID: id, Token: token, Name: "temp",
		Upstream: UpstreamOpenAI, UpstreamKey: "sk-x",
		CreatedAt: time.Now().UTC(), ExpiresAt: &expires,
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt roundtrip: got %v want %v", got.ExpiresAt, expires)
	}

	all, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ExpiresAt == nil || !all[0].ExpiresAt.Equal(expires) {
		t.Errorf("list roundtrip: got %+v", all)
	}
}

func TestSQLite_ExpiresAtOmitted(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "plain", UpstreamOpenAI, "sk-x")
	got, _ := s.GetByID(context.Background(), vk.ID)
	if got.ExpiresAt != nil {
		t.Errorf("ExpiresAt for key without expiry: got %v want nil", got.ExpiresAt)
	}
}

func TestSQLite_ListExpiring(t *testing.T) {
	s := newTestSQLite(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)

	mkKey := func(name string, expiresAt *time.Time) string {
		id, _ := GenerateID()
		token, _ := GenerateToken()
		vk := &VirtualKey{
			ID: id, Token: token, Name: name,
			Upstream: UpstreamOpenAI, UpstreamKey: "sk-x",
			CreatedAt: now, ExpiresAt: expiresAt,
		}
		if err := s.Create(context.Background(), vk); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return id
	}

	aID := mkKey("A-expired", &past)
	_ = mkKey("B-future", &future)
	_ = mkKey("C-no-expiry", nil)
	dID := mkKey("D-expired-and-revoked", &past)
	if err := s.Revoke(context.Background(), dID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListExpiring(context.Background(), now)
	if err != nil {
		t.Fatalf("ListExpiring: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListExpiring: got %d rows want 1", len(got))
	}
	if got[0].ID != aID {
		t.Errorf("ListExpiring: got %q want %q", got[0].ID, aID)
	}
}

func TestSQLite_RevokeAt(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "soon", UpstreamOpenAI, "sk-x")
	expiry := time.Date(2026, 4, 17, 10, 0, 0, 0, time.UTC)

	if err := s.RevokeAt(context.Background(), vk.ID, expiry); err != nil {
		t.Fatalf("RevokeAt: %v", err)
	}
	got, err := s.GetByID(context.Background(), vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RevokedAt == nil || !got.RevokedAt.Equal(expiry) {
		t.Errorf("RevokedAt: got %v want %v", got.RevokedAt, expiry)
	}

	// Idempotent: a second RevokeAt must not overwrite the first stamp.
	later := expiry.Add(time.Hour)
	if err := s.RevokeAt(context.Background(), vk.ID, later); err != nil {
		t.Fatalf("second RevokeAt: %v", err)
	}
	got, _ = s.GetByID(context.Background(), vk.ID)
	if !got.RevokedAt.Equal(expiry) {
		t.Errorf("idempotent RevokeAt: got %v, first stamp must win", got.RevokedAt)
	}
}

func TestSQLite_RevokeAt_NotFound(t *testing.T) {
	s := newTestSQLite(t)
	err := s.RevokeAt(context.Background(), "key_0000000000000001", time.Now().UTC())
	if err != ErrNotFound {
		t.Errorf("got %v want ErrNotFound", err)
	}
}

func TestSQLite_UpdateSetsAndClearsExpiresAt(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "nochange", UpstreamOpenAI, "sk-x")

	future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Millisecond)
	updated, err := s.Update(context.Background(), vk.ID, KeyPatch{ExpiresAt: &future})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(future) {
		t.Errorf("set result: got %v want %v", updated.ExpiresAt, future)
	}
	persisted, _ := s.GetByID(context.Background(), vk.ID)
	if !persisted.ExpiresAt.Equal(future) {
		t.Errorf("persisted set: got %v", persisted.ExpiresAt)
	}

	cleared, err := s.Update(context.Background(), vk.ID, KeyPatch{ClearExpiresAt: true})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.ExpiresAt != nil {
		t.Errorf("clear result: got %v want nil", cleared.ExpiresAt)
	}
	persisted, _ = s.GetByID(context.Background(), vk.ID)
	if persisted.ExpiresAt != nil {
		t.Errorf("persisted clear: got %v want nil", persisted.ExpiresAt)
	}
}

// TestSQLite_ListExpiring_ScanKeyError covers the rows.Next+scanKey error
// branch of ListExpiring. We inject a row with an intentionally malformed
// expires_at value via raw SQL so scanKey's time.Parse fails during
// iteration, and ListExpiring must surface that error rather than
// silently yielding a partial result.
func TestSQLite_ListExpiring_ScanKeyError(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "corrupted", UpstreamOpenAI, "sk-x")

	// Stamp a bogus expires_at that (a) lexically sorts before any
	// current-year RFC3339 timestamp so the TEXT <= TEXT WHERE clause
	// picks the row up, and (b) fails time.Parse in scanKey. The
	// leading "0" keeps the lex-order check honest ("0..." < "2026...").
	if _, err := s.db.Exec(
		`UPDATE virtual_keys SET expires_at = ? WHERE id = ?`,
		"0000-invalid-rfc3339", vk.ID,
	); err != nil {
		t.Fatal(err)
	}

	_, err := s.ListExpiring(context.Background(), time.Now().UTC())
	if err == nil {
		t.Fatal("expected ListExpiring to surface scanKey parse error")
	}
}

// TestSQLite_RevokeAt_DoesNotOverwriteManualRevoke protects the audit
// trail invariant: once an operator has manually revoked a key, a
// subsequent sweeper tick that stamps revoked_at = expires_at must NOT
// overwrite the manual revoked_at. Losing the manual stamp would lie
// about the reason for revocation.
func TestSQLite_RevokeAt_DoesNotOverwriteManualRevoke(t *testing.T) {
	s := newTestSQLite(t)
	vk := mustCreate(t, s, "manual-first", UpstreamOpenAI, "sk-x")

	if err := s.Revoke(context.Background(), vk.ID); err != nil {
		t.Fatal(err)
	}
	manual, err := s.GetByID(context.Background(), vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	manualStamp := *manual.RevokedAt

	// Sweeper happens later with a different at value. The WHERE clause
	// "revoked_at IS NULL" must guard the write.
	laterStamp := manualStamp.Add(time.Hour)
	if err := s.RevokeAt(context.Background(), vk.ID, laterStamp); err != nil {
		t.Fatalf("RevokeAt after manual revoke: %v", err)
	}
	got, _ := s.GetByID(context.Background(), vk.ID)
	if !got.RevokedAt.Equal(manualStamp) {
		t.Errorf("manual revoked_at overwritten: got %v want %v", got.RevokedAt, manualStamp)
	}
}

// TestSQLite_Update_PreservesExpiresAtAcrossUnrelatedPatch catches the
// regression where a PATCH touching only name would clobber the
// previously-set expires_at (e.g., if the UPDATE SET clause dropped the
// expires_at column or ApplyTo mishandled the unchanged branch).
func TestSQLite_Update_PreservesExpiresAtAcrossUnrelatedPatch(t *testing.T) {
	s := newTestSQLite(t)
	id, _ := GenerateID()
	token, _ := GenerateToken()
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	vk := &VirtualKey{
		ID: id, Token: token, Name: "pre-patch",
		Upstream: UpstreamOpenAI, UpstreamKey: "sk-x",
		CreatedAt: time.Now().UTC(), ExpiresAt: &expires,
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}

	newName := "post-patch"
	updated, err := s.Update(context.Background(), id, KeyPatch{Name: &newName})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "post-patch" {
		t.Errorf("name not updated: got %q", updated.Name)
	}
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at not preserved: got %v want %v", updated.ExpiresAt, expires)
	}
	// Verify durability, not just the returned copy.
	persisted, _ := s.GetByID(context.Background(), id)
	if persisted.ExpiresAt == nil || !persisted.ExpiresAt.Equal(expires) {
		t.Errorf("persisted expires_at changed: got %v want %v", persisted.ExpiresAt, expires)
	}
}

// TestSQLite_ExpiresAtMigrationIdempotent reopens a freshly created DB so
// the idempotent-migration branch fires once without adding expires_at twice.
func TestSQLite_ExpiresAtMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expiry-migrate.db")
	c := mustAESGCM(t)
	s1, err := NewSQLite(path, c)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()

	s2, err := NewSQLite(path, c)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	id, _ := GenerateID()
	token, _ := GenerateToken()
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	vk := &VirtualKey{
		ID: id, Token: token, Name: "m", Upstream: UpstreamOpenAI,
		UpstreamKey: "sk-m", CreatedAt: time.Now().UTC(), ExpiresAt: &future,
	}
	if err := s2.Create(context.Background(), vk); err != nil {
		t.Fatalf("create after reopen: %v", err)
	}
	got, err := s2.GetByToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(future) {
		t.Errorf("expires_at after reopen: got %v want %v", got.ExpiresAt, future)
	}
}

// TestSQLite_ExpiresAtMigrationFromPreExistingSchema physically materializes
// a schema without expires_at and confirms NewSQLite adds the column cleanly.
func TestSQLite_ExpiresAtMigrationFromPreExistingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-expires.db")

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		url.PathEscape(path))
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	const preExpiresSchema = `
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
			max_concurrent     INTEGER NOT NULL DEFAULT 0,
			created_at         TEXT NOT NULL,
			revoked_at         TEXT
		);`
	if _, err := raw.Exec(preExpiresSchema); err != nil {
		t.Fatalf("create pre-expires schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO virtual_keys
		(id, token, name, upstream, upstream_key,
		 daily_budget_usd, month_budget_usd, upstream_base_url,
		 rps_limit, rpm_limit, max_concurrent, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"key_0000000000000aaa", "rein_live_legacy_exp", "legacy-exp",
		UpstreamOpenAI, "legacy-ciphertext-placeholder",
		0.0, 0.0, "", 0, 0, 0, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := NewSQLite(path, mustAESGCM(t))
	if err != nil {
		t.Fatalf("open pre-expires db via NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	has, err := sqliteColumnExists(s.db, "virtual_keys", "expires_at")
	if err != nil {
		t.Fatalf("column exists check: %v", err)
	}
	if !has {
		t.Fatal("migration did not add expires_at column")
	}

	// Legacy row must have NULL expires_at (default).
	var legacyExp sql.NullString
	if err := s.db.QueryRow(
		`SELECT expires_at FROM virtual_keys WHERE id = ?`,
		"key_0000000000000aaa",
	).Scan(&legacyExp); err != nil {
		t.Fatalf("select legacy row: %v", err)
	}
	if legacyExp.Valid {
		t.Errorf("legacy row expires_at: got %q want NULL", legacyExp.String)
	}

	// A freshly created key on the migrated DB must persist expires_at.
	id, _ := GenerateID()
	token, _ := GenerateToken()
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	vk := &VirtualKey{
		ID: id, Token: token, Name: "post-migration-exp",
		Upstream: UpstreamOpenAI, UpstreamKey: "sk-post",
		CreatedAt: time.Now().UTC(), ExpiresAt: &future,
	}
	if err := s.Create(context.Background(), vk); err != nil {
		t.Fatalf("create post-migration: %v", err)
	}
	got, err := s.GetByToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(future) {
		t.Errorf("post-migration roundtrip: got %v want %v", got.ExpiresAt, future)
	}
}
