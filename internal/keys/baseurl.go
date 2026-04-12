package keys

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// Stable error codes for per-key upstream_base_url validation failures.
// These are surfaced verbatim in the admin API error envelope so operators
// can tell at a glance which rule their URL violated. Treat as part of the
// public admin-API contract; do not rename without a CHANGELOG entry.
//
// ErrCodeInvalidBaseURLPath is retained as a stable public contract symbol
// for ABI compatibility but is no longer emitted: real OpenAI-compatible
// providers (Groq at /openai, OpenRouter at /api, Fireworks at /inference)
// use a path prefix, so the validator now accepts a non-empty path. A
// future breaking change could remove this constant with a CHANGELOG entry.
const (
	ErrCodeInvalidBaseURL         = "invalid_upstream_base_url"
	ErrCodeInvalidBaseURLScheme   = "invalid_upstream_base_url_scheme"
	ErrCodeInvalidBaseURLHost     = "invalid_upstream_base_url_host"
	ErrCodeInvalidBaseURLPath     = "invalid_upstream_base_url_path" // no longer emitted; retained for ABI stability
	ErrCodeInvalidBaseURLQuery    = "invalid_upstream_base_url_query"
	ErrCodeInvalidBaseURLFragment = "invalid_upstream_base_url_fragment"
)

// BaseURLError is returned by ValidateUpstreamBaseURL. Code is one of the
// ErrCodeInvalidBaseURL* constants; Message is the human-readable explanation
// suitable for an admin-API response body.
type BaseURLError struct {
	Code    string
	Message string
}

func (e *BaseURLError) Error() string { return e.Code + ": " + e.Message }

// newBaseURLError keeps construction local so the three call sites below
// stay tight and no caller can accidentally emit an error with an empty code.
func newBaseURLError(code, msg string) *BaseURLError {
	return &BaseURLError{Code: code, Message: msg}
}

// ValidateUpstreamBaseURL normalizes and validates a per-key upstream base
// URL override. It returns the canonical "scheme://host[:port][/path]" form
// (lowercased scheme, trailing slash stripped, no query, no fragment) on
// success, or a typed *BaseURLError on failure.
//
// Rules:
//  1. Must parse via net/url.Parse.
//  2. Scheme must be "https", except when the host resolves to a loopback
//     address (127.0.0.0/8, ::1), in which case "http" is also accepted for
//     local vLLM / Ollama / LocalAI ergonomics. Schemes other than http/https
//     (file, unix, ftp, ...) are rejected outright.
//  3. Host must be present and non-empty.
//  4. A non-empty path IS allowed. Many real OpenAI-compatible providers
//     mount under a path prefix: Groq at /openai, OpenRouter at /api,
//     Fireworks at /inference. httputil.ProxyRequest.SetURL joins
//     target.Path with the incoming request path, so a base URL of
//     https://api.groq.com/openai + an incoming /v1/chat/completions is
//     routed to https://api.groq.com/openai/v1/chat/completions without
//     any adapter change. The operator-facing convention is "base URL is
//     everything up to but not including the /v1/ segment that Rein
//     already prepends on every outbound request".
//  5. Query and fragment must be empty. An upstream base URL has no
//     business carrying query parameters or anchors, and allowing either
//     would silently change routing semantics.
//  6. Userinfo must be absent. Credentials belong in upstream_key.
//
// Canonicalization strips a trailing slash from the path (so
// https://api.groq.com/openai/ and https://api.groq.com/openai are equal),
// lowercases the scheme, and preserves the host and port verbatim.
//
// The returned string is suitable for direct storage in the keystore and
// for direct url.Parse at hot-path time.
func ValidateUpstreamBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", newBaseURLError(ErrCodeInvalidBaseURL, "upstream_base_url must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", newBaseURLError(ErrCodeInvalidBaseURL, "upstream_base_url is not a valid URL")
	}
	if u.Host == "" {
		return "", newBaseURLError(ErrCodeInvalidBaseURLHost,
			"upstream_base_url must include a host (got no host)")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", newBaseURLError(ErrCodeInvalidBaseURLScheme,
			"upstream_base_url scheme must be https (or http for loopback only)")
	}
	if u.RawQuery != "" {
		return "", newBaseURLError(ErrCodeInvalidBaseURLQuery,
			"upstream_base_url must not contain a query string")
	}
	if u.Fragment != "" {
		return "", newBaseURLError(ErrCodeInvalidBaseURLFragment,
			"upstream_base_url must not contain a fragment")
	}
	if u.User != nil {
		return "", newBaseURLError(ErrCodeInvalidBaseURLHost,
			"upstream_base_url must not contain userinfo; put credentials in upstream_key")
	}
	if scheme == "http" {
		hostname := u.Hostname()
		if !isLoopbackHost(hostname) {
			return "", newBaseURLError(ErrCodeInvalidBaseURLScheme,
				"upstream_base_url must use https for non-loopback hosts")
		}
	}
	// Canonical form: scheme + host (with port if present) + optional
	// path prefix with trailing slash stripped. No query/fragment.
	canonical := scheme + "://" + u.Host
	if path := strings.TrimRight(u.Path, "/"); path != "" {
		canonical += path
	}
	return canonical, nil
}

// isLoopbackHost reports whether the given hostname (as returned by
// url.URL.Hostname(), so bracketless) resolves exclusively to loopback
// addresses. A nil DNS result or any non-loopback answer fails closed.
//
// We accept the literal "localhost" and literal loopback IPs without a
// resolver round-trip so the common path is stdlib-only; for everything
// else we defer to net.LookupIP and require every returned address to be
// loopback. A non-loopback host that happens to also have a loopback A
// record (unusual but legal) is rejected — this is the tighter, safer
// reading.
func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	addrs, err := net.LookupIP(host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !a.IsLoopback() {
			return false
		}
	}
	return true
}

// AsBaseURLError unwraps a *BaseURLError from err if present, or returns nil.
// Callers in the admin package use this to build the API error envelope.
func AsBaseURLError(err error) *BaseURLError {
	var bue *BaseURLError
	if errors.As(err, &bue) {
		return bue
	}
	return nil
}
