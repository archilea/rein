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
const (
	ErrCodeInvalidBaseURL         = "invalid_upstream_base_url"
	ErrCodeInvalidBaseURLScheme   = "invalid_upstream_base_url_scheme"
	ErrCodeInvalidBaseURLHost     = "invalid_upstream_base_url_host"
	ErrCodeInvalidBaseURLPath     = "invalid_upstream_base_url_path"
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
// URL override. It returns the canonical "scheme://host[:port]" form (no
// trailing slash, no path, no query, no fragment) on success, or a typed
// *BaseURLError on failure.
//
// Rules:
//  1. Must parse via net/url.Parse.
//  2. Scheme must be "https", except when the host resolves to a loopback
//     address (127.0.0.0/8, ::1), in which case "http" is also accepted for
//     local vLLM / Ollama / LocalAI ergonomics. Schemes other than http/https
//     (file, unix, ftp, ...) are rejected outright.
//  3. Host must be present and non-empty.
//  4. Path, raw query, and fragment must all be empty. Rein's OpenAI
//     adapter composes the full request path itself; allowing an operator
//     to inject a path prefix would silently change routing semantics.
//     (Azure OpenAI needs path rewriting and is tracked separately.)
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
	// Path / query / fragment are scope-expansion risks. Reject them with a
	// specific code so operators do not have to guess which character
	// caused the rejection.
	if u.Path != "" && u.Path != "/" {
		return "", newBaseURLError(ErrCodeInvalidBaseURLPath,
			"upstream_base_url must not contain a path; Rein's adapter composes the full path itself")
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
	// Canonical form: scheme + host (with port if present), lowercased
	// scheme, no trailing slash, no path/query/fragment.
	canonical := scheme + "://" + u.Host
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
