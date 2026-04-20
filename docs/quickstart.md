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
    "month_budget_usd": 1000,
    "rps_limit": 10,
    "rpm_limit": 300,
    "max_concurrent": 20
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
  "rps_limit": 10,
  "rpm_limit": 300,
  "max_concurrent": 20,
  "created_at": "2026-04-10T08:12:44Z",
  "token": "rein_live_abc123..."
}
```

The `token` is returned exactly once. Rein never shows it again. Subsequent
`GET /admin/v1/keys` responses omit both the rein token and the upstream key.
Copy it straight into your secret manager.

`rps_limit` and `rpm_limit` bound arrival velocity; `max_concurrent` bounds
work-in-progress. Both default to zero (unlimited). An optional `expires_at`
(RFC3339 UTC) auto-revokes the key at the scheduled instant, handy for
contractors or incident-response break-glass tokens. For the full list of
optional fields and the multi-replica caveat for the brakes, see
[admin-api.md](admin-api.md).

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

## 3a. Point a key at an OpenAI-compatible provider

Most "famous" LLM providers outside OpenAI and Anthropic (Groq, Together, Fireworks, DeepSeek, xAI Grok, OpenRouter, Perplexity, Cerebras, and local vLLM / Ollama / LocalAI) speak the exact OpenAI wire protocol. Rein can reach any of them via the existing OpenAI adapter by overriding the upstream base URL on a per-key basis. No new adapter, no process-wide reconfiguration, no separate Rein instance.

```bash
curl -X POST http://localhost:8080/admin/v1/keys \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "groq-prod",
    "upstream": "openai",
    "upstream_key": "gsk_real_groq_key",
    "upstream_base_url": "https://api.groq.com/openai",
    "daily_budget_usd": 25
  }'
```

The `upstream_base_url` convention is **"everything up to but not including the `/v1/` segment"** that Rein prepends on every outbound request. Many OpenAI-compatible providers mount their API under a path prefix, so you include that prefix in the base URL:

| Provider | `upstream_base_url` |
|---|---|
| OpenAI (default, no override needed) | `https://api.openai.com` |
| Groq | `https://api.groq.com/openai` |
| OpenRouter | `https://openrouter.ai/api` |
| Fireworks | `https://api.fireworks.ai/inference` |
| Together | `https://api.together.xyz` |
| DeepSeek | `https://api.deepseek.com` |
| xAI Grok | `https://api.x.ai` |
| Local vLLM / Ollama / LocalAI | `http://127.0.0.1:11434` |

Rein normalizes the URL at create time. `https` is required for non-loopback hosts, loopback `http` is accepted for local providers, query strings and fragments are rejected, and a trailing slash on the path is stripped. The same key still uses the embedded pricing table. Provider-specific models that are not in the table log a loud WARN line ("model not in pricing table; spend not recorded") the first time they are seen and rate-limit to once per minute after the initial burst, so operators discover gaps immediately rather than at the end of the month. Anthropic-compatible providers are not supported in 0.2.

Azure OpenAI is not supported via this override because it uses a deployment-keyed path shape (`/openai/deployments/{deployment}/chat/completions?api-version=...`) that does not fit a base URL override; a dedicated Azure adapter is tracked separately.

## 3b. Add pricing for a provider-specific model

Rein ships with an embedded pricing table covering OpenAI and Anthropic models. When you point a key at Groq, Fireworks, or another OpenAI-compatible provider, the models those providers serve (for example `llama-3.3-70b-versatile`) are **not in the embedded table**, so the unknown-model WARN fires and spend is not recorded for that request. Budgets on that key therefore do not trigger on spend alone — they become observability-only until the model is priced.

The operator-editable pricing file fixes that without a Rein release. Create a `rein.json`, make it available to the process, and Rein merges your entries on top of the embedded table at startup. The file can be reloaded at runtime with `SIGHUP` (or an optional background poll on Kubernetes deployments that prefer ConfigMap file-watching).

**Rein resolves the config file path in this order**:

1. If `REIN_CONFIG_FILE` is set, use it.
2. Otherwise if `/etc/rein/rein.json` exists, use it (**no env var needed for Kubernetes ConfigMap mounts at this path**).
3. Otherwise run zero-config against the embedded table only.

The startup log records which rule fired (`source=env_var`, `source=default_path`, or `source=embedded_only`) so the active source is visible in the first few lines of output.

Minimal `rein.json`:

```json
{
  "version": "1",
  "source": "operator notes, optional",
  "fetched_at": "2026-04-11",
  "models": {
    "openai": {
      "llama-3.3-70b-versatile": { "input_per_mtok": 0.59, "output_per_mtok": 0.79 },
      "deepseek-v3":             { "input_per_mtok": 0.14, "output_per_mtok": 0.28 }
    }
  }
}
```

A few things to know:

