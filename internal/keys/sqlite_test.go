package keys

import (
	"context"
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
