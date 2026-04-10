package keys

import (
	"crypto/rand"
	"strings"
	"testing"
)

func mustAESGCM(t *testing.T) Cipher {
	t.Helper()
	key := make([]byte, AESKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := NewAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAESGCM_RoundTrip(t *testing.T) {
	c := mustAESGCM(t)
	plain := "sk-real-openai-secret"
	ct, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ct, "v1:") {
		t.Errorf("expected v1: tag, got %q", ct)
	}
	if strings.Contains(ct, plain) {
		t.Errorf("ciphertext contains plaintext: %q", ct)
	}
	got, err := c.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Errorf("round-trip: got %q want %q", got, plain)
	}
}

func TestAESGCM_NonceUniqueness(t *testing.T) {
	c := mustAESGCM(t)
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if a == b {
		t.Errorf("two encryptions of the same plaintext produced identical ciphertexts; nonce reuse?")
	}
}

func TestAESGCM_WrongKeyFails(t *testing.T) {
	c1 := mustAESGCM(t)
	c2 := mustAESGCM(t)
	ct, _ := c1.Encrypt("hello")
	if _, err := c2.Decrypt(ct); err == nil {
		t.Fatal("expected decryption with wrong key to fail")
	}
}

func TestAESGCM_TamperedCiphertextFails(t *testing.T) {
	c := mustAESGCM(t)
	ct, _ := c.Encrypt("hello")
	tampered := ct[:len(ct)-2] + "aa"
	if _, err := c.Decrypt(tampered); err == nil {
		t.Error("expected tampered ciphertext to fail auth check")
	}
}

func TestAESGCM_BadKeySize(t *testing.T) {
	if _, err := NewAESGCM(make([]byte, 16)); err == nil {
		t.Error("expected error for 16-byte key")
	}
	if _, err := NewAESGCM(nil); err == nil {
		t.Error("expected error for nil key")
	}
}

func TestDecodeHexKey(t *testing.T) {
	_, err := DecodeHexKey(strings.Repeat("a", 64))
	if err != nil {
		t.Errorf("valid hex rejected: %v", err)
	}
	if _, err := DecodeHexKey("too short"); err == nil {
		t.Error("expected error on short hex")
	}
	if _, err := DecodeHexKey(strings.Repeat("z", 64)); err == nil {
		t.Error("expected error on non-hex chars")
	}
}

func TestNoopCipher_RejectsV1Ciphertext(t *testing.T) {
	n := NoopCipher()
	if _, err := n.Decrypt("v1:anything"); err == nil {
		t.Error("noop cipher should refuse to pass through v1-tagged data")
	}
	if got, _ := n.Encrypt("plain"); got != "plain" {
		t.Errorf("noop encrypt: got %q want plain", got)
	}
}
