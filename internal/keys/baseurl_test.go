package keys

import (
	"strings"
	"testing"
)

func TestValidateUpstreamBaseURL_Canonicalization(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https no trailing slash", "https://api.groq.com", "https://api.groq.com"},
		{"https trailing slash stripped", "https://api.groq.com/", "https://api.groq.com"},
		{"https with port", "https://api.together.xyz:443", "https://api.together.xyz:443"},
		{"uppercase scheme lowercased", "HTTPS://api.groq.com", "https://api.groq.com"},
		{"loopback http allowed", "http://127.0.0.1:11434", "http://127.0.0.1:11434"},
		{"ipv6 loopback http allowed", "http://[::1]:11434", "http://[::1]:11434"},
		{"localhost http allowed", "http://localhost:11434", "http://localhost:11434"},
		{"whitespace trimmed", "  https://api.groq.com  ", "https://api.groq.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateUpstreamBaseURL(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("canonical form: got %q want %q", got, tc.want)
			}
		})
	}
}

func TestValidateUpstreamBaseURL_Rejections(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantCode string
	}{
		{"empty", "", ErrCodeInvalidBaseURL},
		{"whitespace only", "   ", ErrCodeInvalidBaseURL},
		{"no scheme", "api.groq.com", ErrCodeInvalidBaseURLHost},
		{"ftp scheme", "ftp://api.groq.com", ErrCodeInvalidBaseURLScheme},
		{"file scheme", "file:///etc/passwd", ErrCodeInvalidBaseURLHost},
		{"non-loopback http", "http://api.groq.com", ErrCodeInvalidBaseURLScheme},
		{"public ip http", "http://8.8.8.8", ErrCodeInvalidBaseURLScheme},
		{"path rejected", "https://api.groq.com/openai/v1", ErrCodeInvalidBaseURLPath},
		{"query rejected", "https://api.groq.com?foo=bar", ErrCodeInvalidBaseURLQuery},
		{"fragment rejected", "https://api.groq.com#frag", ErrCodeInvalidBaseURLFragment},
		{"userinfo rejected", "https://user:pass@api.groq.com", ErrCodeInvalidBaseURLHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateUpstreamBaseURL(tc.in)
			if err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
			bue := AsBaseURLError(err)
			if bue == nil {
				t.Fatalf("expected *BaseURLError, got %T: %v", err, err)
			}
			if bue.Code != tc.wantCode {
				t.Errorf("error code: got %q want %q (message=%q)", bue.Code, tc.wantCode, bue.Message)
			}
			if bue.Message == "" {
				t.Errorf("error message is empty")
			}
		})
	}
}

func TestValidateUpstreamBaseURL_TrailingSlashOnly(t *testing.T) {
	// A bare "/" path is a non-content path. Accept it and strip to canonical form.
	got, err := ValidateUpstreamBaseURL("https://api.groq.com/")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if strings.HasSuffix(got, "/") {
		t.Errorf("canonical form should not end with /: %q", got)
	}
}

func TestBaseURLError_ErrorString(t *testing.T) {
	e := &BaseURLError{Code: "test_code", Message: "human message"}
	if got := e.Error(); got != "test_code: human message" {
		t.Errorf("Error(): got %q want %q", got, "test_code: human message")
	}
}

func TestAsBaseURLError_NonMatching(t *testing.T) {
	if bue := AsBaseURLError(nil); bue != nil {
		t.Errorf("nil input: got %+v want nil", bue)
	}
}
