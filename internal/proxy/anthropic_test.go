package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropic_ForwardsRequestWithHeaders(t *testing.T) {
	var (
		gotPath    string
		gotVersion string
		gotAuth    string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_test",
			"model": "claude-sonnet-4",
			"usage": {"input_tokens": 20, "output_tokens": 30}
		}`))
	}))
	defer upstream.Close()

	ant, err := NewAnthropic(upstream.URL, nil, nil)
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4","messages":[]}`))
	req.Header.Set("Authorization", "Bearer should-be-stripped")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ant.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path: got %q want /v1/messages", gotPath)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version: got %q want 2023-06-01", gotVersion)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header should be stripped upstream, got %q", gotAuth)
	}

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"input_tokens": 20`) {
		t.Errorf("response missing usage: %s", body)
	}
}

func TestAnthropic_InvalidBase(t *testing.T) {
	if _, err := NewAnthropic("nope", nil, nil); err == nil {
		t.Error("expected error for invalid base")
	}
}
