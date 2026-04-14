// Package rates enforces per-key request velocity limits.
//
// The Store interface abstracts the counter backend so the proxy hot path
// depends on the interface, not the implementation. The default in-memory
// implementation is rates.Memory; a future shared-state implementation
// (Redis + Lua, #53) slots in behind this same interface without rewriting
// the hot path or the adapters.
package rates

import (
	"context"
	"errors"
	"fmt"
)

// ErrRateLimited is returned by Store.Allow when a key's RPS or RPM cap
// has been reached. Callers should translate this into a 429 Too Many
// Requests at the edge. The error may wrap a retryAfterError that carries
// the recommended Retry-After value in seconds.
var ErrRateLimited = errors.New("rate limit exceeded")

// Store tracks per-key request rate counters and enforces RPS/RPM caps.
// Implementations must be safe for concurrent use. A limit of 0 means
// unlimited on that granularity.
type Store interface {
	Allow(ctx context.Context, keyID string, rpsLimit, rpmLimit int) error
}

// retryAfterError wraps ErrRateLimited with a Retry-After value.
type retryAfterError struct {
	seconds int
}

func (e *retryAfterError) Error() string {
	return fmt.Sprintf("rate limit exceeded (retry after %ds)", e.seconds)
}

func (e *retryAfterError) Unwrap() error { return ErrRateLimited }

// newRetryAfterError returns an error wrapping ErrRateLimited with the
// recommended Retry-After value. seconds is clamped to [1, 60].
func newRetryAfterError(seconds int) error {
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 60 {
		seconds = 60
	}
	return &retryAfterError{seconds: seconds}
}

// RetryAfter extracts the Retry-After seconds from an error wrapping
// ErrRateLimited. Returns (0, false) if the error is not rate-limited.
func RetryAfter(err error) (int, bool) {
	var ra *retryAfterError
	if errors.As(err, &ra) {
		return ra.seconds, true
	}
	return 0, false
}
