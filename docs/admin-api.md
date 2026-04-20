# Rein admin API cookbook

Rein ships no dashboard. The admin interface is a small HTTP API protected by a
single bearer token, meant to be driven by `curl`, your runbook of choice, or
whatever HTTP client your team already uses. This file is a copy-pasteable
reference for every operation.

If you want a UI, wrap these endpoints yourself. They are stable, documented,
and deliberately small.

## Setup

Every example assumes two environment variables are set in your shell:

```bash
export REIN_URL=http://localhost:8080
export REIN_ADMIN_TOKEN=...   # the same value the Rein process is running with
```

All admin calls require the header `Authorization: Bearer $REIN_ADMIN_TOKEN`.
The token is compared in constant time on the server to defeat timing
side-channels. There is no login endpoint and no session: every request
carries the token, every response is stateless.

There are no read-only scopes today. Anyone with the admin token can flip the
kill-switch, mint keys, and revoke keys. Treat it like any other root secret.

## Kill-switch

A single global boolean. When frozen, every `/v1/*` request returns
`503 Service Unavailable` with `Retry-After: 60` until unfrozen, regardless of
virtual key, model, or upstream. The upstream is never contacted.

### Check current state

```bash
curl -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  "$REIN_URL/admin/v1/killswitch"
# -> {"frozen": false}
```

### Freeze all traffic

```bash
curl -X POST \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"frozen": true}' \
  "$REIN_URL/admin/v1/killswitch"
# -> {"frozen": true}
```

Every subsequent `/v1/*` request now returns:

```
HTTP/1.1 503 Service Unavailable
Retry-After: 60
Content-Type: application/json

{"error":{"code":"kill_switch_engaged","message":"rein is frozen: kill-switch engaged"}}
```

### Unfreeze

```bash
curl -X POST \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"frozen": false}' \
  "$REIN_URL/admin/v1/killswitch"
# -> {"frozen": false}
```

## Virtual keys

Rein issues `rein_live_...` bearer tokens that wrap a real upstream key
(OpenAI or Anthropic). Your application talks to Rein using the `rein_live_`
token; Rein swaps it for the real upstream key on the way out and enforces
budgets on the way back.

### Mint a new key

