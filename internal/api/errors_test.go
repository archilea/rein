package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusBadRequest, "test_code", "test message")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", ct)
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != "test_code" {
		t.Errorf("code: got %q want test_code", env.Error.Code)
	}
	if env.Error.Message != "test message" {
		t.Errorf("message: got %q want test message", env.Error.Message)
	}
}

func TestWriteError_RetryAfterHeaderPreserved(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Retry-After", "60")
	WriteError(rec, http.StatusServiceUnavailable, "frozen", "frozen")

	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After: got %q want 60", got)
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, map[string]bool{"ok": true})

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", ct)
	}

	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body["ok"] {
		t.Errorf("body: got %v want {ok: true}", body)
	}
}
