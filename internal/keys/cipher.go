package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Cipher encrypts and decrypts sensitive field values (like upstream API keys)
// stored in the durable keystore. Implementations must be safe for concurrent use.
//
// The Encrypt output is a self-describing string (with an algorithm tag prefix)
// so the stored value carries enough metadata to be decrypted later, and so
// future implementations can rotate algorithms without touching the schema.
type Cipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(encoded string) (string, error)
}

// Encryption key size for AES-256-GCM.
const AESKeySize = 32

// v1 tag identifies values produced by NewAESGCM. Everything after the prefix
// is base64(nonce || ciphertext || authTag).
const aesGCMTag = "v1:"

// NoopCipher returns a Cipher that stores values unchanged. Intended only for
// the in-memory keystore and tests. Never use it against a durable store.
func NoopCipher() Cipher { return noopCipher{} }

type noopCipher struct{}

func (noopCipher) Encrypt(s string) (string, error) { return s, nil }

func (noopCipher) Decrypt(s string) (string, error) {
	if strings.HasPrefix(s, aesGCMTag) {
		return "", errors.New("noop cipher cannot decrypt AES-GCM ciphertext; REIN_ENCRYPTION_KEY missing or wrong")
	}
	return s, nil
}

// NewAESGCM returns a Cipher using AES-256-GCM with the supplied 32-byte key.
func NewAESGCM(key []byte) (Cipher, error) {
	if len(key) != AESKeySize {
		return nil, fmt.Errorf("aes-gcm key must be %d bytes, got %d", AESKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return &aesGCMCipher{aead: aead}, nil
}

// DecodeHexKey parses a 64-char hex string into a 32-byte AES-256 key.
func DecodeHexKey(h string) ([]byte, error) {
	h = strings.TrimSpace(h)
	if len(h) != AESKeySize*2 {
		return nil, fmt.Errorf("encryption key must be %d hex chars (32 bytes), got %d", AESKeySize*2, len(h))
	}
	return hex.DecodeString(h)
}

type aesGCMCipher struct {
	aead cipher.AEAD
}

func (c *aesGCMCipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("read nonce: %w", err)
	}
	sealed := c.aead.Seal(nil, nonce, []byte(plaintext), nil)
	payload := make([]byte, 0, len(nonce)+len(sealed))
	payload = append(payload, nonce...)
	payload = append(payload, sealed...)
	return aesGCMTag + base64.RawStdEncoding.EncodeToString(payload), nil
}

func (c *aesGCMCipher) Decrypt(encoded string) (string, error) {
	rest, ok := strings.CutPrefix(encoded, aesGCMTag)
	if !ok {
		return "", errors.New("ciphertext missing v1 tag; data may be corrupt or was written without encryption")
	}
	payload, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(payload) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := payload[:nonceSize], payload[nonceSize:]
	plain, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plain), nil
}
