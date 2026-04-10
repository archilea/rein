# Changelog

All notable changes to Rein will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Planned for v0.2

- Durable SQLite-backed spend meter so budget totals survive process restart.
- Encryption key rotation tool (`rein-rotate-keys`).

## [0.1.0] - TBD

First public alpha. Single Go binary (pure Go, no CGO). Under 2,000 lines of
non-test code. No telemetry, ever.

### Added

- **Reverse proxy** for OpenAI (`/v1/chat/*`, `/v1/completions`,
  `/v1/embeddings`, `/v1/models`, `/v1/audio/*`, `/v1/images/*`) and Anthropic
  (`/v1/messages`). Path matching is by prefix (`strings.HasPrefix`), so any
  new OpenAI sub-endpoint under `/v1/chat/` is forwarded without a Rein code
  change. Requests are forwarded to the configured upstream base URL and
  responses stream back unchanged.
- **Global kill-switch** via `POST /admin/v1/killswitch`. When engaged, every
  `/v1/*` request returns `503 Service Unavailable` with `Retry-After: 60`
  until unfrozen. Backed by a lock-free `atomic.Bool` for negligible overhead
  on the hot path.
- **Virtual keys** (`rein_live_...`) generated via `POST /admin/v1/keys`, with
  list and revoke endpoints. The secret token is returned exactly once on
  create; subsequent list/get responses omit the token and the upstream key.
- **Durable keystore** backed by SQLite via `modernc.org/sqlite` (pure Go, no
  CGO). WAL mode enabled. Configurable via `REIN_DB_URL=sqlite:<path>` or
  `REIN_DB_URL=memory` for ephemeral runs.
- **Encryption at rest** for the `upstream_key` column using AES-256-GCM with
  a key supplied via `REIN_ENCRYPTION_KEY` (64 hex chars = 32 bytes). Rein
  refuses to start if the key is missing when using the SQLite keystore, so
  plaintext credentials can never land on disk by accident. Ciphertext carries
  a `v1:` tag so future algorithm rotations do not require a schema migration.
- **Per-key budget enforcement** with daily and monthly USD caps. Check runs
  on every request before the upstream fetch; breach returns `402 Payment
  Required` with a clean body. Record runs after the upstream response and
  updates per-day and per-month buckets (UTC-anchored).
- **Embedded vendor-verified pricing table** (`internal/meter/pricing.json`)
  covering current OpenAI models (gpt-4o family, gpt-4.1 family, gpt-5, o1,
  o3, o3-mini, o4-mini) and Anthropic models (Opus 4.6/4.5/4.1/4, Sonnet
  4.6/4.5/4, Haiku 4.5/3.5/3). Date-suffixed model IDs returned by vendors
  (e.g. `claude-opus-4-5-20251101`, `gpt-4o-2024-08-06`) are matched against
  their base entry via trailing-date stripping, so new snapshots do not
  require table updates. Unknown or newly released models are logged and
  skipped rather than blocking traffic.
- **Streaming token usage extraction** for both upstreams. SSE responses are
  teed (not buffered) as they flow to the client. For OpenAI, Rein auto-
  injects `stream_options.include_usage: true` into the outbound request body
  so the upstream returns a final usage chunk; an explicit client opt-out is
  respected and logged. For Anthropic, `input_tokens` is parsed from the
  `message_start` event and the final `output_tokens` from the last
  `message_delta`. Spend is recorded on a background context so client
  disconnects cannot race the meter write.
- **Admin API authentication** via a single bearer token (`REIN_ADMIN_TOKEN`)
  compared in constant time to defeat timing side-channels.
- **Health check** at `GET /healthz` and build info at `GET /version`.
- **Graceful shutdown** with a 15 second timeout on SIGINT / SIGTERM.
- **Tuned outbound transport** shared by all upstream adapters. `httputil.ReverseProxy` defaults to `http.DefaultTransport` which has `MaxIdleConnsPerHost: 2`. This is pathologically low for a proxy that talks to the same upstream host for every request. Rein ships a shared `upstreamTransport()` with `MaxIdleConnsPerHost: 200`, `MaxIdleConns: 1000`, `IdleConnTimeout: 90s`, and explicit dial timeouts. This is standard practice for every production reverse proxy and is the difference between sustained multi-thousand-req/s throughput and running out of ephemeral ports under moderate load.
- **Hardened docker-compose quickstart.** The shipped `docker-compose.yml` refuses to boot unless `REIN_ADMIN_TOKEN` and `REIN_ENCRYPTION_KEY` are set in a `.env` file at the repo root (via `${VAR:?message}` substitution), so a fresh `git clone && docker compose up` can never silently start with a weak default admin token or crash deep in the Go startup path on the missing encryption key. A tracked `.env.example` documents both required variables plus every optional one with its default. `make run` uses the in-memory keystore so contributors hacking on Rein do not need to manage an encryption key or clean up `rein.db` between iterations.

### Known limitations

- **In-process spend meter.** Budget totals live in memory and reset if the
  Rein process restarts. Fine for single-replica deployments where a crash
  implies a deliberate restart. Not fine for crash-loop pods: a loop that
  would otherwise climb indefinitely could reset the counter and double-spend.
  A SQLite-backed durable meter is the top item on the v0.2 roadmap. Until
  then, pin a single replica and set caps with a safety margin below the
  bill you actually want to cap at.
- **Soft cap under concurrent bursts.** Check runs before the upstream fetch,
  Record runs after. A burst of `N` concurrent requests can all pass Check at
  the same total, so a cap can overshoot by up to `N × average_request_cost`.
  The kill-switch is the independent hard stop.
- **Encryption key rotation** is not yet supported. Changing
  `REIN_ENCRYPTION_KEY` renders the existing database unreadable. A one-shot
  rotation tool is planned for v0.2.
- **Admin API has no pagination** on `GET /admin/v1/keys`. Not a problem at
  alpha scale; tracked as a future enhancement.
- **Model aliases in front-end gateways bypass spend recording.** When Rein
  sits behind another AI gateway that rewrites the `model` field in the
  upstream response to a friendly alias (for example `haiku`, `sonnet`,
  `auto`), Rein's pricer keys off real vendor IDs, logs "unknown model; spend
  not recorded", and the `daily_budget_usd` cap on that key will not fire.
  The kill-switch still works; only metering is affected. Three workarounds
  are documented in the README "Note on model aliases in front-end gateways"
  section: pass real model IDs through from the upstream gateway, enforce
  budgets at the upstream gateway instead, or treat budgets as
  observability-only in this topology.

[Unreleased]: https://github.com/archilea/rein/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/archilea/rein/releases/tag/v0.1.0