- The outer key is always `"openai"` even for Groq or DeepSeek models. The pricer's axis matches Rein's adapter (wire protocol), not the vendor brand — and every OpenAI-compatible provider rides the OpenAI adapter per [Section 3a](#3a-point-a-key-at-an-openai-compatible-provider).
- **Prices are per million tokens in USD**, matching the embedded `internal/meter/pricing.json` exactly. An operator can copy that file as a starting point and edit it.
- **Zero prices are allowed** (free tiers, local-hosted models) and log an INFO line per zero entry so you can tell at a glance which models you have priced to zero.
- Validation is **strict all-or-nothing**: a single entry with a negative price rejects the whole file. A failed reload at runtime logs an ERROR and keeps the previous snapshot active — a bad config cannot take down a running process.
- **Mount the file into the container**. Do not bake it into the image — that defeats the point of hot-reload. On Docker: `-v $(pwd)/rein.json:/etc/rein/rein.json:ro`. On Kubernetes: mount a ConfigMap at `/etc/rein/rein.json` and omit any `REIN_CONFIG_FILE` env var — the hybrid resolution picks up the default path automatically.

Run with a pricing override file (the env var is optional — the default path is enough):

```bash
docker run -d --name rein -p 8080:8080 \
  -e REIN_ADMIN_TOKEN="$(openssl rand -hex 32)" \
  -e REIN_ENCRYPTION_KEY="$(openssl rand -hex 32)" \
  -v "$(pwd)/rein.json:/etc/rein/rein.json:ro" \
  ghcr.io/archilea/rein:latest
```

Equivalent Kubernetes snippet (no env var, ConfigMap mounted at the default path):

```yaml
volumes:
  - name: rein-pricing
    configMap:
      name: rein-pricing
volumeMounts:
  - name: rein-pricing
    mountPath: /etc/rein
    readOnly: true
# No REIN_CONFIG_FILE env var needed — Rein picks up /etc/rein/rein.json
# via the default-path rule.
```

Reload after editing the file:

```bash
# bare metal
kill -HUP $(pidof rein)

# Docker
docker kill --signal=HUP rein

# systemd
systemctl reload rein
```

Rein emits an `INFO config reload succeeded` log line with the new model count on success, or `ERROR config reload failed, keeping previous snapshot active` with the error and the active snapshot's model count on failure. The previous snapshot stays active across a failed reload by design — you can fix the file and send another SIGHUP.

For Kubernetes deployments where sending signals to a pod requires `kubectl exec`, set `REIN_CONFIG_POLL_INTERVAL=30s` and Rein will check the file's mtime on that cadence and reload on change. The poll must be between `1s` and `1h` (inclusive); anything outside that range is a fatal startup error. Editor-safe: file-rename on save (vim, emacs) is handled correctly because Rein re-stats the path each tick rather than holding a file descriptor across reloads.

## 4. Freeze in an incident

```bash
curl -X POST http://localhost:8080/admin/v1/killswitch \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -d '{"frozen": true}'
```

All `/v1/*` calls return `503 Service Unavailable` with `Retry-After: 60` until you unfreeze. The kill-switch is a single atomic boolean read on the hot path, so the check is effectively free when off and instant when on.

## 5. Rolling deploys on Kubernetes

Rein supports graceful drain for rolling updates. On `SIGTERM` or `SIGINT` the process flips a drain flag and gives in-flight requests up to `REIN_SHUTDOWN_GRACE` (default `30s`, bounded to `[1s, 5m]`) to finish before force-closing any connections still active. New `/v1/*` requests during drain receive `503 Service Unavailable` with `Retry-After: 5` and the structured `{"error":{"code":"draining", ...}}` envelope so clients retry against another replica via your load balancer.

Two HTTP probes split the signals a Kubernetes pod needs:

- `/healthz` — **liveness**. Returns `200 OK` for the entire process lifetime. A liveness-fail means the process is stuck, so Kubernetes restarts the pod.
- `/readyz` — **readiness**. Returns `200 {"status":"ready"}` normally, `503 {"status":"draining"}` once the drain flag is set. A readiness-fail means the pod should be removed from the Service's endpoint pool so no new traffic arrives, but the pod must NOT be restarted — that would kill the in-flight requests the drain window is protecting.

Probe wiring:

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 8080
  periodSeconds: 10
readinessProbe:
  httpGet:
    path: /readyz
    port: 8080
  periodSeconds: 5
```

Set `terminationGracePeriodSeconds` on the pod to at least `REIN_SHUTDOWN_GRACE + 5s` so Kubernetes does not `SIGKILL` Rein before its own drain window elapses.

A second `SIGTERM`/`SIGINT` during drain force-closes immediately: operators who want to cut a bad deploy short do not have to wait out the full grace window.

## Next steps

- Full admin API reference (kill-switch, keys, health, version) with copy-paste curl: [docs/admin-api.md](./admin-api.md).
- How metering, streaming, and the kill-switch fit together: [docs/architecture.md](./architecture.md).
- Hard-caps, soft-cap caveat, and the in-process meter limitation: the Budgets section of the main [README](../README.md#budgets).
