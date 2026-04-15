# Architecture

Rein is a modern, lightweight reverse proxy for LLMs with three responsibilities:

1. **Forward** requests to upstream LLM providers, swapping a rein virtual key for the real upstream key at the edge.
2. **Meter** token usage and USD cost from the upstream response, streaming or not.
3. **Enforce** per-key daily and monthly USD caps plus a global kill-switch, both in front of every upstream call.

Everything else (observability, evals, tracing, prompt caching, routing, fallbacks, schema translation) is explicitly out of scope.

## Request pipeline

Every `/v1/*` request goes through the same six steps, in order. The order matters: the kill-switch is first so it can shed load with near-zero work during an incident.

1. **Path dispatch.** The mux routes `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`, `/v1/audio/*`, and `/v1/images/*` to the OpenAI adapter, and `/v1/messages` to the Anthropic adapter. Unknown paths return `404`.
2. **Kill-switch check.** A single `atomic.Bool` read. If engaged, respond `503 Service Unavailable` with `Retry-After: 60` and return. No key lookup, no upstream dial.
3. **Key resolution.** The inbound `Authorization: Bearer rein_live_...` header is looked up in the keystore. Unknown or revoked tokens return `401`. On SQLite, this is a single indexed `SELECT` plus an AES-256-GCM decrypt of the `upstream_key` column.
4. **Budget check.** Reads the key's current daily and monthly USD totals from the spend meter and compares them to the key's caps. If either total has already reached the cap, the request returns `402 Payment Required` with a clean body and the upstream is never contacted.
5. **Rate limit check.** If the key has a non-zero `rps_limit` or `rpm_limit`, the in-memory sliding window counter checks both granularities. Returns `429 Too Many Requests` with `Retry-After` if either cap is breached. The upstream is never contacted.
6. **Forward.** The adapter swaps the inbound rein bearer for the real upstream key (OpenAI uses `Authorization: Bearer`, Anthropic uses `x-api-key`), rewrites the outbound URL to the upstream base, and proxies via `httputil.ReverseProxy` over a tuned shared transport.

On the response path, the adapter parses token usage from the upstream response (streamed or buffered), computes USD cost from the embedded pricing table, and records the amount in the spend meter. The record call runs on a background context so a client disconnect mid-flight cannot race the meter write.

## Budget enforcement model

Each virtual key carries an optional `daily_budget_usd` and `month_budget_usd` cap. Zero means no cap.

- **Check** runs before the upstream fetch. It compares the key's accumulated daily and monthly totals against the caps. If the total has already reached the cap, the request is rejected with `402 Payment Required` before the upstream is contacted. No per-request cost estimation.
- **Record** runs after the upstream response is fully parsed. It looks up USD cost from the embedded pricing table (`internal/meter/pricing.json`, keyed on real vendor model IDs with date-suffix stripping) and adds it to both the daily and monthly buckets, UTC-anchored.

Two properties worth naming explicitly:

1. **Budgets are soft under concurrent bursts.** Check runs before the upstream fetch, Record runs after. `N` concurrent requests can all pass Check at the same total, so the cap can overshoot by up to `N × average_request_cost`. The kill-switch is the independent hard stop. Set caps with a safety margin if you need a true ceiling.
2. **Totals are durable.** Each Record persists to the SQLite spend table inside a single transaction, so a restart or crash cannot lose already-recorded spend. With `REIN_DB_URL=memory` (the ephemeral path for tests), totals reset on restart.
3. **Rate limits are an orthogonal velocity brake.** Budget caps bound total spend. Rate limits bound request velocity. Both run before the upstream fetch. A key can have both: budget caps prevent runaway cost, rate limits prevent runaway throughput.

## Streaming

For SSE responses, Rein tees the upstream body as it flows to the client. It does not buffer the stream. A wrapping `streamMeter` inspects chunks for usage metadata as they pass through.

- **OpenAI.** Rein auto-injects `stream_options.include_usage: true` into the outbound request body so the upstream emits a final usage chunk. An explicit client opt-out is respected and logged (and spend for that request is not recorded).
- **Anthropic.** `input_tokens` is parsed from the `message_start` event. The final `output_tokens` is parsed from the last `message_delta` event.

Spend is recorded on a background context, so a client disconnect mid-stream cannot race the meter write.

## Persistence

One table. One driver. No CGO.

- **Keystore.** `virtual_keys` in SQLite via `modernc.org/sqlite` (pure Go). WAL mode is enabled so reads do not block writes. The `upstream_key` column is encrypted at rest with AES-256-GCM using a key supplied via `REIN_ENCRYPTION_KEY`. Rein refuses to start without the key, so plaintext credentials cannot land on disk by accident. Ciphertext carries a `v1:` tag so future algorithm rotations do not require a schema migration.
- **Kill-switch.** In-process `atomic.Bool`. The kill-switch is global to the process and does not persist across restarts by design: a crash-restart that clears it is the signal to investigate why the process went down.
- **Spend meter.** Durable SQLite table `spend(key_id, period, amount) WITHOUT ROWID` on the same file as the keystore. Check is one `SELECT period, amount FROM spend WHERE key_id = ? AND period IN (?, ?)` returning at most two rows (day + month bucket). Record is a single transaction with two UPSERTs so a crash between the day and month writes cannot leave them out of sync. Totals survive process restart, OOM, and `kill -9`. WAL mode with the SQLite default `synchronous=NORMAL` keeps every committed Record durable across process crashes; a power-loss window of seconds is possible if the OS panics before flushing the WAL to disk. `SetMaxOpenConns(1)` serializes writes at the Go pool level because `modernc.org/sqlite` does not honor `busy_timeout` for intra-process write-lock contention. With `REIN_DB_URL=memory` the in-process `meter.Memory` is used instead; totals reset on restart.
- **Rate limiter.** In-process `sync.Map` of per-key sliding window counters under per-key mutexes. Counters reset on restart, which is fine since in-flight requests cannot outlive the process. A future Redis-backed implementation is tracked in #53.

Supported `REIN_DB_URL` schemes:

- `sqlite:<path>`. The default. Durable on-disk storage with the `upstream_key` column encrypted at rest.
- `memory`. Ephemeral, in-memory only. Useful for tests and throw-away runs.

There is no Postgres driver in 0.1 and no plan to add one in 0.2. The design target is single-replica deployments where the process is the consistency boundary. Multi-replica coordination is a different product.

All datetimes are persisted and compared in UTC.

## What Rein does not do

- **Traces, spans, or prompt-level observability.** Use Langfuse, Helicone, or Braintrust alongside.
- **Evals.** Use Langfuse or Braintrust.
- **Model routing or fallbacks.** Use a dedicated routing gateway.
- **Schema translation.** OpenAI calls go to OpenAI; Anthropic calls go to Anthropic. Rein is a pure reverse proxy.
- **A dashboard or web UI.** The admin surface is an HTTP API driven by `curl`. See [admin-api.md](./admin-api.md).

Staying narrow is the product.
