package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAI_ForwardsRequestAndBody(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotMethod string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"model": "gpt-4o-mini",
			"usage": {
				"prompt_tokens": 5,
				"completion_tokens": 10,
				"total_tokens": 15
			}
		}`))
	}))
	defer upstream.Close()

	oai, err := NewOpenAI(upstream.URL, nil, nil)
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	oai.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code: got %d want 200", rec.Code)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("upstream method: got %q want POST", gotMethod)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path: got %q want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("upstream auth header: got %q want Bearer sk-test", gotAuth)
	}

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"total_tokens": 15`) {
		t.Errorf("response body missing usage field, got: %s", body)
	}
}

func TestOpenAI_UpstreamError(t *testing.T) {
	// An intentionally broken base URL so the dial fails.
	oai, err := NewOpenAI("http://127.0.0.1:1", nil, nil)
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()

	oai.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", rec.Code)
	}
}

func TestOpenAI_InvalidBase(t *testing.T) {
	if _, err := NewOpenAI("not a url", nil, nil); err == nil {
		t.Error("expected error for base without scheme/host")
	}
}
