package keys

import (
	"context"
	"crypto/rand"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// newTestCipher returns an AES-256-GCM Cipher with a freshly random key and
// the raw key bytes. Tests that need to flip between old and new ciphers
// use this to derive two independent ciphers.
func newTestCipher(t *testing.T) Cipher {
	t.Helper()
	k := make([]byte, AESKeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	c, err := NewAESGCM(k)
	if err != nil {
		t.Fatalf("new aes: %v", err)
	}
	return c
}

// seedKeys mints n virtual keys under the given cipher-backed store and
// returns their plaintext upstream keys keyed by row id. Tests use the
// returned map to assert that plaintext survives rotation end to end.
func seedKeys(t *testing.T, s *SQLite, n int) map[string]string {
	t.Helper()
	out := make(map[string]string, n)
	for i := 0; i < n; i++ {
		plain := "sk-real-" + strings.Repeat("x", 40)
		// Add an index so each plaintext is unique; rotation must
		// preserve per-row identity, not just any-plaintext.
		plain = plain[:len(plain)-2] + string(rune('a'+i%26)) + string(rune('0'+i%10))
		vk := mustCreate(t, s, "seed", UpstreamOpenAI, plain)
		out[vk.ID] = plain
	}
	return out
}

func TestRotateEncryption_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rein.db")

	oldC := newTestCipher(t)
	newC := newTestCipher(t)

	sOld, err := NewSQLite(path, oldC)
	if err != nil {
		t.Fatalf("open (old): %v", err)
	}
	originals := seedKeys(t, sOld, 5)
	_ = sOld.Close()

	res, err := RotateEncryption(context.Background(), path, oldC, newC)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if res.Rotated != 5 || res.Skipped != 0 {
		t.Fatalf("summary: got rotated=%d skipped=%d want rotated=5 skipped=0", res.Rotated, res.Skipped)
	}

	sNew, err := NewSQLite(path, newC)
	if err != nil {
		t.Fatalf("open (new): %v", err)
	}
	defer func() { _ = sNew.Close() }()
	all, err := sNew.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("row count: got %d want 5", len(all))
	}
	for _, k := range all {
		want, ok := originals[k.ID]
		if !ok {
			t.Errorf("unexpected id %s", k.ID)
			continue
		}
		if k.UpstreamKey != want {
			t.Errorf("plaintext drift on %s: got %q want %q", k.ID, k.UpstreamKey, want)
		}
	}
}

func TestRotateEncryption_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rein.db")
	oldC := newTestCipher(t)
	newC := newTestCipher(t)

	s, err := NewSQLite(path, oldC)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = seedKeys(t, s, 3)
	_ = s.Close()

	res1, err := RotateEncryption(context.Background(), path, oldC, newC)
	if err != nil {
		t.Fatalf("first rotate: %v", err)
	}
	if res1.Rotated != 3 || res1.Skipped != 0 {
		t.Fatalf("first: got rotated=%d skipped=%d want rotated=3 skipped=0", res1.Rotated, res1.Skipped)
	}

	// Second run with the SAME old/new pair: every row is already on the
	// new cipher, so none should need rotating. No error, no DB changes.
	res2, err := RotateEncryption(context.Background(), path, oldC, newC)
	if err != nil {
		t.Fatalf("second rotate: %v", err)
	}
	if res2.Rotated != 0 || res2.Skipped != 3 {
		t.Fatalf("second: got rotated=%d skipped=%d want rotated=0 skipped=3", res2.Rotated, res2.Skipped)
	}
}

func TestRotateEncryption_WrongOldKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rein.db")
	realOld := newTestCipher(t)
	wrongOld := newTestCipher(t)
	newC := newTestCipher(t)

	s, err := NewSQLite(path, realOld)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	originals := seedKeys(t, s, 2)
	_ = s.Close()

	_, err = RotateEncryption(context.Background(), path, wrongOld, newC)
	if err == nil {
		t.Fatal("expected error when old-key does not match the DB, got nil")
	}

	// DB must be untouched: reopening with the REAL old cipher must still
	// recover the original plaintexts.
	sCheck, err := NewSQLite(path, realOld)
	if err != nil {
		t.Fatalf("reopen after failed rotate: %v", err)
	}
	defer func() { _ = sCheck.Close() }()
	all, err := sCheck.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, k := range all {
		want := originals[k.ID]
		if k.UpstreamKey != want {
			t.Errorf("plaintext changed on failed rotate for %s: got %q want %q", k.ID, k.UpstreamKey, want)
		}
	}
}

