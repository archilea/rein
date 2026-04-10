# Quickstart

Rein runs as a single container. Point your app at it, give it upstream API keys, and set budget rules.

## Prerequisites

- Docker
- An upstream LLM API key (OpenAI or Anthropic)

## 1. Run Rein

Rein refuses to start without two secrets: an admin bearer token for the admin
API, and a 32-byte encryption key used to encrypt upstream keys at rest in the
SQLite keystore. Generate both once and store them like any other root secret.

```bash
docker run -d \
  --name rein \
  -p 8080:8080 \
  -e REIN_ADMIN_TOKEN="$(openssl rand -hex 32)" \
  -e REIN_ENCRYPTION_KEY="$(openssl rand -hex 32)" \
  -e REIN_DB_URL="sqlite:/data/rein.db" \
  -v rein-data:/data \
  ghcr.io/archilea/rein:latest
```

Save both values. The admin token is required for every admin call. Losing the
encryption key makes the database unreadable, so back it up the moment you
generate it.

If you prefer docker compose, copy `.env.example` to `.env` at the repo root,
fill in both secrets, and run `docker compose up -d`. The compose file refuses
to start if either value is missing.

## 2. Create a virtual key

Rein issues its own keys and maps them to upstream provider keys. Your app never sees the real key.

```bash
curl -X POST http://localhost:8080/admin/v1/keys \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "staging-chat-bot",
    "upstream": "openai",
    "upstream_key": "sk-...",
    "daily_budget_usd": 50,
    "month_budget_usd": 1000
  }'
```

Response:

```json
{
  "id": "key_01HXYZ...",
  "name": "staging-chat-bot",
  "upstream": "openai",
  "daily_budget_usd": 50,
  "month_budget_usd": 1000,
  "created_at": "2026-04-10T08:12:44Z",
  "token": "rein_live_abc123..."
}
```

The `token` is returned exactly once. Rein never shows it again. Subsequent
`GET /admin/v1/keys` responses omit both the rein token and the upstream key.
Copy it straight into your secret manager.

## 3. Point your app at Rein

The `rein_live_...` key is a drop-in replacement for your OpenAI key.

```python
from openai import OpenAI

client = OpenAI(
    api_key="rein_live_abc123...",
    base_url="http://localhost:8080/v1",
)

resp = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "Hello"}],
)
```

That is it. Rein transparently forwards the request, parses token usage from the response, and enforces the per-key USD budget before the next upstream call.

## 4. Freeze in an incident

```bash
curl -X POST http://localhost:8080/admin/v1/killswitch \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -d '{"frozen": true}'
```

All `/v1/*` calls return `503 Service Unavailable` with `Retry-After: 60` until you unfreeze. The kill-switch is a single atomic boolean read on the hot path, so the check is effectively free when off and instant when on.

## Next steps

- Full admin API reference (kill-switch, keys, health, version) with copy-paste curl: [docs/admin-api.md](./admin-api.md).
- How metering, streaming, and the kill-switch fit together: [docs/architecture.md](./architecture.md).
- Hard-caps, soft-cap caveat, and the in-process meter limitation: the Budgets section of the main [README](../README.md#budgets).
