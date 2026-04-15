# Runbook: rotating the Rein encryption key

`REIN_ENCRYPTION_KEY` is the AES-256-GCM key that Rein uses to encrypt every
`upstream_key` column in the SQLite keystore. If that key is compromised,
you must rotate it: generate a new key, re-encrypt every row under the new
key, and restart Rein with the new key exported.

This runbook covers the full offline rotation flow. Use it during an incident
or as part of scheduled key hygiene.

## When to use this

- Suspected or confirmed compromise of `REIN_ENCRYPTION_KEY`
- Scheduled rotation per your organization's key management policy
- Operator handoff where the previous key holder should no longer have access

## Before you start

- You have shell access to the host running Rein
- You have permission to stop and start the Rein process
- You can back up the SQLite database file (and its WAL and SHM sidecars)
- You have both the current encryption key and a newly generated replacement

Generate a fresh 32-byte key:

```bash
openssl rand -hex 32
```

The output is 64 hex characters, exactly what `REIN_ENCRYPTION_KEY` accepts.

## Why rotation is offline

A running Rein process reads `REIN_ENCRYPTION_KEY` once at startup and
holds it in memory for the lifetime of the process. It cannot hold both
the old and new keys at the same time, so live rotation against a running
server is not supported.

`rein-rotate-keys` is a separate binary (not a `rein` subcommand) for
exactly this reason: shipping it as a distinct binary makes the offline
contract visible in the UX, so you never run it against a live server
by accident.

## Runbook

### 1. Stop Rein

```bash
# Systemd
systemctl stop rein

# Docker
docker stop rein

# Raw process
kill -TERM $(pidof rein)
```

Wait for the process to exit cleanly. Any in-flight requests complete,
and the SQLite file is flushed.

### 2. Back up the database

Copy the SQLite file (and its WAL/SHM sidecars if they exist) to a safe
location before making any changes:

```bash
cp rein.db        rein.db.bak
cp rein.db-wal    rein.db-wal.bak 2>/dev/null || true
cp rein.db-shm    rein.db-shm.bak 2>/dev/null || true
```

If anything goes wrong, you can restore this backup and be back where you
started.

### 3. Run the rotation tool

```bash
rein-rotate-keys \
  --db sqlite:./rein.db \
  --old-key $OLD_HEX \
  --new-key $NEW_HEX
```

The tool prints a single summary line to stdout on success:

```
rotated=N skipped=M duration=...
```

- `rotated` is the count of rows re-encrypted under the new key.
- `skipped` is the count of rows already encrypted under the new key
  (idempotent second runs).
- Any other case (wrong old key, mid-batch failure, row encrypted under
  a third key) aborts the whole run and leaves the database unchanged.

Flags are deliberately explicit: keys are passed on the command line, not
read from the environment. The operator may already have the new
`REIN_ENCRYPTION_KEY` exported for the upcoming restart, and reading the
old key from the same environment would be easy to misconfigure under
pressure.

### 4. Export the new key and start Rein

```bash
export REIN_ENCRYPTION_KEY=$NEW_HEX

systemctl start rein
# or: docker start rein
# or: your systemd unit / container restart command
```

### 5. Verify

Two quick checks:

1. The admin API is responsive:

   ```bash
   curl -s -H "Authorization: Bearer $REIN_ADMIN_TOKEN" \
     http://localhost:8080/admin/v1/keys | jq '.[0].id'
   ```

2. A real proxy call still succeeds end-to-end, proving that decryption
   works under the new key:

   ```bash
   curl -s https://localhost:8080/v1/chat/completions \
     -H "Authorization: Bearer $YOUR_REIN_VIRTUAL_KEY" \
     -H "Content-Type: application/json" \
     -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}'
   ```

If both succeed, the rotation is complete.

## Recovery

If the rotation tool exits with an error, the database was not modified
(the tool aborts before any write when it cannot plan a clean rotation).
Restart Rein with the ORIGINAL key and diagnose the error message.

If Rein fails to start with the new key after a successful rotation,
restore the backup from step 2, start Rein with the old key, and open
an issue with the error log.

## Safety properties

`rein-rotate-keys` is designed to be safe under operator error:

- **Atomic.** Updates run inside a single write transaction. On any
  failure (including SIGINT or SIGTERM mid-run) the transaction rolls
  back and the DB is left unchanged.
- **Idempotent.** A second run with the same `--new-key` leaves every
  row untouched and prints `rotated=0 skipped=N`.
- **Aborts on foreign ciphertext.** If any row decrypts under neither
  the old key nor the new key, the tool aborts BEFORE any write. A
  partial rotation is impossible.
- **Verifies before committing.** After applying all updates the tool
  re-reads one rotated row and confirms it decrypts cleanly under the
  new cipher. Commit only happens if this verification succeeds.
- **No secrets in logs.** The binary never prints plaintext, key
  material, or anything derived from them. Stdout is a single summary
  line. Stderr carries only structural errors with row identifiers.
- **No live-server coordination.** The tool does not try to detect
  whether Rein is running. If you forget to stop Rein, SQLite's own
  write lock surfaces a busy error and the rotation aborts.

## Out of scope

- Rotating the encryption algorithm itself (AES-256-GCM stays the
  same in 0.2; the ciphertext `v1:` tag leaves room for a future
  algorithm rotation without a schema migration).
- Rotating secrets stored outside the keystore (admin token, config
  environment variables, etc).
- Online rotation without stopping Rein.
