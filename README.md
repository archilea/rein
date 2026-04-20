<div align="center">

# Rein

### A modern, lightweight reverse proxy for LLMs.

Rein is a small, auditable Go reverse proxy that sits between your apps and the major LLM providers. It swaps virtual keys for real upstream credentials at the edge, meters token spend from streaming and non-streaming responses, enforces hard USD budget caps per key, rate-limits request velocity (RPS and RPM per key), caps concurrent in-flight requests per key, and exposes an instant global kill-switch for incident response. Native adapters ship for OpenAI and Anthropic, with a per-key base URL override for any OpenAI-compatible provider. Single static binary, pure Go, no CGO. No telemetry, ever.

[![CI](https://github.com/archilea/rein/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/archilea/rein/actions/workflows/ci.yml)
![License](https://img.shields.io/badge/license-MIT-blue.svg)
![Go](https://img.shields.io/badge/go-1.25%2B-00ADD8)
![Status](https://img.shields.io/badge/status-alpha-orange)

<br />

<img src="docs/demo.gif" alt="rein kill-switch demo" width="760" />

</div>

---

## Core features

Reverse proxy:

1. **Reverse proxy** for OpenAI (`/v1/chat/*`, `/v1/completions`, `/v1/embeddings`, `/v1/models`, `/v1/audio/*`, `/v1/images/*`) and Anthropic (`/v1/messages`). Streaming-aware with pure passthrough. Per-key upstream base URL override unlocks any OpenAI-compatible provider without additional adapter code.
2. **Virtual keys** (`rein_live_*`) that wrap your real upstream credentials. Clients never see the upstream key. Rotating a compromised credential is one admin API call; old rein tokens keep working until you revoke them.
3. **Admin API** for virtual key create, list, update, revoke, and kill-switch, all protected by a single bearer token compared in constant time.

Cost and safety controls:

4. **Hard budget caps** per virtual key, daily and monthly USD. Rein checks accumulated spend before every upstream fetch; breach returns `402 Payment Required` and the upstream is never called.
5. **Instant global kill-switch** via `POST /admin/v1/killswitch`. One HTTP call and every `/v1/*` request returns `503 Service Unavailable` with `Retry-After: 60` until someone unfreezes. A single `atomic.Bool` read on the hot path, effectively free when off, instant when on.
6. **Per-key rate limiting** with RPS (requests per second) and RPM (requests per minute) caps. A sliding window counter bounds request velocity before the upstream is contacted. Over-limit requests return `429 Too Many Requests` with `Retry-After`.
7. **Per-key concurrency cap** (`max_concurrent`) bounds the number of simultaneously in-flight requests per virtual key. The nginx `limit_conn` analog: rate limit bounds arrival velocity, concurrency cap bounds work-in-progress. Together they bound budget overshoot to `max_concurrent x max_request_cost`. Over-limit requests return `429 Too Many Requests` with `Retry-After: 1`.
8. **Per-key upstream request timeout** (`upstream_timeout_seconds`, `[0, 3600]`) bounds the wall-clock duration of any one upstream call. The `proxy_read_timeout` analog for LLM traffic, sized for reasoning-model tail latency. Non-streaming responses that exceed the ceiling return `504 Gateway Timeout` with `Retry-After: 1`; streaming responses close the SSE stream cleanly with a final comment line and record whatever usage was parsed before the cancel. Default `0` means unlimited and pays zero hot-path cost.
9. **Spend metering** from both JSON and SSE streaming responses, priced against an embedded pricing table verified against OpenAI and Anthropic vendor docs. Dated model snapshots (`claude-opus-4-5-20251101`, `gpt-4o-2024-08-06`) resolve to their base entries automatically.
10. **Streaming usage auto-inject.** For OpenAI chat completions, Rein injects `stream_options.include_usage: true` into the outbound body so streaming clients cannot silently bypass budget enforcement. An explicit client opt-out is respected and logged.
11. **Graceful drain for rolling deploys.** `SIGTERM` / `SIGINT` flips a drain flag and gives in-flight LLM calls up to `REIN_SHUTDOWN_GRACE` (default `30s`, bounded to `[1s, 5m]`) to finish before force-closing. New `/v1/*` requests during drain receive `503 Service Unavailable` with `Retry-After: 5` and the structured `draining` code so clients retry on another replica. The liveness-vs-readiness split lets Kubernetes remove the pod from the Service pool (`/readyz` flips) without restarting it (`/healthz` stays green), protecting the in-flight work the drain window is there to finish. A second signal within the window force-closes immediately for operators who want to cut a bad deploy short.

Operational foundation:

12. **Durable SQLite keystore + spend meter** via `modernc.org/sqlite` (pure Go, no CGO). WAL mode enabled. Spend totals survive process restart, OOM, and `kill -9`. Single static binary deploys anywhere.
13. **Encryption at rest** for the upstream key column using AES-256-GCM. Rein refuses to start without `REIN_ENCRYPTION_KEY`, so plaintext credentials cannot land on disk by accident. Ciphertext carries a `v1:` tag so future algorithm rotations do not require a schema migration.
14. **Offline encryption key rotation** via the `rein-rotate-keys` CLI. Rotates the AES-256-GCM key that wraps the upstream credential column, idempotent and atomic. Operator runbook in `docs/runbooks/key-rotation.md`.

## Who this is for

Rein exists specifically for teams who:

- Want a **lightweight reverse proxy** as their first layer in front of direct provider calls, not a refactor target
- Already call their LLM provider SDK directly and **do not want to refactor to a schema-translation gateway**
- **Already run an AI gateway of any shape** and want an independent safety net that platform or SRE can trigger without touching the AI team's stack
- Want a proxy **small enough that their security team can audit it end-to-end in an afternoon**
- Want a **kill-switch** they can trigger without coordinating with another team
- Want **no telemetry**, no phone-home, no SaaS account, no signup

## What Rein deliberately does not do

- **Schema translation.** Call OpenAI at OpenAI, Anthropic at Anthropic. Rein is a pure reverse proxy.
- **Routing or fallbacks.** Use a dedicated routing gateway.
- **Observability, traces, or evals.** Use Langfuse, Helicone, or Braintrust.
- **Model marketplaces.** OpenAI and Anthropic are supported. Other providers only if adding them does not break the size ceiling.
- **Ship a dashboard or web UI.** The admin interface is a small HTTP API, driven by `curl` or whatever client your team already uses. Every UI is a supply chain we do not want to own on top of a brake. Full cookbook in [docs/admin-api.md](docs/admin-api.md).
- **Telemetry.** Rein will never phone home. This is a hard commitment, not a current default.

## Audit-friendly ceiling

Rein is intentionally small. A security review can cover every line in an afternoon. The disciplines we enforce in CI to keep it that way are a ceiling on direct non-stdlib dependencies, a ceiling on the number of modules that reach the production binary, and a ceiling on compressed image size. These are internal design targets, not a public SLA: they exist because drift is easier to catch early than it is to reverse.

**Current state**, as of the latest release:

- **1 direct non-stdlib dependency** (`modernc.org/sqlite`)
- **10 compiled production modules** (measured with `go list -deps ./cmd/rein` on Linux, excluding stdlib and test-only deps)
- **~14 MB compressed amd64 image**

If those numbers change materially in a future release, the `CHANGELOG.md` entry for that release will say why. The specific CI thresholds live in `.github/workflows/ci.yml` as grep-able literals so the history of every budget change is visible in `git log` on that file.

Source LOC is not a pinned number. It is a private design forcing-function we use when sizing proposals. If a feature would push Rein past an audit-friendly shape, it does not belong here. We will document the pattern in an issue and point you at a complementary tool that already solves it.

## Works with

Rein is designed as a **sidecar**, not a replacement. It sits between your application and whatever LLM stack you already have.

| Setup | How Rein fits |
|---|---|
| Direct SDK calls to any supported provider | Point the SDK's `base_url` at Rein. Two env vars. |
| AI gateway already in place | Put Rein in front of your existing gateway for an independent kill-switch SRE can trigger without touching the gateway's config. See the [model aliases caveat](#note-on-model-aliases-in-front-end-gateways) below before enabling budgets. |
| Custom in-house gateway | Rein can sit on either side depending on what you want audited end-to-end. |

### Note on model aliases in front-end gateways

Rein's embedded pricing table keys off **real vendor model IDs** (`gpt-4o`, `claude-sonnet-4-5`, dated snapshots like `claude-opus-4-5-20251101` get stripped to their base entry automatically).

If you put Rein behind another AI gateway that rewrites the `model` field in the upstream response to a friendly alias (`haiku`, `sonnet`, `auto`, and so on), Rein's pricer looks up the alias, correctly reports "unknown model", and emits a loud WARN log line, **but it does not record spend for that response**. Silent result: a `daily_budget_usd` cap on that key will not fire, because the meter never sees a non-zero increment. The kill-switch still works; only metering is affected.

Three options for operators in this topology:

1. **Configure the upstream gateway to pass real vendor model IDs through**, or rename its aliases to match (for example, naming an entry `claude-3-5-haiku-20241022` instead of `haiku`). Rein's date-stripping fallback then resolves dated snapshots automatically and caps fire normally. Cleanest fix.
2. **Enforce budgets at the upstream gateway** using whatever per-key spend limits it offers, and use Rein purely as the independent kill-switch SRE can trigger. This matches a layered-responsibility story: the gateway owns routing and budgets, Rein owns the emergency brake.
3. **Treat budgets as observability-only** in this topology and trust the kill-switch as the hard stop.

If you call OpenAI or Anthropic directly (or through a gateway that passes real model IDs through unchanged), none of this applies: the vendor returns real model IDs and Rein's pricer matches them out of the box.

## Quickstart

```bash
# 1. Run Rein (both secrets are generated once and stored by you)
docker run -d --name rein -p 8080:8080 \
  -e REIN_ADMIN_TOKEN="$(openssl rand -hex 32)" \
  -e REIN_ENCRYPTION_KEY="$(openssl rand -hex 32)" \
  ghcr.io/archilea/rein:latest

# 2. Mint a virtual key that wraps your real OpenAI key
curl -X POST http://localhost:8080/admin/v1/keys \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "prod-app",
    "upstream": "openai",
    "upstream_key": "sk-your-real-openai-key"
  }'
# -> {"id":"key_...","token":"rein_live_...", ...}

# 3. Point your SDK at Rein using the rein_live_ token
export OPENAI_API_KEY=rein_live_...
export OPENAI_BASE_URL=http://localhost:8080/v1
```

Anthropic works the same way. Rein handles the header translation automatically.

```bash
# Mint a virtual key for Anthropic
curl -X POST http://localhost:8080/admin/v1/keys \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "claude-app",
    "upstream": "anthropic",
    "upstream_key": "sk-ant-your-real-anthropic-key"
  }'

# Call Claude through Rein (always use Authorization: Bearer, not x-api-key)
curl http://localhost:8080/v1/messages \
  -H "Authorization: Bearer rein_live_..." \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-20250514",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

Inbound requests to Rein always use `Authorization: Bearer rein_live_...` regardless of the upstream provider. Rein translates to the correct upstream format (`Authorization: Bearer` for OpenAI, `x-api-key` for Anthropic) on the outbound side.

The `token` is returned once, on create. Rein never shows it again: list and get responses omit the token and the upstream key entirely. Store it in your secret manager the moment you see it.

Keys are persisted in a local SQLite database (`./rein.db` by default). The `upstream_key` column is encrypted at rest with AES-256-GCM using `REIN_ENCRYPTION_KEY`. Rein refuses to start if the key is missing, so plaintext credentials can never land on disk by accident. Lose the encryption key and the database becomes unreadable: treat it like any other root secret. Override the DB location with `REIN_DB_URL=sqlite:/var/lib/rein/rein.db`, or use `REIN_DB_URL=memory` for an ephemeral in-memory store.

For a longer walkthrough with a virtual-key example and an incident runbook, see [docs/quickstart.md](docs/quickstart.md).

## Verifying the image

Every tagged Rein release is signed with [cosign](https://docs.sigstore.dev/cosign/overview/) using a committed public key at [`cosign.pub`](./cosign.pub). Before running Rein in production, verify the image came from this repo and has not been tampered with.

Requires `cosign` v2.6 or later (ideally v3.0+). Install from the [sigstore/cosign releases page](https://github.com/sigstore/cosign/releases). Signatures are stored in the new protobuf bundle format as OCI 1.1 referring artifacts, which older cosign clients cannot read.

```bash
cosign verify \
  --key https://raw.githubusercontent.com/archilea/rein/main/cosign.pub \
  ghcr.io/archilea/rein:0.2.0
```

Replace `0.2.0` with the release you are pulling. The command exits `0` and prints the verified payload when the signature is valid. A non-zero exit means do not run the image.

You can also pin the key to a specific release tag to defend against a compromised `main` replacing the public key:

```bash
cosign verify \
  --key https://raw.githubusercontent.com/archilea/rein/0.2.0/cosign.pub \
  ghcr.io/archilea/rein:0.2.0
```

The image is signed by canonical digest, so every tag (`0.2.0`, `0.2`, `latest`) that resolves to the same digest passes the same verification.

## Budgets

Every virtual key can carry a `daily_budget_usd` and `month_budget_usd` cap. Rein parses `usage` from every upstream response, looks up USD cost in an embedded, vendor-verified pricing table (OpenAI and Anthropic models, see `internal/meter/pricing.json`), and adds it to the key's running total. On the next inbound request, Rein checks the totals before forwarding. If either cap is reached, the request returns `402 Payment Required` with a clear body and the upstream is never called.

```bash
curl -X POST http://localhost:8080/admin/v1/keys \
  -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "prod-app",
    "upstream": "openai",
    "upstream_key": "sk-...",
    "daily_budget_usd": 100,
    "month_budget_usd": 2000
  }'
```

Two things to know, because honesty is the whole point of this project:

1. **Budgets are soft in one narrow sense.** Check runs before the upstream fetch, Record runs after. A burst of N concurrent requests can all pass Check at the same total, so the cap can overshoot by up to `N × average_request_cost`. To bound that overshoot, set `max_concurrent` on the key: per-key concurrency caps act as the work-in-progress brake (nginx `limit_conn` analog), so worst-case overshoot is bounded by `max_concurrent × max_request_cost`. The kill-switch remains the independent hard stop. For real production guarantees, set `daily_budget_usd` with a safety margin below the bill you actually want to cap at.

2. **Totals are durable.** The spend meter is durable when `REIN_DB_URL=sqlite:<path>` (the default). Totals survive a process restart, OOM, or `kill -9`. Each Record is a single SQLite transaction against the same file as the keystore, so a crash between the daily and monthly updates cannot leave them out of sync. Set `REIN_DB_URL=memory` for ephemeral runs (tests or throw-away deployments), where totals reset on restart. Multi-replica deployments are still out of scope for 0.2: per-replica SQLite files do not coordinate, so pin a single replica if you care about global totals.

Budgets use the embedded pricing table under `internal/meter/pricing.json`, which was verified against vendor docs on the date shown in the `fetched_at` field. Verify against your own account's pricing before turning on caps in production. Unknown or newly released models are logged and skipped (they do not count toward spend) so a new model release never breaks the proxy. Operators who want to **override a vendor price** (out-of-date embedded value, or a custom rate from their account) or **add pricing for a provider-specific model** (Groq's `llama-3.3-70b-versatile`, Fireworks models, DeepSeek, etc.) can drop a `rein.json` at `/etc/rein/rein.json` — or point at any path via `REIN_CONFIG_FILE` — and reload with `SIGHUP`. See [docs/quickstart.md § 3b](docs/quickstart.md) for the full file format, hybrid resolution rules, and Kubernetes ConfigMap guidance.

**Streaming** is fully supported. Rein tees SSE response bodies as they flow to your client and parses the token usage chunks in line. For OpenAI chat completions, Rein automatically injects `stream_options.include_usage: true` into the outbound request body so the upstream returns a final usage chunk (the client sees the stream unchanged). For Anthropic, usage is parsed from the native `message_start` and `message_delta` events. If a client explicitly sets `stream_options.include_usage: false` on an OpenAI request, Rein respects that choice and logs a warning: spend for that request will not be recorded.

## Kill-switch

Rein exposes a global kill-switch through the admin API. It is a single `atomic.Bool` on the Rein process. When flipped, every `/v1/*` request returns `503 Service Unavailable` with `Retry-After: 60`, regardless of virtual key, model, or upstream. The upstream is never contacted. No restart, no config edit, instantly reversible.

```bash
# Check current state
curl -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  http://localhost:8080/admin/v1/killswitch
# -> {"frozen": false}

# Freeze everything
curl -X POST -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -d '{"frozen": true}' \
  http://localhost:8080/admin/v1/killswitch
# -> {"frozen": true}

# Every subsequent /v1/* call now returns:
#   HTTP/1.1 503 Service Unavailable
#   Retry-After: 60
#   Content-Type: application/json
#   {"error":{"code":"kill_switch_engaged","message":"rein is frozen: kill-switch engaged"}}

# Unfreeze
curl -X POST -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
  -d '{"frozen": false}' \
  http://localhost:8080/admin/v1/killswitch
```

The check runs first in the proxy pipeline, before key resolution or budget check. A single atomic boolean read on the hot path is effectively free, so leaving the switch wired has no measurable overhead.

The full admin surface (kill-switch, virtual keys, health, version) is documented as copy-pasteable curl in [docs/admin-api.md](docs/admin-api.md). Rein does not ship a dashboard by design.

## Architecture

```mermaid
flowchart LR
    Client["Client<br/>(app, SDK,<br/>or gateway)"]
    Rein["Rein"]
    Upstream["OpenAI<br/>or<br/>Anthropic"]

    Client -- "rein_live_* + Bearer" --> Rein
    Rein -- "real upstream key" --> Upstream
    Upstream -- "response + token usage" --> Rein
    Rein -- "response stream" --> Client
```

Inside Rein, every `/v1/*` request passes through eight checks in order:

1. **Kill-switch check.** A single `atomic.Bool` read. If frozen, returns `503 Service Unavailable` with `Retry-After: 60`. No key lookup, no upstream dial.
2. **Key lookup.** Resolves the inbound `Authorization: Bearer rein_live_...` against the SQLite keystore, with AES-256-GCM decrypt of the `upstream_key` column. Returns `401` if missing, invalid, or revoked.
3. **Budget check.** Reads the key's daily and monthly USD totals from the spend meter. Returns `402 Payment Required` if either cap is reached. The upstream is never contacted.
4. **Rate limit check.** If the key has `rps_limit` or `rpm_limit` set, checks the in-memory sliding window counter. Returns `429 Too Many Requests` with `Retry-After` if either cap is breached. The upstream is never contacted.
5. **Concurrency cap check.** If the key has `max_concurrent` set, an atomic per-key counter is incremented; if the new value exceeds the cap, it is decremented and the request returns `429 Too Many Requests` with `Retry-After: 1`. A deferred Release frees the slot after the adapter returns, covering happy path, upstream error, client disconnect, and panic unwind alike. Unlimited keys (`max_concurrent: 0`) short-circuit before any state lookup and pay zero cost.
6. **Upstream timeout wrap.** If the key has `upstream_timeout_seconds` set, the request context is wrapped with `context.WithTimeout`. Non-streaming responses that exceed the deadline return `504 Gateway Timeout` with `Retry-After: 1`; streaming responses close the SSE stream cleanly with a final comment line. Unlimited keys (`upstream_timeout_seconds: 0`) skip the wrap and pay zero cost.
7. **Forward.** The adapter swaps the rein bearer for the real upstream key (`Authorization: Bearer` for OpenAI, `x-api-key` for Anthropic) and proxies through a tuned `httputil.ReverseProxy`.
8. **Meter record on the response path.** The adapter parses token usage from the upstream response (streaming or buffered), computes USD cost from the embedded pricing table, and records the amount. The next request's Check sees the updated total.

The kill-switch sits first so it can shed load with a single atomic read during an incident. Metering runs on the response path, so the check-then-record ordering is what creates the "soft cap under concurrent bursts" caveat documented in the Budgets section.

For deeper detail see [docs/architecture.md](docs/architecture.md).

## Performance

Rein is a thin layer. The only honest question is whether it is honest about being thin. Short answer: **Rein's full production hot path adds about 34 microseconds per request on a 4-core Apple M5**, measured with the production SQLite keystore, AES-256-GCM decrypt on every virtual-key lookup, and budget enforcement enabled. With the durable SQLite meter enabled (`REIN_DB_URL=sqlite:<path>`, the default), the same hot path measures about 72 microseconds per request on a 4-core Apple M5 (macOS 15, APFS). The ~38 microsecond increment is two SQLite round trips (Check SELECT + Record transaction with WAL append + fdatasync) plus pool serialization; on Linux NVMe with cheaper fdatasync, expect the increment closer to 15 to 25 microseconds. The durable meter prioritizes application-crash durability (process crash, OOM, kill -9) via WAL. Power-loss durability for the last few seconds of writes is bounded by the SQLite WAL default (synchronous=NORMAL), which single-replica operators can strengthen later if their threat model warrants it.

### Measured numbers (Apple M5, Go 1.26, 4 parallel workers)

| Path | Time per req | Req/s | Allocs | What it measures |
|---|---|---|---|---|
| Normal hot path (SQLite + budget) | **34.0 µs** | **~29,400** | 308 | Full production config |
| MemStore + budget (no SQLite) | 29.6 µs | ~33,800 | 243 | SQLite adds ~5 µs |
| Kill-switch engaged (SQLite) | **11.6 µs** | **~86,600** | 96 | Fast-path rejection |
| Per-key base URL override (#24) | 35.3 µs | ~28,300 | 310 | One sync.Map load on the cached URL |
| Rate-limited path (#26) | 34.8 µs | ~28,800 | 314 | Sliding window check + counter increment |
| Concurrency cap, unlimited (#27) | 34.7 µs | ~28,800 | 308 | `max_concurrent: 0` short-circuit |
| Concurrency cap, limited (#27) | 34.1 µs | ~29,300 | 310 | One atomic Add + deferred Release |
| Multi-key concurrency, 100 keys (#27) | 34.4 µs | ~29,000 | 308 | Cache-line padding prevents false sharing |
| Upstream timeout, unlimited (#30) | 34.5 µs | ~28,900 | 311 | `upstream_timeout_seconds: 0` skip |
| Upstream timeout, limited, not firing (#30) | 34.7 µs | ~28,700 | 321 | One `context.WithTimeout` + deferred cancel |

Every 0.2 and 0.3 brake (rate limit, concurrency cap, base URL override, expires_at, upstream timeout) adds **zero measurable overhead** versus the unbraked baseline. Budget enforcement, rate limiting, concurrency capping, and upstream timeouts each cost a handful of nanoseconds amortized across the request; the bottleneck is everywhere except Rein.

### Why this does not matter in production

Real LLM calls take 200 ms to 30 seconds. Rein's overhead is 34 µs. For any real request Rein contributes between 0.0001% and 0.02% of total time, and the upstream model is always the bottleneck.

Throughput scales with concurrency via Little's Law: `RPS = concurrent_in_flight / upstream_latency`. Rein's contribution to the denominator is rounding error.

| Upstream latency | Rein's share of total time | Concurrency to sustain 1,000 req/s |
|---|---|---|
| 200 ms (gpt-4o-mini, short reply) | 0.017% | ~200 |
| 500 ms (gpt-4o, typical) | 0.007% | ~500 |
| 2,000 ms (gpt-4o, long context) | 0.002% | ~2,000 |
| 30,000 ms (reasoning model) | 0.0001% | ~30,000 |

On a 4-core VM, a single Rein instance can sustain thousands of concurrent in-flight LLM requests before it becomes the bottleneck, which in any realistic deployment is well over 10,000 req/s of actual traffic.

### Honest caveats

- **These numbers are from a laptop.** Bigger servers with more cores should scale roughly linearly, but we have not benchmarked at scale yet.
- **The mock upstream is in-process.** `httptest.NewServer` uses a real loopback listener but adds near-zero latency. Real TCP and TLS add a few hundred microseconds per new connection, amortized to near-zero with keep-alive.
- **Streaming is not in the table.** SSE throughput depends on chunk size, not Rein's code path. The tee reader adds one copy per `Read`, which is well under 1 ms for typical chat completions.
- **The benchmark table above reflects the in-memory meter path.** With the durable SQLite meter enabled (`REIN_DB_URL=sqlite:<path>`, the default), the same hot path measures about 72 microseconds per request on a 4-core Apple M5 (macOS 15, APFS). The ~38 microsecond increment is two SQLite round trips (Check SELECT + Record transaction with WAL append + fdatasync) plus pool serialization; on Linux NVMe with cheaper fdatasync, expect the increment closer to 15 to 25 microseconds. The durable meter prioritizes application-crash durability (process crash, OOM, kill -9) via WAL. Power-loss durability for the last few seconds of writes is bounded by the SQLite WAL default (synchronous=NORMAL), which single-replica operators can strengthen later if their threat model warrants it.

### Reproducing

The benchmarks are committed to the repo. On any machine with Go 1.25+:

```bash
git clone https://github.com/archilea/rein
cd rein
# Production hot path (SQLite + encryption + budgets)
REIN_BENCH_QUIET=1 go test ./internal/proxy \
  -bench BenchmarkRein_SQLite_WithBudget_ZeroLatency \
  -benchtime=3s -cpu=4 -run=^$

# Kill-switch fast path
REIN_BENCH_QUIET=1 go test ./internal/proxy \
  -bench BenchmarkRein_Frozen \
  -benchtime=3s -cpu=4 -run=^$

# Full suite (takes ~60 seconds with pauses for socket drain)
REIN_BENCH_QUIET=1 go test ./internal/proxy -bench . -benchtime=3s -cpu=4 -run=^$
```

Benchmark source: [`internal/proxy/bench_test.go`](internal/proxy/bench_test.go). Numbers will vary by CPU, Go version, and parallel worker count. Reproduce on your own hardware before relying on them in production sizing.

## Configuration

Rein is configured via environment variables. Only `REIN_ADMIN_TOKEN` is required; the binary will fail to start without it.

| Variable | Default | Description |
|---|---|---|
| `REIN_PORT` | `8080` | HTTP port |
| `REIN_ADMIN_TOKEN` | _(required)_ | Bearer token for the admin API |
| `REIN_DB_URL` | `sqlite:./rein.db` | `sqlite:<path>` for durable storage, or `memory` for ephemeral |
| `REIN_ENCRYPTION_KEY` | _(required for sqlite)_ | 64-char hex (32 bytes) AES-256-GCM key for at-rest encryption |
| `REIN_OPENAI_BASE` | `https://api.openai.com` | OpenAI upstream override |
| `REIN_ANTHROPIC_BASE` | `https://api.anthropic.com` | Anthropic upstream override |
| `REIN_CONFIG_FILE` | _(unset)_ | Operator-editable pricing overrides file. If unset, Rein probes `/etc/rein/rein.json`; if that is also absent, runs against just the embedded pricing table. See [docs/quickstart.md § 3b](docs/quickstart.md). |
| `REIN_CONFIG_POLL_INTERVAL` | _(unset)_ | Opt-in background poll interval for `REIN_CONFIG_FILE`. Duration in Go `time.ParseDuration` form (e.g. `30s`, `5m`). Bounded to `[1s, 1h]`; out-of-range values fail at startup. Unset means SIGHUP-only reload. |

## Roadmap

Kept deliberately short. Features that would break the size ceiling are not here.

- [x] `0.1` OpenAI and Anthropic reverse proxy
- [x] `0.1` Kill-switch (instant freeze via admin API)
- [x] `0.1` Virtual keys with upstream key swap
- [x] `0.1` Admin endpoints for key creation, listing, and revocation
- [x] `0.1` Durable keystore (SQLite, pure-Go driver, no CGO)
- [x] `0.1` Encrypt `upstream_key` at rest (AES-256-GCM, env-scoped key)
- [x] `0.1` Budget enforcement (daily and monthly caps per key)
- [x] `0.1` Embedded vendor-verified pricing table for OpenAI and Anthropic
- [x] `0.1` Streaming token usage extraction (SSE) for OpenAI and Anthropic
- [x] `0.2` Per-key upstream base URL override for any OpenAI-compatible provider (Groq, Fireworks, OpenRouter, DeepSeek, xAI, local vLLM/Ollama, ...)
- [x] `0.2` Operator-editable pricing overrides with SIGHUP + poll-based hot reload
- [x] `0.2` CI-enforced audit-friendly ceilings (direct dep count, compiled dep count, compressed image size)
- [x] `0.2` Per-key request rate limiting (RPS + RPM, sliding window counter)
- [x] `0.2` Per-key max concurrent in-flight requests (`max_concurrent`, nginx `limit_conn` analog)
- [x] `0.2` Durable SQLite-backed meter (spend survives restart)
- [x] `0.2` Encryption key rotation tool (`rein-rotate-keys`, offline)
- [x] `0.3` Structured JSON errors on all proxy endpoints (#75)
- [x] `0.3` `PATCH /admin/v1/keys/{id}`: update a key's caps without re-minting (#74)
- [x] `0.3` Per-key `expires_at` with automatic revocation (#77)
- [ ] `0.3` Per-key model allowlist (#28)
- [x] `0.3` Per-key upstream request timeout (#30)
- [x] `0.3` Graceful shutdown: drain flag, configurable grace, proxy-side 503, `/readyz` (#76)

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) for the flow. Issues tagged `good first issue` are a good place to start.

Scope-of-work expectations: Rein stays small. If your PR adds more than a few hundred lines, please open an issue first so we can agree it fits the identity. This is reviewer-fatigue guidance, not a hard cap. The enforced size bounds are direct dependency count and compressed image size, not source LOC (see [Audit-friendly ceiling](#audit-friendly-ceiling)).

## Security

Found a vulnerability? Please do not open a public issue. Email `security@archilea.com` and read [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)

## Summary

Rein is a specific shape: a small, auditable reverse proxy that does a deliberately short list of things. The differentiators are architectural, not feature-count.

- **Small on purpose.** CI-enforced ceilings on dependency count and compressed image size keep supply-chain surface tight; a security team can read the whole codebase end-to-end in an afternoon. Current state is published under [Audit-friendly ceiling](#audit-friendly-ceiling) each release. The pricing table is embedded JSON, not Go code, so the binary stays focused on behavior.
- **Pure Go, no CGO.** No Python runtime, no dependency conflicts, no base-image surprises. Runs as a sidecar next to anything.
- **No telemetry, ever.** A hard commitment as a core identity, not a config flag you can turn off later.
- **AES-256-GCM encryption at rest by default.** The process refuses to start without an encryption key, so plaintext credentials cannot land on disk by accident.
- **Instant global kill-switch as a first-class feature.** `POST /admin/v1/killswitch` is one atomic HTTP call that halts every `/v1/*` request in the process, backed by a lock-free `atomic.Bool` on the hot path. Platform or SRE teams can own it independently of whoever owns the rest of the AI stack, so incident response never needs a cross-team handoff.

Rein is built for the moment you need it most: an incident, a runaway bill, a compromised key. Every design choice compounds toward that moment. You can audit the brake end-to-end in an afternoon before you trust it. The kill-switch fast path is a single atomic boolean read, measured in microseconds. Nothing phones home while you are working the problem, and nothing goes to disk without AES-256-GCM wrapping it first.

Staying narrow is the product. If Rein grew a dashboard, a routing layer, or a plugin system, it would stop being Rein.

---

## Contributors

<a href="https://github.com/archilea/rein/graphs/contributors">
  <img src="https://stg.contrib.rocks/image?repo=archilea/rein" alt="Rein contributors" />
</a>

---

<div align="center">

*Maintained by [Archilea](https://archilea.com) and contributors.*

</div>
