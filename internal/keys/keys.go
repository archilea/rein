// Package keys manages Rein virtual keys: generation, storage, and lookup.
//
// A virtual key is a Rein-issued bearer token (rein_live_...) that maps to a
// real upstream provider (OpenAI, Anthropic, ...) and the real API key to use
// against that provider. Clients never see the upstream key; they talk to
// Rein with the virtual key, and Rein swaps it before forwarding.
package keys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Known upstream providers.
const (
	UpstreamOpenAI    = "openai"
	UpstreamAnthropic = "anthropic"
)

// Identifier prefixes.
const (
	idPrefix    = "key_"
	tokenPrefix = "rein_live_"
)

// ErrNotFound is returned when a key lookup fails.
var ErrNotFound = errors.New("virtual key not found")

// VirtualKey is a Rein-issued bearer that maps to an upstream provider and API key.
// All timestamps are UTC.
type VirtualKey struct {
	ID             string
	Token          string
	Name           string
	Upstream       string
	UpstreamKey    string
	DailyBudgetUSD float64
	MonthBudgetUSD float64
	CreatedAt      time.Time
	RevokedAt      *time.Time
}

// IsRevoked reports whether the key has been revoked.
func (k *VirtualKey) IsRevoked() bool {
	return k != nil && k.RevokedAt != nil
}

// Store is the persistence contract for virtual keys.
// Implementations must be safe for concurrent use.
type Store interface {
	Create(ctx context.Context, k *VirtualKey) error
	GetByToken(ctx context.Context, token string) (*VirtualKey, error)
	GetByID(ctx context.Context, id string) (*VirtualKey, error)
	List(ctx context.Context) ([]*VirtualKey, error)
	Revoke(ctx context.Context, id string) error
}

// Memory is an in-memory Store. Contents are lost on process restart.
// Intended for development and tests; use a durable implementation in production.
type Memory struct {
	mu      sync.RWMutex
	byID    map[string]*VirtualKey
	byToken map[string]*VirtualKey
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		byID:    make(map[string]*VirtualKey),
		byToken: make(map[string]*VirtualKey),
	}
}

// Create persists a new virtual key.
// Returns an error if ID or Token collide, or if fields are missing.
func (m *Memory) Create(_ context.Context, k *VirtualKey) error {
	if k == nil {
		return errors.New("nil virtual key")
	}
	if k.ID == "" || k.Token == "" {
		return errors.New("virtual key requires ID and Token")
	}
	if k.Upstream != UpstreamOpenAI && k.Upstream != UpstreamAnthropic {
		return fmt.Errorf("unsupported upstream %q", k.Upstream)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byID[k.ID]; exists {
		return fmt.Errorf("virtual key ID %q already exists", k.ID)
	}
	if _, exists := m.byToken[k.Token]; exists {
		return errors.New("virtual key token collision")
	}
	cp := *k
	m.byID[k.ID] = &cp
	m.byToken[k.Token] = &cp
	return nil
}

// GetByToken returns a copy of the virtual key for the given secret token.
func (m *Memory) GetByToken(_ context.Context, token string) (*VirtualKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.byToken[token]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *k
	return &cp, nil
}

// GetByID returns a copy of the virtual key for the given admin ID.
func (m *Memory) GetByID(_ context.Context, id string) (*VirtualKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *k
	return &cp, nil
}

// List returns copies of all virtual keys.
func (m *Memory) List(_ context.Context) ([]*VirtualKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*VirtualKey, 0, len(m.byID))
	for _, k := range m.byID {
		cp := *k
		out = append(out, &cp)
	}
	return out, nil
}

// Revoke marks a key as revoked. Revoked keys still resolve but IsRevoked() is true.
func (m *Memory) Revoke(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.byID[id]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	k.RevokedAt = &now
	return nil
}

// GenerateID returns a new public admin identifier, 16 hex chars prefixed with "key_".
func GenerateID() (string, error) {
	b, err := randomHex(8)
	if err != nil {
		return "", err
	}
	return idPrefix + b, nil
}

// GenerateToken returns a new secret bearer token, 64 hex chars prefixed with "rein_live_".
func GenerateToken() (string, error) {
	b, err := randomHex(32)
	if err != nil {
		return "", err
	}
	return tokenPrefix + b, nil
}

// IsReinToken reports whether s has the Rein virtual-key prefix.
func IsReinToken(s string) bool {
	return strings.HasPrefix(s, tokenPrefix)
}

// validIDPattern matches a well-formed admin identifier as produced by
// GenerateID: the "key_" prefix followed by exactly 16 lowercase hex chars.
// Intentionally strict so admin handlers can reject malformed path parameters
// before they reach the keystore, any log line, or any error response.
var validIDPattern = regexp.MustCompile(`^key_[a-f0-9]{16}$`)

// ValidID reports whether s is a syntactically well-formed virtual-key admin
// identifier. A true return guarantees the string contains only the literal
// "key_" followed by 16 lowercase hex characters, so it is safe to pass to
// log fields, error responses, and URL templates without further escaping.
// It does NOT confirm the key exists in the store.
func ValidID(s string) bool {
	return validIDPattern.MatchString(s)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