`name` and `upstream_key` are required. `upstream` must be `openai` or
`anthropic`. Budgets, rate limits, and `upstream_timeout_seconds` are optional
and default to zero, which is treated as unlimited. `expires_at` is optional;
omit it for a key with no expiry, see [Time-bounded keys](#time-bounded-keys)
for the auto-revocation semantics. See
[Upstream request timeout](#upstream-request-timeout) for the per-request
duration ceiling.

```bash
curl -X POST "$REIN_URL/admin/v1/keys" \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "prod-app",
    "upstream": "openai",
    "upstream_key": "sk-your-real-openai-key",
    "daily_budget_usd": 100,
    "month_budget_usd": 2000,
    "rps_limit": 10,
    "rpm_limit": 300,
    "max_concurrent": 50,
    "upstream_timeout_seconds": 120
  }'
```

Response:

```json
{
  "id": "key_...",
  "token": "rein_live_...",
  "name": "prod-app",
  "upstream": "openai",
  "daily_budget_usd": 100,
  "month_budget_usd": 2000,
  "rps_limit": 10,
  "rpm_limit": 300,
  "max_concurrent": 50,
  "upstream_timeout_seconds": 120,
  "created_at": "2026-04-10T12:00:00Z"
}
```

**The `token` is returned exactly once.** Rein never shows it again. Any
subsequent `GET /admin/v1/keys` response omits both the rein token and the
upstream key entirely. Copy the token straight into your secret manager.

### List all keys

Returns every key ever minted, including revoked ones. The `token` and
`upstream_key` fields are never included in the response.

```bash
curl -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  "$REIN_URL/admin/v1/keys"
```

Response:

```json
{
  "keys": [
    {
      "id": "key_...",
      "name": "prod-app",
      "upstream": "openai",
      "daily_budget_usd": 100,
      "month_budget_usd": 2000,
      "rps_limit": 10,
      "rpm_limit": 300,
      "max_concurrent": 50,
      "created_at": "2026-04-10T12:00:00Z"
    }
  ]
}
```

Pipe through `jq` for a readable operator view:

```bash
curl -s -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  "$REIN_URL/admin/v1/keys" \
  | jq '.keys[] | {id, name, upstream, daily_budget_usd, month_budget_usd, rps_limit, rpm_limit, max_concurrent, revoked_at}'
```

### Update a key's caps

`PATCH /admin/v1/keys/{id}` lets you change a virtual key's mutable fields
without revoking and re-minting it. Only the fields present in the JSON body
are changed; absent fields are preserved. Zero values explicitly set the cap
to unlimited (same semantics as create).

Mutable fields: `name`, `daily_budget_usd`, `month_budget_usd`, `rps_limit`,
`rpm_limit`, `max_concurrent`, `upstream_timeout_seconds`, `upstream_base_url`,
`expires_at`.

`expires_at` is tri-state on PATCH: omit the field to leave it unchanged, pass
an RFC3339 UTC timestamp to set or replace the expiry, or pass the explicit
JSON value `null` to clear the expiry. Past or malformed timestamps are
rejected with the structured codes documented in
[Time-bounded keys](#time-bounded-keys).

Immutable fields (`id`, `token`, `upstream`, `upstream_key`, `created_at`,
`revoked_at`) cannot be changed and are rejected with `400` if included.
Revoked keys return `409 Conflict`.

```bash
curl -X PATCH "$REIN_URL/admin/v1/keys/key_..." \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "daily_budget_usd": 200,
    "max_concurrent": 10,
    "rps_limit": 20
  }'
```

Response is the full key view with the updated fields:

```json
{
  "id": "key_...",
  "name": "prod-app",
  "upstream": "openai",
  "daily_budget_usd": 200,
  "month_budget_usd": 2000,
  "rps_limit": 20,
  "rpm_limit": 300,
  "max_concurrent": 10,
  "created_at": "2026-04-10T12:00:00Z"
}
```

The response never includes `token` or `upstream_key`.

Common use cases:

- Raise a per-key rate limit for a scheduled load test and lower it after.
- Extend a daily budget when a customer spikes legitimately.
- Lower `max_concurrent` when the upstream rate-limits harder than expected.
- Point an OpenAI-compatible key at a new `upstream_base_url` without rotating
  the rein token (e.g., migrating from Together to Fireworks).

### Revoke a key

Revocation is immediate and permanent. Subsequent requests using that token
return `401 Unauthorized`. The database row is kept with `revoked_at` set so
audit trails are preserved.

```bash
curl -X POST \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  "$REIN_URL/admin/v1/keys/key_.../revoke"
```

Response is the revoked key view with `revoked_at` populated:

```json
{
  "id": "key_...",
  "name": "prod-app",
  "upstream": "openai",
  "daily_budget_usd": 100,
  "month_budget_usd": 2000,
  "rps_limit": 10,
  "rpm_limit": 300,
  "max_concurrent": 50,
  "created_at": "2026-04-10T12:00:00Z",
  "revoked_at": "2026-04-10T13:30:00Z"
}
```

## Time-bounded keys

Any virtual key can carry an optional `expires_at` (RFC3339 UTC timestamp)
that automates revocation. Two independent brakes enforce it:

- The proxy hot path rejects requests using an expired key with a `401`
  whose body is bit-for-bit identical to a manually revoked key
  (`{"error":{"code":"key_revoked", ...}}`). Clients cannot tell whether
  the key was auto-revoked or revoked by an operator; the expiry schedule
  never leaks outside the admin surface.
- A background sweeper ticks on `REIN_EXPIRY_SWEEP_INTERVAL` (default
  `60s`, bounded to `[10s, 1h]`) and stamps `revoked_at = expires_at`
  on any expired key that the hot path had already been rejecting. The
  resulting database row is indistinguishable from one that was manually
  revoked, except `revoked_at == expires_at` within a few milliseconds,
  which is the signal operators can grep for to audit which revocations
  were scheduled vs reactive.

### Validation

- `expires_at` must parse as `time.RFC3339` / `time.RFC3339Nano` and must
  be strictly in the future (more than 1 second from now). Past values
  are rejected with `400 Bad Request` and `code = "expires_in_past"`.
  Malformed strings are rejected with `code = "invalid_expires_at"`. Both
  use the same envelope shape the rest of the admin API uses.
- There is no upper bound: a 10-year `expires_at` is valid. Keep your
  own calendar discipline for very long-lived keys.
- All `expires_at` values are normalized to UTC at the admin boundary.

### Contractor example

Mint a key that auto-revokes in 30 days. Good for contractors, vendor
evaluations, and anyone on a fixed-length engagement:

```bash
EXPIRES=$(python3 -c 'import datetime;print((datetime.datetime.now(datetime.UTC)+datetime.timedelta(days=30)).isoformat(timespec="seconds"))')

curl -X POST "$REIN_URL/admin/v1/keys" \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "contractor-acme",
    "upstream": "openai",
    "upstream_key": "sk-your-real-openai-key",
    "daily_budget_usd": 25,
    "expires_at": "'"$EXPIRES"'"
  }'
```

Response includes `expires_at` in the view:

```json
{
  "id": "key_...",
  "name": "contractor-acme",
  "upstream": "openai",
  "daily_budget_usd": 25,
  "month_budget_usd": 0,
  "rps_limit": 0,
  "rpm_limit": 0,
  "max_concurrent": 0,
  "created_at": "2026-04-17T12:00:00Z",
  "expires_at": "2026-05-17T12:00:00Z",
  "token": "rein_live_..."
}
```

### Break-glass / incident-response tokens

Mint a short-lived high-cap key for an on-call responder and have it
auto-revoke at shift end. No "who do I remember to revoke at 5pm" risk:

```bash
curl -X POST "$REIN_URL/admin/v1/keys" \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "oncall-break-glass",
    "upstream": "openai",
    "upstream_key": "sk-your-real-openai-key",
    "max_concurrent": 50,
    "expires_at": "2026-04-17T23:59:00Z"
  }'
```

### Extending or clearing an existing expiry

Use `PATCH /admin/v1/keys/{id}`:

```bash
# Extend by 7 days
curl -X PATCH "$REIN_URL/admin/v1/keys/key_..." \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"expires_at": "2026-05-24T12:00:00Z"}'

# Convert to a permanent key (remove the expiry entirely)
curl -X PATCH "$REIN_URL/admin/v1/keys/key_..." \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"expires_at": null}'
```

Backdating `expires_at` to force immediate revocation is intentionally
rejected: use `POST /admin/v1/keys/{id}/revoke` for that instead, which
is the unambiguous operator idiom for "cut this key now".

### Auditing upcoming expiries

Every list response includes `expires_at` when set and omits it
otherwise, so you can pipe the admin API through `jq` to find keys
expiring in the next week:

```bash
curl -s -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  "$REIN_URL/admin/v1/keys" | jq '
    .keys[] | select(.expires_at != null)
    | {id, name, expires_at}
    | select(.expires_at < (now + 7*24*3600 | strftime("%Y-%m-%dT%H:%M:%SZ")))
  '
```

## Rate limiting

Each virtual key can carry optional `rps_limit` (requests per second) and `rpm_limit`
(requests per minute) caps. Both default to zero, which means unlimited. When either
cap is reached, the request returns `429 Too Many Requests` with a `Retry-After` header
and the upstream is never contacted.

The algorithm is a sliding window counter (the same approach used by Cloudflare, Kong,
and Envoy). It bounds boundary-burst overshoot to approximately 1.1x the configured
limit, so `rps_limit: 10` allows at most approximately 11 requests in any rolling
1-second window.

Rate limit counters are in-memory and reset on process restart.

### Multi-replica note

In a multi-replica Rein deployment, each replica maintains its own rate counters.
Per-key limits are per-replica, not global. A 3-replica deployment with `rps_limit: 30`
effectively allows 90 RPS aggregate across all replicas.

Operator formula: `per_replica_limit = desired_global_limit / replica_count`.

A globally-synchronized variant via Redis is tracked in #53.

## Concurrency limiting

Each virtual key can carry an optional `max_concurrent` cap on the number of
in-flight `/v1/*` requests. The default is zero, which means unlimited. When the
cap is reached, the next request returns `429 Too Many Requests` with
`Retry-After: 1` and the upstream is never contacted. Slots free as in-flight
requests complete (success, upstream error, client disconnect, or context
cancel are all treated identically).

This is the nginx `limit_conn` analog. It is orthogonal to the rate limit:
rate limits bound arrival velocity (requests per second/minute), the
concurrency cap bounds work-in-progress (requests held at any instant). The
two compose: a key with `rps_limit: 10, max_concurrent: 5` allows 10 starts
per second but never more than 5 simultaneously.

The concurrency cap is the recommended brake to bound the soft-cap budget
overshoot documented in the README. With `max_concurrent: K` and a
worst-case per-request cost of `C`, the budget overshoot is bounded by
`K × C` regardless of arrival pattern.

Counters are in-memory and reset on process restart. In-flight requests
cannot outlive the process, so restart resets are safe.

### Multi-replica note

In a multi-replica Rein deployment, each replica maintains its own
concurrency counters. Per-key limits are per-replica, not global. A 3-replica
deployment with `max_concurrent: 30` effectively allows 90 simultaneous
in-flight requests aggregate across all replicas.

Operator formula: `per_replica_limit = desired_global_limit / replica_count`.

A globally-synchronized variant (a distributed semaphore via Redis, or
similar) is out of scope for 0.2 but slots in behind the same `Store`
interface without rewriting the hot path.

## Upstream request timeout

Each virtual key can carry an optional `upstream_timeout_seconds` ceiling on
the wall-clock duration of any one upstream call. The default is zero, which
means unlimited. When set, Rein wraps the request context with
`context.WithTimeout(vkey.UpstreamTimeoutSeconds * time.Second)` before
dispatching to the provider adapter. If the deadline fires before the
upstream finishes:

- **Non-streaming JSON responses** return `504 Gateway Timeout` with
  `Retry-After: 1` and the structured `upstream_timeout` code. The message
  names the configured ceiling so operators can correlate 504s with their
  key config.
- **Streaming (SSE) responses** cannot retroactively change their `200 OK`
  status; Rein writes a short SSE comment line
  (`: rein upstream timeout after N seconds\n\n`) and closes the connection
  cleanly. Any usage tokens parsed before the cancel are still recorded
  against the key budget (the partial-metering invariant from
  `docs/architecture.md`).

This is the `proxy_read_timeout` analog for LLM traffic, sized for
reasoning-model tail latency. A 30-second reasoning call terminated at
second 18 because the pod rolled is the failure mode this is designed to
prevent, as is the opposite case: a hanging call that silently burns
upstream cost for minutes while no usage chunk ever arrives.

### Validation

- `upstream_timeout_seconds` must be an integer in `[0, 3600]`.
- `0` means unlimited.
- Values greater than `3600` (one hour) are rejected with `400 Bad Request`
  to protect operators from typos such as `86400` (a full day in seconds).
  One hour covers every realistic reasoning-model or extended-thinking
  request.
- Sub-second precision is not supported in this release; revisit if
  operators ask for `"60s"` / `"5m"` duration strings.

### Setting a timeout on create

```bash
curl -X POST "$REIN_URL/admin/v1/keys" \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "reasoning-tier",
    "upstream": "openai",
    "upstream_key": "sk-your-real-openai-key",
    "upstream_timeout_seconds": 120
  }'
```

### Changing a timeout after the fact

```bash
# Lower the ceiling for a key that has been hanging.
curl -X PATCH "$REIN_URL/admin/v1/keys/key_..." \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"upstream_timeout_seconds": 45}'

# Remove the ceiling (back to unlimited).
curl -X PATCH "$REIN_URL/admin/v1/keys/key_..." \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"upstream_timeout_seconds": 0}'
```

### Interaction with other brakes

- **Dial timeout** (10 s, global) still fires independently on unreachable
  upstreams; a dial failure returns `502 Bad Gateway`, not `504`.
- **Budget caps** only fire after usage is recorded. A hanging call never
  reaches Record; the timeout is what bounds its worst case.
- **Rate limit** bounds arrival velocity, not request duration.
- **Concurrency cap** bounds in-flight slots; timeout releases hung slots
  so the cap stays useful under upstream flakiness.
- **max_output_tokens** (future) bounds worst-case cost per call; timeout
  bounds worst-case duration. Operators typically want both.

### Multi-replica note

`upstream_timeout_seconds` is a per-key field stored in the keystore and a
per-process timer at request time. No distributed state is required. In a
multi-replica deployment every replica enforces the same ceiling for the
same key without coordination.

## Proxy error response format

Every proxy-side error (`/v1/*` requests) returns a structured JSON envelope
so machine clients can branch on a stable `code` string rather than
substring-matching the message:

```json
{
  "error": {
    "code": "budget_exceeded",
    "message": "budget exceeded for this virtual key"
  }
}
```

The same envelope shape is used by the admin API for validation errors.
Headers (`Retry-After`, status codes) are unchanged from prior releases.

### Error code catalog

| Status | Code | When |
|--------|------|------|
| 404 | `unknown_route` | Request path is not a known `/v1/*` upstream route |
| 503 | `kill_switch_engaged` | Global kill-switch is frozen |
| 401 | `missing_key` | No `Authorization: Bearer` header present |
| 401 | `invalid_key` | Token does not match any row or is malformed |
| 401 | `key_revoked` | Token matches a revoked key |
| 400 | `upstream_mismatch` | Key's upstream does not match the request path |
| 402 | `budget_exceeded` | Daily or monthly USD cap already reached |
| 429 | `rate_limited` | RPS or RPM sliding window cap exceeded |
| 429 | `concurrency_exceeded` | Per-key in-flight cap reached |
| 504 | `upstream_timeout` | Per-key `upstream_timeout_seconds` exceeded (non-streaming) |
| 500 | `internal_error` | Unexpected server-side failure |

`rate_limited`, `concurrency_exceeded`, and `upstream_timeout` responses
include a `Retry-After` header. `kill_switch_engaged` includes
`Retry-After: 60`.

## Health and version

Both endpoints are unauthenticated by design so liveness probes and deployment
tooling can hit them without holding the admin token.

### Healthcheck

Returns `200 OK` with body `ok` when the process is up.

```bash
curl "$REIN_URL/healthz"
# -> ok
```

### Build info

Useful for verifying which version is running during an incident.

```bash
curl "$REIN_URL/version"
```

## Incident runbook

The shortest useful incident flow, copy-pasteable into a playbook:

```bash
# 1. Freeze everything.
curl -X POST \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"frozen": true}' \
  "$REIN_URL/admin/v1/killswitch"

# 2. Identify the offending key from your logs, then revoke it.
curl -X POST \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  "$REIN_URL/admin/v1/keys/key_.../revoke"

# 3. Unfreeze the rest of the fleet.
curl -X POST \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"frozen": false}' \
  "$REIN_URL/admin/v1/killswitch"
```

## Operational notes

- **Put the admin API on a private network where possible.** Rein serves admin
  calls on the same port as proxy traffic. If you expose Rein to the public
  internet, front it with a reverse proxy or firewall rule that restricts
  `/admin/v1/*` to your ops VPN or tailnet.
- **Script these, do not memorize them.** This file is meant to live next to
  your runbooks. Fork it, adapt it, keep a copy checked into your ops repo.
- **The admin token is all-or-nothing.** Read-only scopes and per-user tokens
  are not implemented. If you need those, gate the admin port with your own
  identity-aware proxy.
- **No pagination on list yet.** `GET /admin/v1/keys` returns every row. This
  is fine at alpha scale and tracked as a future enhancement.
