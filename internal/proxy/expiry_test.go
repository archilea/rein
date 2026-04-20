package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/archilea/rein/internal/keys"
)

// newExpiringTestKey creates a key and sets its ExpiresAt directly in
// the store. Bypassing the admin validator lets us materialize the
// "already past" and "just about to expire" states that the admin API
// would reject at create time.
func newExpiringTestKey(t *testing.T, expiresAt time.Time) (*keys.Memory, *keys.VirtualKey) {
	t.Helper()
	store := keys.NewMemory()
	id, err := keys.GenerateID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := keys.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	exp := expiresAt.UTC()
	vk := &keys.VirtualKey{
		ID:          id,
		Token:       token,
		Name:        "temp",
		Upstream:    keys.UpstreamOpenAI,
		UpstreamKey: "sk-real",
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   &exp,
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatalf("create: %v", err)
	}
	return store, vk
}

// TestProxy_ExpiredKeyRejectedOnHotPath confirms the belt-and-suspenders
// rule from #77: a key whose expires_at has already passed fails closed
// on every request even when the sweeper has not yet stamped revoked_at.
// The response is indistinguishable from manual revocation: same 401,
// same key_revoked code, no expires_at information leaked.
func TestProxy_ExpiredKeyRejectedOnHotPath(t *testing.T) {
	store, vk := newExpiringTestKey(t, time.Now().UTC().Add(-time.Minute))
	p := newTestProxy(t, store, "https://api.openai.com", "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expired: got %d want 401; body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec, CodeKeyRevoked)

	// The error message and code must be identical to manual revocation
	// so clients cannot learn when a key was set to expire.
	if got := rec.Body.String(); got == "" {
		t.Fatal("empty body")
	}
	// expires_at must not appear in the response to avoid leaking the operator detail.
	if rec.Body.Len() > 0 {
		for _, forbidden := range []string{"expires_at", "expired", "expiry"} {
			if contains := containsFold(rec.Body.String(), forbidden); contains {
				t.Errorf("response leaks %q: %s", forbidden, rec.Body.String())
			}
		}
	}
}

func TestProxy_ExactlyExpiredKeyRejected(t *testing.T) {
	// Boundary: expires_at == now. Per IsExpired semantics (!now.Before(expires)),
	// this must be treated as expired.
	now := time.Now().UTC()
	store, vk := newExpiringTestKey(t, now)
	p := newTestProxy(t, store, "https://api.openai.com", "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("exactly expired: got %d want 401", rec.Code)
	}
}

func TestProxy_FutureExpiryAllowed(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer upstream.Close()

	store, vk := newExpiringTestKey(t, time.Now().UTC().Add(time.Hour))
	p := newTestProxy(t, store, upstream.URL, "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("future expiry: got %d body=%s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer sk-real" {
		t.Errorf("upstream auth: got %q", gotAuth)
	}
}

// containsFold is a case-insensitive substring check used to scan the
// error body for leaked expiry terminology. Kept local to the test file
// so a typo in a leaked-term regex stays as local as possible.
func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
