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
3. **Key resolution.** The inbound `Authorization: Bearer rein_live_...` header is looked up in the keystore. Unknown, revoked, or expired tokens return `401`. On SQLite, this is a single indexed `SELECT` plus an AES-256-GCM decrypt of the `upstream_key` column. Keys with a non-nil `expires_at` whose instant has passed fail closed on every request regardless of whether the background sweeper has yet stamped `revoked_at`: the response is indistinguishable from manual revocation so operator expiry schedules do not leak to clients. Keys with `expires_at == nil` pay zero additional hot-path cost.
4. **Budget check.** Reads the key's current daily and monthly USD totals from the spend meter and compares them to the key's caps. If either total has already reached the cap, the request returns `402 Payment Required` with a clean body and the upstream is never contacted.
5. **Rate limit check.** If the key has a non-zero `rps_limit` or `rpm_limit`, the in-memory sliding window counter checks both granularities. Returns `429 Too Many Requests` with `Retry-After` if either cap is breached. The upstream is never contacted.
6. **Concurrency cap check.** If the key has a non-zero `max_concurrent`, an atomic per-key counter is incremented; if the new value exceeds the cap, it is decremented and the request returns `429 Too Many Requests` with `Retry-After: 1`. The upstream is never contacted. A `defer` in the proxy releases the slot after the adapter returns, covering happy path, upstream error, client disconnect, context cancel, and panic unwind alike.
7. **Forward.** The adapter swaps the inbound rein bearer for the real upstream key (OpenAI uses `Authorization: Bearer`, Anthropic uses `x-api-key`), rewrites the outbound URL to the upstream base, and proxies via `httputil.ReverseProxy` over a tuned shared transport.

On the response path, the adapter parses token usage from the upstream response (streamed or buffered), computes USD cost from the embedded pricing table, and records the amount in the spend meter. The record call runs on a background context so a client disconnect mid-flight cannot race the meter write.

## Budget enforcement model

Each virtual key carries an optional `daily_budget_usd` and `month_budget_usd` cap. Zero means no cap.

- **Check** runs before the upstream fetch. It compares the key's accumulated daily and monthly totals against the caps. If the total has already reached the cap, the request is rejected with `402 Payment Required` before the upstream is contacted. No per-request cost estimation.
- **Record** runs after the upstream response is fully parsed. It looks up USD cost from the embedded pricing table (`internal/meter/pricing.json`, keyed on real vendor model IDs with date-suffix stripping) and adds it to both the daily and monthly buckets, UTC-anchored.

Two properties worth naming explicitly:

1. **Budgets are soft under concurrent bursts.** Check runs before the upstream fetch, Record runs after. `N` concurrent requests can all pass Check at the same total, so the cap can overshoot by up to `N × average_request_cost`. To bound that overshoot, set `max_concurrent` on the key: with the per-key concurrency cap at `K`, worst-case overshoot is bounded by `K × max_request_cost`. The kill-switch is the independent hard stop. Set caps with a safety margin if you need a true ceiling.
2. **Totals are durable.** Each Record persists to the SQLite spend table inside a single transaction, so a restart or crash cannot lose already-recorded spend. With `REIN_DB_URL=memory` (the ephemeral path for tests), totals reset on restart.
3. **Rate limits are an orthogonal velocity brake.** Budget caps bound total spend. Rate limits bound request velocity. Both run before the upstream fetch. A key can have both: budget caps prevent runaway cost, rate limits prevent runaway throughput.

## Streaming

For SSE responses, Rein tees the upstream body as it flows to the client. It does not buffer the stream. A wrapping `streamMeter` inspects chunks for usage metadata as they pass through.

- **OpenAI.** Rein auto-injects `stream_options.include_usage: true` into the outbound request body so the upstream emits a final usage chunk. An explicit client opt-out is respected and logged (and spend for that request is not recorded).
- **Anthropic.** `input_tokens` is parsed from the `message_start` event. The final `output_tokens` is parsed from the last `message_delta` event.

Spend is recorded on a background context, so a client disconnect mid-stream cannot race the meter write.

