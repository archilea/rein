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

There are no read-only scopes in v0.1. Anyone with the admin token can flip the
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

rein is frozen: kill-switch engaged
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
`anthropic`. Budgets are optional and default to zero, which is treated as
unlimited.

```bash
curl -X POST "$REIN_URL/admin/v1/keys" \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "prod-app",
    "upstream": "openai",
    "upstream_key": "sk-your-real-openai-key",
    "daily_budget_usd": 100,
    "month_budget_usd": 2000
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
      "created_at": "2026-04-10T12:00:00Z"
    }
  ]
}
```

Pipe through `jq` for a readable operator view:

```bash
curl -s -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  "$REIN_URL/admin/v1/keys" \
  | jq '.keys[] | {id, name, upstream, daily_budget_usd, month_budget_usd, revoked_at}'
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
  "created_at": "2026-04-10T12:00:00Z",
  "revoked_at": "2026-04-10T13:30:00Z"
}
```

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
  are not in v0.1. If you need those, gate the admin port with your own
  identity-aware proxy.
- **No pagination on list yet.** `GET /admin/v1/keys` returns every row. This
  is fine at alpha scale and tracked as a future enhancement.
