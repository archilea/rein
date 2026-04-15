// Package concurrency enforces per-key caps on the number of in-flight
// /v1/* requests.
//
// The Store interface abstracts the counter backend so the proxy hot path
// depends on the interface, not the implementation. The default in-memory
// implementation is concurrency.Memory; a future shared-state implementation
// (a distributed semaphore, for example) slots in behind this same
// interface without rewriting the hot path or the adapters.
//
// This is the nginx limit_conn analog: bounds work-in-progress per key,
// orthogonal to the rate limiter (internal/rates) which bounds arrival
// velocity. Both are standard reverse-proxy brakes and are typically wired
// together.
package concurrency

// Store tracks per-key in-flight request counts and enforces MaxConcurrent
// caps. Implementations must be safe for concurrent use. A limit of 0 means
// unlimited and Acquire must always return true without recording state.
type Store interface {
	// Acquire attempts to reserve one in-flight slot for keyID. Returns
	// true if the slot was reserved, false if the limit has been reached.
	// Callers that get true MUST call Release exactly once when the
	// request lifecycle ends, regardless of exit mode.
	Acquire(keyID string, limit int) bool

	// Release frees one in-flight slot for keyID. Safe to call with a
	// limit of 0 (no-op) to simplify unconditional defer sites.
	Release(keyID string, limit int)
}
