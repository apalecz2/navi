# Restore runbook

This is what Q5 means in practice: *"Recovery from total host loss is
restoring the Litestream replica and starting the container."* Read this
section first if you are here because the disk is gone.

## The panic-time procedure

1. **Get a host with Docker and this repository checked out.** You do not
   need the old `./data` directory — that's the point.
2. **Point `LITESTREAM_REPLICA_URL` (and, for R2, `LITESTREAM_ENDPOINT` /
   `LITESTREAM_ACCESS_KEY_ID` / `LITESTREAM_SECRET_ACCESS_KEY`) at the real
   replica** in `.env`.
3. **Restore into a clean directory before touching `./data`:**
   ```sh
   mkdir -p data-restore
   docker run --rm \
     -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY \
     -v "$(pwd)/data-restore:/data-restore" \
     litestream/litestream:0.3.13 restore \
       -o /data-restore/navi.db \
       -endpoint "$LITESTREAM_ENDPOINT" \
       -access-key-id "$LITESTREAM_ACCESS_KEY_ID" \
       -secret-access-key "$LITESTREAM_SECRET_ACCESS_KEY" \
       "$LITESTREAM_REPLICA_URL"
   ```
   (For a `file://` replica, as in the drill below, only `-o` and the URL are
   needed — see that section for the exact form used.)
4. **Sanity-check before promoting it:**
   ```sh
   sqlite3 data-restore/navi.db 'PRAGMA integrity_check;'
   sqlite3 data-restore/navi.db 'SELECT COUNT(*) FROM items;'
   sqlite3 data-restore/navi.db 'SELECT COUNT(*) FROM occurrences;'
   ```
5. **Promote it:** `mv data-restore data`, then `docker compose up -d`.
   Litestream picks the restored file up as `/data/navi.db` and starts a new
   replication generation against it — this is expected (see the caveat
   below) and not itself a sign anything went wrong.
6. **Confirm:** `curl localhost:8000/healthz` should read `"status":"ok"`
   within a few seconds, with `pending_overdue` matching what you'd expect
   from the restored data.

## The drill actually run this session

No R2 bucket exists for this repository yet, and the user asked not to stand
one up just to prove the mechanism. Litestream treats a `file://` replica and
an `s3://` one through the identical replication/restore code path, so the
drill below proves the mechanism byte-for-byte without a cloud account. It
does **not** prove R2 network/credential connectivity specifically — that is
the one gap this session leaves open, and it's a real one: the first time
this procedure runs against actual R2, it should be treated as unverified
until it's been done once for real.

Reproduce with `ops/docker-compose.drill.override.yml.example` (copy to
`docker-compose.override.yml`) and `LITESTREAM_REPLICA_URL=file:///backup/navi`
in `.env`.

**What happened, in order, 2026-08-07:**

