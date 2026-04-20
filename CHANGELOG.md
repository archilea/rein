# Changelog

All notable changes to Rein will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Per-key `expires_at` with automatic revocation** (#77). Virtual
  keys gain an optional RFC3339 UTC `expires_at` field. Admin API
  accepts it on create and on `PATCH /admin/v1/keys/{id}` (explicit
  `null` clears the expiry; past or malformed timestamps are rejected
  with stable `{expires_in_past, invalid_expires_at}` codes in the
  standard error envelope). Clients using an expired key receive the
  exact same `401 Unauthorized` with `key_revoked` as a manually
  revoked key, so operator expiry schedules never leak to callers. The
  proxy hot path performs one nil check plus one time comparison after
  the existing `IsRevoked` gate; keys without `expires_at` pay zero
  cost and the end-to-end benchmark delta against the prior hot path
  is within noise. A background sweeper, configurable via
  `REIN_EXPIRY_SWEEP_INTERVAL` (default `60s`, bounded to `[10s, 1h]`),
  enumerates expired-but-unrevoked rows once per tick and stamps
  `revoked_at = expires_at` so the audit trail distinguishes
  automatic from manual revocation. The sweeper cancels on process
  shutdown and never blocks graceful drain. SQLite keystore gains an
  additive `expires_at` column with a forward-compatible, idempotent
  migration. See `docs/admin-api.md` "Time-bounded keys" for operator
  recipes (contractor access, break-glass tokens, temporary elevated
  caps).

- **Update a key's caps without re-minting** (#74). New
  `PATCH /admin/v1/keys/{id}` endpoint lets operators change a virtual
  key's `name`, `daily_budget_usd`, `month_budget_usd`, `rps_limit`,
  `rpm_limit`, `max_concurrent`, and `upstream_base_url` on an
  already-minted key. Immutable fields (`id`, `token`, `upstream`,
  `upstream_key`, `created_at`, `revoked_at`) are rejected with `400`
  and a structured error envelope. Revoked keys return `409 Conflict`.
  Partial updates are explicit: only fields present in the JSON body
  are changed; absent fields are preserved; zero values explicitly set
  the cap to unlimited. Validation reuses the same rules as
  `POST /admin/v1/keys`. The `keys.Store` interface gains an `Update`
  method implemented by both `Memory` and `SQLite` stores. See
  `docs/admin-api.md` for the full endpoint documentation.

### Changed

- **Structured JSON errors on proxy endpoints** (#75). Every proxy-side
  error response (`/v1/*` short-circuits: 401, 400, 402, 429, 503, 404)
  now returns a `{"error": {"code": "...", "message": "..."}}` JSON
  envelope instead of plain text. The envelope shape matches the admin
  API format shipped in 0.2. Status codes and `Retry-After` headers are
  unchanged. Stable error codes (`missing_key`, `invalid_key`,
  `key_revoked`, `upstream_mismatch`, `budget_exceeded`, `rate_limited`,
  `concurrency_exceeded`, `kill_switch_engaged`, `unknown_route`,
  `internal_error`) are defined as constants in `internal/proxy/errcodes.go`
  so machine clients can branch on `code` instead of substring-matching
  messages. The `writeAPIError` and `writeJSON` helpers are extracted to
  a shared `internal/api` package used by both admin and proxy surfaces.
  See `docs/admin-api.md` for the full error code catalog.

## [0.2.0] - 2026-04-15

### Added

- **Per-key concurrency cap** (#27). Virtual keys gain an optional
  `max_concurrent` field that caps the number of simultaneously in-flight
  `/v1/*` requests for that key. When the cap is reached, the next
  request receives `429 Too Many Requests` with `Retry-After: 1` and the
  upstream is never contacted. This is the nginx `limit_conn` analog and
  pairs cleanly with the existing rate limit (`rps_limit`/`rpm_limit`)
  brake: the rate limit bounds arrival velocity, the concurrency cap
  bounds work-in-progress. The cap is the recommended way to bound the
  soft-budget overshoot documented in the README: with
  `max_concurrent: K`, worst-case overshoot is bounded by
  `K × max_request_cost` regardless of arrival pattern. The hot-path
  implementation is a lock-free per-key atomic counter behind a
  `concurrency.Store` interface; counters are cache-line-padded to
  prevent false sharing across keys, and unlimited keys
  (`max_concurrent: 0`) pay zero cost. In multi-replica deployments,
  per-key limits are per-replica; a globally-synchronized variant slots
  in behind the same interface in a future release. See
  `docs/admin-api.md` for operator guidance.

- **Encryption key rotation tool** (#18). A new offline CLI
  `rein-rotate-keys` rotates the AES-256-GCM key that encrypts
  `upstream_key` in the SQLite keystore. It is a separate binary (not
  a `rein` subcommand) because a running `rein` process holds exactly
  one `REIN_ENCRYPTION_KEY` at a time, so rotation is inherently
  offline. Usage:

  ```
  rein-rotate-keys --db sqlite:./rein.db \
    --old-key $OLD_HEX --new-key $NEW_HEX
  ```

  Safety properties: the tool walks every `virtual_keys` row,
  decrypting with the old cipher and re-encrypting with the new
  cipher inside a single write transaction. Rows already encrypted
  under the new cipher are detected and skipped, so a second run with
  the same `--new-key` is a no-op (idempotent). Any row that decrypts
  under neither cipher aborts the whole run BEFORE any write, so a
  partial rotation cannot leave the DB in a half-migrated state.
  After applying updates the tool re-reads one rotated row and
  verifies it decrypts under the new cipher before committing. No
  plaintext or key material appears in logs or error messages; the
  binary prints only `rotated=N skipped=M duration=...`. The operator
  runbook with the full stop, backup, rotate, restart flow lives at
  `docs/runbooks/key-rotation.md`. See #18.

- **Operator-editable pricing overrides with hot reload** (#25). A new
  `rein.json` config file resolved via the hybrid rule — env var
  `REIN_CONFIG_FILE` wins if set, otherwise `/etc/rein/rein.json`
  (the default path, picked up automatically in K8s ConfigMap
  deployments without any env var boilerplate), otherwise run
  zero-config against the embedded table. Startup log records which
  rule fired (`source=env_var|default_path|embedded_only`). The file's
  `models` block merges on top of the embedded pricing table:
  override entries win for the same `(upstream, model)` pair; new
  pairs are added. Enables honest budget enforcement and spend
  recording for every OpenAI-compatible provider unlocked by #24
  (Groq, Fireworks, OpenRouter, DeepSeek, xAI Grok, and any future
  entrant) without a Rein release — operators add the model prices
  in their own file and reload. Zero-config default is unchanged: if
  neither the env var nor the default path is set, Rein uses just the
  embedded table, bit-for-bit identical to pre-0.2 behavior.

  Reload triggers: **SIGHUP** (always on when `REIN_CONFIG_FILE` is set;
  operators run `kill -HUP $(pidof rein)`, `docker kill --signal=HUP
  rein`, or `systemctl reload rein`) and an **optional background poll**
  via `REIN_CONFIG_POLL_INTERVAL` (opt-in for Kubernetes ConfigMap
  deployments; bounded to `[1s, 1h]`, rejected outside that range at
  startup). Both triggers share the same load-and-swap path so their
  failure and success behavior are identical by construction.

  Hot-path safety: the active `*Pricer` is wrapped in a new
  `meter.PricerHolder` that uses `atomic.Pointer[Pricer]` for
  publication. Adapters (`NewOpenAI` / `NewAnthropic`) take
  `*PricerHolder` instead of `*Pricer`; every response-side `recordSpend`
  call does one lock-free atomic load before resolving the price.
  Micro-benchmark shows the indirection is **10.3 ns/op vs 10.4 ns/op**
  for the direct path — within measurement noise, zero allocations on
  either path. Full SQLite+budget hot-path benchmark (~33.5 µs) is
  unchanged across the swap.

  Validation: strict all-or-nothing. File must parse as JSON, `version`
  must be `"1"` (or empty, defaults to 1), every `input_per_mtok` and
  `output_per_mtok` must be `>= 0`. Zero prices are allowed (free tiers,
  local-hosted models) and log an INFO line per zero entry so operators
  can tell at a glance what they shipped. A bad reload logs ERROR,
  includes the active snapshot's model count, and keeps the previous
  snapshot active — a bad config cannot take down a running process.

  Version mismatch is asymmetric: **fatal at startup**, **warn-and-keep
  previous snapshot** on reload. An unknown future schema version on
  `kill -HUP` does not crash an operator's production process.

  File format documented in `docs/quickstart.md` section 3b with a Groq
  example, the correct SIGHUP / docker kill / systemctl commands, and
  the Kubernetes poll-interval guidance. Mount the file into the
  container; do not bake it into the image.

- **Per-key upstream base URL override** (#24). A virtual key can now
  carry an `upstream_base_url` that replaces the global `REIN_OPENAI_BASE`
  for that key's requests. Unlocks any OpenAI-compatible provider (Groq,
  Together, Fireworks, DeepSeek, xAI Grok, OpenRouter, Perplexity,
  Cerebras, local vLLM / Ollama / LocalAI, ...) using Rein's existing
  OpenAI adapter with no new wire-protocol code. Admin validation
  accepts `https` (or `http` for loopback hosts), allows an optional
  path prefix (so `https://api.groq.com/openai` is accepted because
  Groq mounts its OpenAI-compatible surface under `/openai`; ditto
  `https://openrouter.ai/api` and `https://api.fireworks.ai/inference`),
  strips a trailing slash during canonicalization, rejects query string,
  fragment, and userinfo, and returns a stable `{"error": {"code": ...,
  "message": ...}}` envelope on failure.
  The hot-path override uses a `sync.Map` of parsed URLs keyed by raw
  string so repeat requests pay only a single lock-free load; benchmark
  delta against the existing SQLite+budget hot path is within noise.
  Only the OpenAI adapter is overridable in 0.2; Anthropic and Azure
  OpenAI are tracked separately. Unknown models hit by an override key
  trigger a `model not in pricing table; spend not recorded` WARN that
  fires for every occurrence within the first 60 seconds of a new
  `(key_id, model)` pair and rate-limits to once per minute afterwards,
  so operators notice the gap immediately. SQLite keystore gains an
  additive `upstream_base_url` column with a forward-compatible
  migration that runs idempotently against pre-existing databases.

- **Per-key request rate limiting (RPS + RPM)** (#26). Each virtual key
  can now carry optional `rps_limit` and `rpm_limit` caps enforced on the
  proxy hot path before the upstream is contacted. Algorithm is a sliding
  window counter (same as Cloudflare, Kong, Envoy) that bounds
  boundary-burst overshoot to approximately 1.1x the configured limit.
  Over-limit requests return `429 Too Many Requests` with a computed
  `Retry-After` header. New `internal/rates` package exposes a `Store`
  interface with an in-memory `Memory` implementation; a future
  Redis-backed implementation (#53) slots in behind the same interface
  for multi-replica deployments. Counters are in-memory and reset on
  process restart. Unlimited keys (both limits zero) pay zero cost on
  the hot path. Admin API accepts `rps_limit` and `rpm_limit` on key
  create. SQLite schema gains two additive columns via idempotent
  migration.

- **Durable SQLite-backed spend meter** (#17). Per-key daily and monthly
  USD totals now persist to the same SQLite file as the keystore so they
  survive a process restart, OOM, or `kill -9`. Schema is one row per
  `(key_id, period)` under `WITHOUT ROWID`; Check is one `SELECT ... WHERE
  period IN (?, ?)`, Record is a single transaction with two UPSERTs so a
  crash between the day and month writes cannot leave them out of sync.
  Wiring follows the keystore rule: `REIN_DB_URL=sqlite:<path>` picks up
  the durable meter (default), `REIN_DB_URL=memory` keeps the in-process
  ephemeral meter used in 0.1. On a 4-core Apple M5 the full production
  hot path measures about 72 microseconds per request with the durable
  meter enabled, versus about 34 microseconds with the in-memory meter.
  Durability is scoped to application-crash survival: committed Records survive process crashes, OOM, and kill -9 (WAL replays on reopen). Power-loss durability uses the SQLite WAL default (synchronous=NORMAL); operators who need stronger guarantees can tighten it later.

### Changed

- **Public positioning** (#23). Reframed as "a modern, lightweight reverse proxy for LLMs" (previously "a small, boring cost and safety brake for LLM API traffic"). No scope change: the same "deliberately does not do" constraints still hold.

- **Audit-friendly ceiling** (#39). Replaced the "under 2,000 lines of Go" public guarantee with CI-enforced internal design disciplines: a ceiling on direct non-stdlib dependencies, a ceiling on compiled production modules, and a ceiling on compressed amd64 image size. The specific thresholds live as grep-able literals in `.github/workflows/ci.yml` so every budget change is visible in `git log` on that file. The README now publishes the current state as of each release as transparency, not as a public SLA, so future releases that legitimately move these numbers can evolve without a messy public renegotiation. Source LOC stays as an internal design forcing-function but is no longer pinned in docs. No scope change and no code in `internal/` or `cmd/` touched; only README, CHANGELOG, and the CI workflow changed.

- **Audit-friendly ceiling state for 0.2 release.** Direct non-stdlib dependencies remain at **1** (`modernc.org/sqlite`); compiled production modules remain at **10** (no new module roots added across the seven 0.2 features); compressed amd64 image grew from **~12 MB to ~14 MB** as a side effect of the new code paths (rate limiter, concurrency store, durable spend meter, key rotation CLI, per-key URL override caches) and stdlib growth between Go releases. The CI cap on compiled deps (`MAX_COMPILED_DEPS=15`) and the existing image-size cap both still hold with margin. The image growth is the single biggest 0.2 surface change to surface for operators planning rollout sizing.

## [0.1.1] - 2026-04-10

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
  A SQLite-backed durable meter is the top item on the 0.2 roadmap. Until
  then, pin a single replica and set caps with a safety margin below the
  bill you actually want to cap at.
- **Soft cap under concurrent bursts.** Check runs before the upstream fetch,
  Record runs after. A burst of `N` concurrent requests can all pass Check at
  the same total, so a cap can overshoot by up to `N × average_request_cost`.
  The kill-switch is the independent hard stop.
- **Encryption key rotation** is not yet supported. Changing
  `REIN_ENCRYPTION_KEY` renders the existing database unreadable. A one-shot
  rotation tool is planned for 0.2.
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

[Unreleased]: https://github.com/archilea/rein/compare/0.1.1...HEAD
[0.1.1]: https://github.com/archilea/rein/releases/tag/0.1.1
