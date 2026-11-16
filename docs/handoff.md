# Single-writer host migration

This runbook moves one Atlas Render state path from host A to host B during a
maintenance window, fences host A so an accidental restart cannot fork the
state, and includes a rollback drill. The machinery is small and deliberately
verifiable:

| Fact | How it stays true |
|---|---|
| Fixed inputs | Each job stores its own `Payload` snapshot. A manifest `factsDigest` covers `SnapshotDigest(payload)` per job, so a retried or recovered attempt cannot silently read newer assets. |
| Task promises | Idempotency receipts and job states are part of `factsDigest`. Replaying the original `Idempotency-Key` after the move returns the original job, even after its artifact bytes were purged. |
| Cleaned facts | A purged artifact keeps `state: purged` and an empty body; both are in `factsDigest`. Download stays `410`; the success job stays `succeeded`. |
| No split brain | Every start publishes a new `generation` + `writer`. A process whose generation no longer matches the file refuses every route with `503 fenced`, including reads and health. |
| Old host stays dead | `state.json.superseded` is checked by `ConditionPathExistsNot`, by process start, and is not released by deleting the lock file or by replacing `state.json`. |

`generation` is monotonic per state file and is carried into the destination.
`writer` is the process identity (`ATLAS_WRITER` or `host:pid`); it appears in
`GET /v1/status` and in the `process.claimed` / `handoff.adopted` events.

## Prerequisites

- Build both binaries on the new host: `go build -o atlas ./cmd/atlas` and
  `go build -o atlasctl ./cmd/atlasctl`.
- Stop the service on host A **before** running any `atlasctl` command.
  `atlasctl` acquires the same exclusive lock and fails with a clear message if
  the service is still running.
- Keep host A's state directory intact until the drill is signed off. It is the
  rollback source.

## Cutover steps

```sh
# Host A, service stopped by the administrator:
./atlasctl inspect --state /var/lib/atlas/state.json
./atlasctl capture --state /var/lib/atlas/state.json \
                   --to /var/lib/atlas/state.json \
                   --manifest handoff-a-to-b.json   # optional early check
./atlasctl cutover --state /var/lib/atlas/state.json \
                   --to /var/lib/atlas/state.json \
                   --manifest handoff-a-to-b.json

# Copy ONLY the state file to host B (scp/rsync while A is still stopped):
scp /var/lib/atlas/state.json host-b:/var/lib/atlas/state.json
scp handoff-a-to-b.json      host-b:/var/lib/atlas/handoff-a-to-b.json

# Host B, before starting:
./atlasctl verify --state /var/lib/atlas/state.json \
                  --manifest handoff-a-to-b.json
ATLAS_MANIFEST=/var/lib/atlas/handoff-a-to-b.json ./atlas
```

`cutover` performs capture, copy, byte verification and fencing in one locked
sequence. `ATLAS_MANIFEST` on host B makes the state process verify the file
against the manifest **before** it increments the generation and starts
serving. If the file was edited, truncated or advanced, startup fails and the
path stays unclaimed.

`atlasctl` paths in the destination directory are illustrative; in a real move
the destination is a different host, so copy `state.json` and the manifest, then
run `verify` on host B. `cutover` is intended for host-local moves, shared
volumes, and for the drill below.

Idempotency keys are global to the state file, so clients that replay a request
during the window with their original key get the original job. Clients that
mint a new key are creating a new promise and are expected to do so only after
the maintenance window closes.

## Fencing the old host

`atlasctl cutover` writes `state.json.superseded` next to the old state. The
marker records the destination, the manifest digest, the generation and the
state digest. It must not be deleted to "restart" host A.

`deploy/systemd/atlas.service` contains `ConditionPathExistsNot=` for the
marker, so `systemctl start atlas` on host A is a no-op after cutover, and
`Restart=on-failure` cannot resurrect it. Starting the binary by hand still
exits non-zero with `state was superseded by handoff ...`.

## Rollback drill

The drill is real: it interrupts a running job with SIGKILL, cuts over, replays
promises on the new path, rolls back, and simulates a stale write to the live
path.

```sh
python3 scripts/handoff_drill.py --keep
```

Phases printed by the script and what each proves:

1. `interrupted` — the heavy job is left `running` with no drain/flush.
2. `cutover` — destination bytes equal the stopped source; `factsDigest`
   unchanged; `state.json.superseded` exists on the old path.
3. `old-host-restart` — starting the old path again is refused.
4. `stale-copy` — a copy that advanced its own generation is rejected by
   `atlasctl verify` with exit code 3.
5. `adopted` — host B's `GET /v1/status` reports its own writer, generation+1
   and `adoptedBy=<manifest>`.
6. `replay` — the original idempotency key returns the original job; the purged
   artifact is still `410`.
7. `recovered` — the interrupted job succeeds at attempt 1, and a fresh render
   compares pixel-identical (`changedPixels == 0`).
8. `rollback` / `rollback-verified` — new facts after cutover move back; the
   destination is fenced; the replay and recovered artifact are still there.
9. `abandoned-restart` — the fenced destination refuses to start even after its
   lock file is deleted.
10. `generation-fence` — a simulated take-over of the live path makes the
    serving process return `503 fenced` on health, reads and writes.

Run the drill on a copy of production data before the real window, then once
against the real paths with the service stopped. Do not infer "no fork" from
a successful restart alone: the drill asserts bytes, receipts, purged facts and
pixels.

## What to do when something goes wrong

- `verify` exit 3 or startup shows `handoff facts changed`: stop. A fact moved.
  Compare `atlasctl inspect` on both paths; do not edit the JSON.
- Old host was started by accident before fencing: it may have served the stale
  copy with the same facts. Capture, cut over, and let the old process be
  fenced by the generation check on its next write. Any promises it accepted
  are not in the destination state; reconcile them from its `state.json` before
  cleanup. This is the one case a handoff cannot repair automatically.
- New host refuses with `state was claimed by another writer`: a process is
  still bound to the file (usually the old host on a shared volume). Stop it,
  verify the manifest, restart.
