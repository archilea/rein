package proxy

// Proxy error codes. Every code is a public API contract once shipped.
// New codes must be added here so reviewers can track the full set.
const (
	// CodeKillSwitchEngaged is returned (503) when the global kill-switch is frozen.
	CodeKillSwitchEngaged = "kill_switch_engaged"

	// CodeDraining is returned (503) when the proxy has entered shutdown
	// drain mode. Distinct from the kill-switch because drain is a planned
	// rolling-deploy signal ("retry against a sibling replica") while the
	// kill-switch is an incident-response hard stop ("everybody fails
	// now"). Carries Retry-After: 5 so clients back off briefly before
	// retrying against the load balancer's next replica.
	CodeDraining = "draining"

	// CodeMissingKey is returned (401) when no Authorization: Bearer header is present.
	CodeMissingKey = "missing_key"

	// CodeInvalidKey is returned (401) when the token does not match any row or is malformed.
	CodeInvalidKey = "invalid_key"

	// CodeKeyRevoked is returned (401) when the token matches a revoked key.
	CodeKeyRevoked = "key_revoked"

	// CodeUpstreamMismatch is returned (400) when the key's upstream does not match the request path.
	CodeUpstreamMismatch = "upstream_mismatch"

	// CodeBudgetExceeded is returned (402) when the daily or monthly USD cap is reached.
	CodeBudgetExceeded = "budget_exceeded"

	// CodeRateLimited is returned (429) when the RPS or RPM sliding window cap is exceeded.
	CodeRateLimited = "rate_limited"

	// CodeConcurrencyExceeded is returned (429) when the per-key in-flight cap is reached.
	CodeConcurrencyExceeded = "concurrency_exceeded"

	// CodeUnknownRoute is returned (404) when the request path is not a known upstream route.
	CodeUnknownRoute = "unknown_route"

	// CodeUpstreamTimeout is returned (504) when a request exceeds the per-key
	// upstream_timeout_seconds ceiling. Fires only on non-streaming responses:
	// on streams the 200 status has already been flushed before the timeout
	// fires, so the stream reader writes a short SSE comment line and closes
	// the connection instead of re-flagging the status.
	CodeUpstreamTimeout = "upstream_timeout"

	// CodeInternalError is returned (500) for unexpected server-side failures
	// (kill-switch read error, key resolution error, no handler wired).
	CodeInternalError = "internal_error"
)
