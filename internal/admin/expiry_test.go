package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdmin_CreateKey_WithExpiresAt(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	future := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	body := `{"name":"contractor","upstream":"openai","upstream_key":"sk-x","expires_at":"` +
		future.Format(time.RFC3339) + `"}`
	rec := postAuthed(t, mux, "/admin/v1/keys", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ExpiresAt == nil || !resp.ExpiresAt.Equal(future) {
		t.Errorf("expires_at in response: got %v want %v", resp.ExpiresAt, future)
	}

	vk, err := store.GetByToken(context.Background(), resp.Token)
	if err != nil {
		t.Fatalf("store lookup: %v", err)
	}
	if vk.ExpiresAt == nil || !vk.ExpiresAt.Equal(future) {
		t.Errorf("expires_at in store: got %v", vk.ExpiresAt)
	}
}

func TestAdmin_CreateKey_ExpiresAt_PastRejected(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	body := `{"name":"x","upstream":"openai","upstream_key":"sk-x","expires_at":"` + past + `"}`
	rec := postAuthed(t, mux, "/admin/v1/keys", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != "expires_in_past" {
		t.Errorf("error code: got %q want expires_in_past", env.Error.Code)
	}
}

func TestAdmin_CreateKey_ExpiresAt_Malformed(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	cases := []string{"tomorrow", "2026-13-01T00:00:00Z", "1700000000", ""}
	for i, raw := range cases {
		name := raw
		if name == "" {
			name = "empty_string"
		}
		t.Run(name, func(t *testing.T) {
			if i == len(cases)-1 {
				// Empty string means the field was present but blank;
				// treat as absent (no expiry) rather than malformed.
				body := `{"name":"y","upstream":"openai","upstream_key":"sk-x","expires_at":""}`
				rec := postAuthed(t, mux, "/admin/v1/keys", body)
				if rec.Code != http.StatusCreated {
					t.Fatalf("empty string expires_at should be treated as absent: got %d body=%s",
						rec.Code, rec.Body.String())
				}
				return
			}
			body := `{"name":"y","upstream":"openai","upstream_key":"sk-x","expires_at":"` + raw + `"}`
			rec := postAuthed(t, mux, "/admin/v1/keys", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400 for %q; body=%s", rec.Code, raw, rec.Body.String())
			}
			var env errorEnvelope
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Error.Code != "invalid_expires_at" {
				t.Errorf("error code: got %q want invalid_expires_at", env.Error.Code)
			}
		})
	}
}

func TestAdmin_CreateKey_ExpiresAt_TooSoonRejected(t *testing.T) {
	// A timestamp within the 1-second future-skew window is treated as
	// in the past: the sweeper would race a key that was already
	// practically expired at creation time.
	_, _, _, mux := newTestServer(t)
	tooSoon := time.Now().UTC().Add(500 * time.Millisecond).Format(time.RFC3339Nano)
	body := `{"name":"x","upstream":"openai","upstream_key":"sk-x","expires_at":"` + tooSoon + `"}`
	rec := postAuthed(t, mux, "/admin/v1/keys", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestAdmin_ListKeys_IncludesExpiresAt(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	createBody := `{"name":"temp","upstream":"openai","upstream_key":"sk-x","expires_at":"` +
		future.Format(time.RFC3339) + `"}`
	if rec := postAuthed(t, mux, "/admin/v1/keys", createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Keys []keyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Keys) != 1 || resp.Keys[0].ExpiresAt == nil || !resp.Keys[0].ExpiresAt.Equal(future) {
		t.Errorf("list expires_at: got %+v want %v", resp.Keys, future)
	}
}

func TestAdmin_ListKeys_OmitsExpiresAtWhenNull(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	if rec := postAuthed(t, mux, "/admin/v1/keys",
		`{"name":"plain","upstream":"openai","upstream_key":"sk-x"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `"expires_at"`) {
		t.Errorf("list must omit expires_at when null; body=%s", rec.Body.String())
	}
}

func TestAdmin_UpdateKey_SetExpiresAt(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	id := createTestKey(t, mux, `{"name":"x","upstream":"openai","upstream_key":"sk-x"}`)
	future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)

	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id,
		`{"expires_at":"`+future.Format(time.RFC3339)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d body=%s", rec.Code, rec.Body.String())
	}

	var resp keyView
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.ExpiresAt == nil || !resp.ExpiresAt.Equal(future) {
		t.Errorf("expires_at: got %v want %v", resp.ExpiresAt, future)
	}

	vk, _ := store.GetByID(context.Background(), id)
	if vk.ExpiresAt == nil || !vk.ExpiresAt.Equal(future) {
		t.Errorf("persisted expires_at: got %v", vk.ExpiresAt)
	}
}

func TestAdmin_UpdateKey_ClearExpiresAtViaNull(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	id := createTestKey(t, mux,
		`{"name":"x","upstream":"openai","upstream_key":"sk-x","expires_at":"`+
			future.Format(time.RFC3339)+`"}`)

	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, `{"expires_at":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp keyView
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.ExpiresAt != nil {
		t.Errorf("expires_at after null patch: got %v want nil", resp.ExpiresAt)
	}
	vk, _ := store.GetByID(context.Background(), id)
	if vk.ExpiresAt != nil {
		t.Errorf("persisted clear: got %v", vk.ExpiresAt)
	}
}

func TestAdmin_UpdateKey_ExpiresAt_PastRejected(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	id := createTestKey(t, mux, `{"name":"x","upstream":"openai","upstream_key":"sk-x"}`)
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, `{"expires_at":"`+past+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "expires_in_past" {
		t.Errorf("code: got %q want expires_in_past", env.Error.Code)
	}
}

func TestAdmin_UpdateKey_ExpiresAt_Malformed(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	id := createTestKey(t, mux, `{"name":"x","upstream":"openai","upstream_key":"sk-x"}`)
	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, `{"expires_at":"not-a-date"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "invalid_expires_at" {
		t.Errorf("code: got %q want invalid_expires_at", env.Error.Code)
	}
}

func TestParseExpiresAt_EmptyStringRejected(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	_, code := parseExpiresAt("   ", now)
	if code != errCodeInvalidExpiresAt {
		t.Errorf("empty whitespace: got %q want %q", code, errCodeInvalidExpiresAt)
	}
}

func TestMessageForExpiresAtCode_Default(t *testing.T) {
	// Defensive default branch that fires if a new error code is added
	// without a matching case. Testing it directly keeps the branch
	// honest rather than silently unreachable code.
	msg := messageForExpiresAtCode("totally_unknown")
	if msg == "" {
		t.Error("default branch must return a non-empty message")
	}
}

func TestAdmin_UpdateKey_ExpiresAt_NonStringNonNullRejected(t *testing.T) {
	// A JSON number, boolean, or object for expires_at must be rejected
	// as invalid_expires_at rather than silently ignored. This guards
	// against a careless client sending a unix timestamp integer.
	_, _, _, mux := newTestServer(t)

	id := createTestKey(t, mux, `{"name":"x","upstream":"openai","upstream_key":"sk-x"}`)
	cases := []string{`{"expires_at":1234567890}`, `{"expires_at":true}`, `{"expires_at":{}}`}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("body %s: got %d want 400", body, rec.Code)
			}
		})
	}
}