### Streaming under a per-key timeout

When a virtual key carries a non-zero `upstream_timeout_seconds` (see
[admin-api.md](./admin-api.md#upstream-request-timeout)) and the deadline
fires mid-stream, the HTTP status has already been flushed as `200 OK` and
cannot be retroactively changed to `504`. Rein handles this deterministically:

- The stream reader detects `context.DeadlineExceeded`, writes a single SSE
  comment line (`: rein upstream timeout after N seconds\n\n`) to the
  client, and closes the connection. SSE comments are legal no-ops in every
  compliant parser, so strict clients see a clean close with no corrupted
  frame. Clients that look for a `[DONE]` sentinel will not see one, which
  is the correct signal because the call did not finish normally.
- `streamMeter.Close()` still fires its `onDone` callback with whatever
  `input_tokens` / `output_tokens` were parsed before the cancel, so
  partial usage is recorded against the key's budget on the same
  background-context meter write described above. A hanging stream that
  was canceled at second 60 after the upstream emitted one usage chunk at
  second 30 records the usage from that chunk, not zero.

This is why the background-context Record invariant is load-bearing: the
stream reader's cancel path and the meter write must not compete on the
same context for the cancel-to-propagate.

## Persistence

One table. One driver. No CGO.

- **Keystore.** `virtual_keys` in SQLite via `modernc.org/sqlite` (pure Go). WAL mode is enabled so reads do not block writes. The `upstream_key` column is encrypted at rest with AES-256-GCM using a key supplied via `REIN_ENCRYPTION_KEY`. Rein refuses to start without the key, so plaintext credentials cannot land on disk by accident. Ciphertext carries a `v1:` tag so future algorithm rotations do not require a schema migration.
- **Expiry sweeper.** A background goroutine ticks on `REIN_EXPIRY_SWEEP_INTERVAL` (default `60s`, bounded to `[10s, 1h]`) and stamps `revoked_at = expires_at` on every non-revoked row whose `expires_at` has passed. The sweeper is strictly for audit-trail durability: the proxy hot path already rejects expired keys independently, so a sweeper that is behind schedule, crashed, or disabled cannot widen the window during which an expired key is accepted. The sweeper cancels on process shutdown and does not block graceful drain.
- **Kill-switch.** In-process `atomic.Bool`. The kill-switch is global to the process and does not persist across restarts by design: a crash-restart that clears it is the signal to investigate why the process went down.
- **Spend meter.** Durable SQLite table `spend(key_id, period, amount) WITHOUT ROWID` on the same file as the keystore. Check is one `SELECT period, amount FROM spend WHERE key_id = ? AND period IN (?, ?)` returning at most two rows (day + month bucket). Record is a single transaction with two UPSERTs so a crash between the day and month writes cannot leave them out of sync. Totals survive process restart, OOM, and `kill -9`. WAL mode with the SQLite default `synchronous=NORMAL` keeps every committed Record durable across process crashes; a power-loss window of seconds is possible if the OS panics before flushing the WAL to disk. `SetMaxOpenConns(1)` serializes writes at the Go pool level because `modernc.org/sqlite` does not honor `busy_timeout` for intra-process write-lock contention. With `REIN_DB_URL=memory` the in-process `meter.Memory` is used instead; totals reset on restart.
- **Rate limiter.** In-process `sync.Map` of per-key sliding window counters under per-key mutexes. Counters reset on restart, which is fine since in-flight requests cannot outlive the process. A future Redis-backed implementation is tracked in #53.
- **Concurrency limiter.** In-process `sync.Map` of per-key cache-line-padded `atomic.Int64` counters. The padding prevents false sharing on multi-core machines where unrelated keys would otherwise contend on the same cache line. Counters reset on restart, which is fine since in-flight requests cannot outlive the process. A future shared-state implementation (a distributed semaphore) slots in behind the same `concurrency.Store` interface.

Supported `REIN_DB_URL` schemes:

- `sqlite:<path>`. The default. Durable on-disk storage with the `upstream_key` column encrypted at rest.
- `memory`. Ephemeral, in-memory only. Useful for tests and throw-away runs.

There is no Postgres driver in 0.1 and no plan to add one in 0.2. The design target is single-replica deployments where the process is the consistency boundary. Multi-replica coordination is a different product.

All datetimes are persisted and compared in UTC.

## Shutdown

Rolling deploys are the common case. The shutdown lifecycle is built around the Kubernetes pod lifecycle and the fact that LLM calls can legitimately take tens of seconds.

**Signal flow on the first SIGTERM / SIGINT:**

1. `cmd/rein` flips `proxy.Proxy.draining` to true via `SetDraining`. A single atomic store; the proxy hot path reads the flag before the kill-switch (so drain 503s are even cheaper than frozen 503s) and short-circuits new `/v1/*` requests with `503 Service Unavailable`, `Retry-After: 5`, and the structured envelope `{"error":{"code":"draining", ...}}`. Clients see a clean, machine-readable signal and retry against another replica via the operator's load balancer.
2. The background contexts that drive the expiry sweeper and the config-reload poller are cancelled so those goroutines exit promptly and do not block shutdown.
3. `http.Server.Shutdown(ctx)` runs against a context bounded by `REIN_SHUTDOWN_GRACE` (default `30s`, bounded to `[1s, 5m]`). Shutdown stops accepting new connections, lets idle keep-alive connections close on schedule, and lets in-flight requests run to completion. In-flight streaming SSE responses keep flowing until the upstream finishes or the client disconnects.
4. If the grace window expires with requests still in flight, `Shutdown` returns `context.DeadlineExceeded` and net/http force-closes the open connections. Rein logs a WARN line with the count of connections still mid-request at the moment the grace ran out, so operators tuning `REIN_SHUTDOWN_GRACE` can see whether their value is too short for the upstream tail latency they actually observe.
5. The spend meter's SQLite handle is closed last. Any background-context `meter.Record` call that was already in flight completes first (Record uses `context.Background()`, so it does not observe the drain signal); the durability contract on already-running Record calls is unchanged.

**Double-signal escalation.** A second `SIGTERM` / `SIGINT` during the grace window is an operator saying "cut it short now": `cmd/rein` calls `http.Server.Close`, which force-closes every connection immediately and returns from the shutdown path without waiting for the grace deadline. Useful for cancelling a bad deploy that is going to be rolled back regardless.

**Liveness vs readiness.** Kubernetes deployments should wire two probes to two different Rein endpoints:

- `/healthz` is **liveness**. It is strictly "is the process up?" — a liveness-fail tells Kubernetes to restart the pod. `/healthz` never flips during drain: if it did, Kubernetes would restart a pod that is mid-drain, which would kill the very in-flight requests the drain window exists to protect.
- `/readyz` is **readiness**. It reads the same `draining` atomic the proxy hot path reads: `200 {"status":"ready"}` normally, `503 {"status":"draining"}` the instant `SetDraining(true)` runs. A readiness-fail tells the Service's endpoint controller to remove the pod from the load balancer's pool, so no new traffic arrives — but the pod is NOT restarted, leaving the drain window intact for in-flight work. `/readyz` does not report keystore health, upstream reachability, or anything broader; "is this replica in the pool?" is the entire contract.

The admin port is not split from the proxy in 0.3: both surfaces share one `http.Server` and therefore drain together. An operator who still wants to hit `POST /admin/v1/killswitch` during the grace window can, because the admin handlers keep serving until `Shutdown` finishes draining everything.

## What Rein does not do

- **Traces, spans, or prompt-level observability.** Use Langfuse, Helicone, or Braintrust alongside.
- **Evals.** Use Langfuse or Braintrust.
- **Model routing or fallbacks.** Use a dedicated routing gateway.
- **Schema translation.** OpenAI calls go to OpenAI; Anthropic calls go to Anthropic. Rein is a pure reverse proxy.
- **A dashboard or web UI.** The admin surface is an HTTP API driven by `curl`. See [admin-api.md](./admin-api.md).

Staying narrow is the product.