1. Stack brought up (`docker compose up -d --build`) against the existing
   local dev database (4 items, 88 occurrences — real data from earlier
   sessions' `go run` testing, not seeded for this drill).
2. Litestream opened generation `4bae3a811fab69f8`, wrote an initial
   snapshot and WAL segment to `./backup/navi` within one second of boot.
3. `docker compose stop navi` — clean shutdown, litestream's own "subprocess
   exited, litestream shutting down" followed the app's shutdown log lines.
4. Restored to a **clean** directory with the pinned image directly (no app
   container involved):
   ```
   docker run --rm -v ./backup:/backup -v ./data-restore:/data-restore \
     litestream/litestream:0.3.13 restore -o /data-restore/navi.db file:///backup/navi
   ```
   Output: `restoring snapshot` → `restoring wal files` (index 0-1) →
   `applied wal` ×2 → `renaming database from temporary location`. Exit 0.
5. Compared original (WAL-checkpointed first: `PRAGMA
   wal_checkpoint(TRUNCATE)`) against the restored copy, via a throwaway
   `alpine` container with `sqlite3` installed:

   | check | original | restored |
   |---|---|---|
   | `items` count | 4 | 4 |
   | `occurrences` count | 88 | 88 |
   | `kv` count | 4 | 4 |
   | `PRAGMA integrity_check` | `ok` | `ok` |
   | `sqlite3 .dump \| sha256sum` | `3e4ff2f4ec639bdc267d5366a57b8a7b459ff418a1045374670821ab79a9fc16` | **same hash** |

   The dump hashes matching exactly is the strongest statement available
   short of a live production restore: the restored database is not just
   row-count-equal, it is byte-for-byte the same logical content.
6. `docker compose start navi` — came back to `"status":"ok"` within
   seconds, all five loops healthy.

**Operational caveat found during the drill, worth keeping here:** running
`PRAGMA wal_checkpoint(TRUNCATE)` directly against the live database file
*while Litestream is actively replicating it* invalidates Litestream's
position tracking — on the next tick it logged `"cannot determine last wal
position, clearing generation"` and started a fresh generation. This is
Litestream protecting itself correctly, not a bug, but it means: **don't
checkpoint or otherwise hand-edit the live WAL while the container is
running.** If you need to inspect the live database, `litestream restore` a
copy first, the same way this drill did, rather than opening `./data/navi.db`
directly.

## R2-unreachable-at-boot check

Also run for real, same session, same stack: pointed `LITESTREAM_REPLICA_URL`
at `s3://nonexistent-bucket-navi-drill/navi` with `LITESTREAM_ENDPOINT`
resolving nowhere (`https://invalid.invalid.example`) and fake credentials —
the closest reproducible stand-in for "R2 is unreachable" without touching
real R2.

Result:
- `navi` container started and stayed up; `/healthz` returned
  `{"status":"ok", ...}` with all five loops healthy throughout.
- Litestream logged `level=ERROR msg="monitor error" ... dial tcp: lookup
  invalid.invalid.example ... no such host"` once a second, continuously —
  loud, in the same `docker compose logs navi` stream as everything else,
  exactly as point 2 of the session brief asked for.
- With the outage still active, a synthetic occurrence was inserted 8 seconds
  out and the scheduler claimed and sent it on its next tick:
  `{"msg":"notifications sent","loop":"scheduler","result":{"claimed":1,"sent":1,"failed":0,...}}`,
  logged in the same second as another `"monitor error"` line. The fire path
  and the backup path are separate OS processes connected only by Litestream's
  `-exec` wrapping (signal forwarding on shutdown, nothing else at runtime),
  and this is the concrete demonstration of that separation holding: a
  completely unreachable backup target never touched the reminder.

## Chaos check — supervisor restart

`internal/supervisor/supervisor.go`'s `run()` only returns on context
cancellation as written, and `tickOnce`'s per-tick `recover()` means a
panicking `Tick` never produces a flat line — it's caught, logged, and
`health.Observe` still runs (see the package doc comment: "a loop that is
running but failing still reports as ticking"). So testing the *restart*
path honestly requires forcing exactly the case the code calls "should be
impossible": `run()` returning on its own.

Done via a temporary, uncommitted edit — `os.Getenv("NAVI_CHAOS_LOOP")`
gating an early `return` from `run()` for one named loop, before `tickOnce`
(and therefore before `health.Observe`) ever ran, held for 220 seconds
(comfortably past the health registry's staleness threshold of `3×interval +
5s` = 185s for a 60s-interval loop) — then reverted with `git checkout --
internal/supervisor/supervisor.go` and rebuilt clean. `copywriter` was the
target: harmless to skip, logs and returns.

**Observed, with the fault active:**
```json
{"status":"degraded","loops":{
  "copywriter":  {"last_tick": null, "healthy": false},
  "materializer":{"last_tick": "2026-08-07T19:14:42Z", "healthy": true},
  "reconciler":  {"last_tick": "2026-08-07T19:14:42Z", "healthy": true},
  "scheduler":   {"last_tick": "2026-08-07T19:14:42Z", "healthy": true},
  "sweeper":     {"last_tick": "2026-08-07T19:14:42Z", "healthy": true}
}}
```
Log stream showed `"chaos: forcing run() to return unexpectedly"` immediately
followed by `"loop exited unexpectedly, restarting"` every ~1 second (the
`restartBackoff`) for the duration — the supervisor kept trying, exactly as
designed, and the other four loops kept ticking on their own schedules the
entire time.

**After the fault window (220s later), unprompted:**
```json
{"status":"ok","loops":{
  "copywriter":{"last_tick": "2026-08-07T19:18:22Z", "healthy": true}, ...
}}
```
`copywriter` resumed ticking on its own the instant the injected fault
stopped forcing it down — no restart, no intervention. `git status
internal/supervisor/` was clean afterward; the fault-injection edit never
shipped.

## What this session did not verify

- **Real R2 connectivity or credentials.** The drills above prove the
  Litestream mechanism and the unreachable-backend behavior; they do not
  prove a real R2 bucket and access keys work. Run this runbook's numbered
  procedure once against the real bucket before relying on it.
- **Reachability through the actual Cloudflare Tunnel.** `/healthz` was
  never checked from outside this host — see `ops/cloudflared-ingress.md`
  for the ingress rule to apply on the real `cloudflared` instance, and
  verify `/healthz` (not `/metrics`) resolves through it.
