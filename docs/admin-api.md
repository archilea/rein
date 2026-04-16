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
`anthropic`. Budgets and rate limits are optional and default to zero, which is
treated as unlimited.

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
    "max_concurrent": 50
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
| 500 | `internal_error` | Unexpected server-side failure |

`rate_limited` and `concurrency_exceeded` responses include a `Retry-After`
header. `kill_switch_engaged` includes `Retry-After: 60`.

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