func TestRotateEncryption_EmptyKeystore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rein.db")
	oldC := newTestCipher(t)
	newC := newTestCipher(t)

	// Initialize the schema but insert nothing.
	s, err := NewSQLite(path, oldC)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()

	res, err := RotateEncryption(context.Background(), path, oldC, newC)
	if err != nil {
		t.Fatalf("rotate empty: %v", err)
	}
	if res.Rotated != 0 || res.Skipped != 0 {
		t.Fatalf("empty keystore: got rotated=%d skipped=%d want 0/0", res.Rotated, res.Skipped)
	}
}

func TestRotateEncryption_MidBatchFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rein.db")
	realOld := newTestCipher(t)
	wrongOld := newTestCipher(t)
	newC := newTestCipher(t)

	// Seed two rows with the real old key, then insert a third row whose
	// ciphertext belongs to a different cipher entirely. The rotation
	// planner will succeed on rows 1 and 2 but fail on row 3, and must
	// roll back without touching rows 1 and 2.
	s, err := NewSQLite(path, realOld)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	originals := seedKeys(t, s, 2)
	_ = s.Close()

	// Directly overwrite one row's upstream_key with a ciphertext encrypted
	// under a key the rotator will not be given, simulating a corrupt or
	// alien row in the middle of the table.
	rawDB, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	alienCT, err := wrongOld.Encrypt("alien-plaintext")
	if err != nil {
		t.Fatalf("encrypt alien: %v", err)
	}
	// Insert the alien row directly to bypass the keystore validation.
	if _, err := rawDB.Exec(
		`INSERT INTO virtual_keys (id, token, name, upstream, upstream_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"key_alien0000000", "rein_live_alien", "alien", UpstreamOpenAI, alienCT,
		"2026-04-15T00:00:00Z",
	); err != nil {
		t.Fatalf("insert alien: %v", err)
	}
	_ = rawDB.Close()

	_, err = RotateEncryption(context.Background(), path, realOld, newC)
	if err == nil {
		t.Fatal("expected error on mid-batch failure, got nil")
	}

	// Original rows must still decrypt under the old cipher: the alien row
	// aborted the rotate, so none of the happy-path rows were updated.
	sCheck, err := NewSQLite(path, realOld)
	if err != nil {
		t.Fatalf("reopen after failed rotate: %v", err)
	}
	defer func() { _ = sCheck.Close() }()
	all, err := sCheck.List(context.Background())
	if err != nil {
		// List decrypts every row; the alien row will surface as a decrypt
		// error here, which is fine. We only care that the well-formed rows
		// are still recoverable.
		if !strings.Contains(err.Error(), "decrypt") {
			t.Fatalf("unexpected list error: %v", err)
		}
	}
	for _, k := range all {
		want, ok := originals[k.ID]
		if !ok {
			continue // alien row
		}
		if k.UpstreamKey != want {
			t.Errorf("plaintext changed on failed rotate for %s: got %q want %q", k.ID, k.UpstreamKey, want)
		}
	}
}

func TestRotateEncryption_NilArgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rein.db")
	c := newTestCipher(t)
	if _, err := RotateEncryption(context.Background(), "", c, c); err == nil {
		t.Error("empty path should error")
	}
	if _, err := RotateEncryption(context.Background(), path, nil, c); err == nil {
		t.Error("nil old cipher should error")
	}
	if _, err := RotateEncryption(context.Background(), path, c, nil); err == nil {
		t.Error("nil new cipher should error")
	}
}

func TestRotateEncryption_NoSecretsInError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rein.db")
	realOld := newTestCipher(t)
	wrongOld := newTestCipher(t)
	newC := newTestCipher(t)

	s, err := NewSQLite(path, realOld)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	secret := "sk-topsecret-plaintext-donotleak"
	vk := mustCreate(t, s, "prod", UpstreamOpenAI, secret)
	_ = sNewMustList(t, s)
	_ = s.Close()
	_ = vk

	_, err = RotateEncryption(context.Background(), path, wrongOld, newC)
	if err == nil {
		t.Fatal("expected wrong-key error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaks plaintext: %v", err)
	}
}

// sNewMustList is a tiny helper that exercises the read path so the test name
// carries its intent (verify the keystore is usable before we try to rotate).
func sNewMustList(t *testing.T, s *SQLite) []*VirtualKey {
	t.Helper()
	all, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return all
}
