# Design: active-passive high availability

Status: agreed (#65 phase 1, roadmap 5.2). Phase 2 (active-active over a shared metadata store) is out of scope.

A single MongoRescue instance is a single point of failure for scheduled backups and point-in-time recovery (PITR) collection: while it is down, no job runs and the oplog is not collected. Active-passive HA runs a second instance as a **standby** that takes over within a bounded time after the **active** instance dies, without ever running the same scheduled backup twice.

The two instances share nothing but a storage target. Each has its own data directory and metadata database. The storage target holds the **lease**, which decides who is active, and the **metadata snapshots**, which carry the active instance's database to the standby.

## Goals and non-goals

Goals:

- At most one instance runs scheduled backups, PITR collection, copies, integrity sweeps, re-encryption, metadata snapshots, notifications and the heartbeat at any time, also during network partitions, process pauses and clock jumps.
- A standby takes over within `ttl + renew` of the active instance's last successful lease renewal, plus the takeover work (below).
- Off by default. With `MONGORESCUE_HA_MODE=off` nothing changes.

Non-goals (phase 1):

- **A shared data directory.** Two processes on one data directory are refused by the data directory lock (`mongorescue.lock`). Where the volume itself survives the node (a network disk that reattaches), run a single instance with a restart policy instead.
- Active-active, or more than one standby doing work. More than two instances may join a cluster; all but one are standbys.
- Moving runs. A backup or restore that was running on the active instance when it died is not resumed: it is recorded as interrupted, and its archive, if complete, is imported.
- Reads or writes on the standby. It serves no data and accepts no sign-in.

## Lease

The lease is one small JSON object on a storage target, `_mongorescue/ha/<cluster>/lease.json`:

```json
{"cluster":"prod","holder":"a1b2…","holder_name":"mongorescue-0","epoch":7,"seq":4211,
 "renewed_at":"2026-10-12T10:00:00Z","ttl_seconds":30,"key_id":"9f…",
 "last_snapshot_epoch":7,"last_snapshot_at":"2026-10-12T09:58:41.517Z","mac":"3c…"}
```

- `holder` is the instance ID (random, kept in `<data dir>/ha.json`). An empty holder means the lease was released.
- `epoch` is the fencing token. It grows on every takeover and never decreases: an acquired lease gets an epoch above the old lease's, above every epoch the cluster's snapshots on the HA target authenticate (see [Metadata snapshots](#metadata-snapshots); a name alone counts for nothing) and above the last one the instance held, so a lease deleted or corrupted by hand never restarts the epochs below existing snapshots (an acquisition waits while the snapshots cannot be listed, or while a snapshot that sorts above the newest verified one cannot be checked, since it may carry the highest epoch). Every run record and every metadata snapshot carries the epoch it was written under.
- `seq` grows with every renewal, so every write has distinct content (and an S3 ETag of its own).
- `renewed_at` is for people only. No decision reads it.
- `last_snapshot_epoch` and `last_snapshot_at` name the newest metadata snapshot the active instances wrote: the active instance updates them, and writes the lease at once, after every snapshot it stores, and a takeover sets them to the snapshot it restored. A takeover must find a verified snapshot at least as new ([Rollback protection](#rollback-protection)).
- `mac` authenticates `cluster`, `epoch`, `holder`, `last_snapshot_epoch` and `last_snapshot_at`: HMAC-SHA256 with an HKDF subkey of `secret.key` (domain `ha-lease`). The holder name, `seq`, `renewed_at` and `ttl_seconds` are not authenticated (changing them can only delay a takeover). A lease taken over from one that is not trusted stays unsigned until the takeover is confirmed, so it never vouches for a last snapshot nobody authenticated.
- **Trust on first use, cluster-wide.** Earlier versions write the lease without `mac`. Such a lease (and a snapshot without a header) is trusted only while the HA target holds no snapshot of the cluster with an authenticated header, so a new joiner or an instance with a reset data directory cannot be fooled once the cluster runs the new version. Each instance also records `seen_authenticated_lease` for the cluster in `ha.json` the first time it reads or writes an authenticated lease, and refuses a lease without a MAC from then on whatever the HA target holds. A lease with a wrong MAC is always refused. A rolling upgrade therefore needs no operator: upgrade the standbys first and the active instance last; whichever upgraded instance takes over from the old lease signs it, and every instance has the flag within one renew interval.
- `key_id` is derived from `secret.key` (like the install ID of metadata backups). An instance whose key differs refuses to start, since it could not read the active instance's snapshots or credentials. For the same reason an instance in HA mode refuses to rotate `secret.key` on its own (`keyrotation.ErrHAMode`); the key of a cluster changes in a maintenance window ([production.md](../production.md#rotating-the-secret-key-of-an-ha-cluster)).

### Conditional writes

The lease is safe only if writes are compare-and-swap. Storage drivers that support it implement the optional capability `storage.LeaseStore`:

- `ReadVersioned(key)` returns the content and its version.
- `CreateIfAbsent(key, data)` fails with `storage.ErrPreconditionFailed` when the object exists.
- `ReplaceIfMatch(key, data, version)` fails with `storage.ErrPreconditionFailed` when the object changed or is gone.

Drivers:

- **S3**: `PutObject` with `If-None-Match: *` and `If-Match: <etag>`. AWS S3 supports both since 2024; so does MinIO. HTTP 412 and 409 (`ConditionalRequestConflict`) map to `ErrPreconditionFailed`. Some S3-compatible services accept the headers but ignore them. At HA startup, `storage.ProbeLeaseStore` writes a probe object twice with `If-None-Match: *` and replaces it with a wrong ETag; if either write succeeds, HA refuses to start with `storage.ErrConditionalWritesUnsupported`. Targets with S3 Object Lock are refused: every renewal would leave a locked version behind.
- **Local**: every operation holds an exclusive `flock` (`LockFileEx` on Windows) on a sidecar file `<key>.lock`. Create uses `O_EXCL`; replace compares the SHA-256 of the current content with the expected version and swaps the file with a rename. The local driver is meant for tests and shared file systems. On NFS, `flock` needs NFSv4 (or NFSv3 with a working `lockd`) and a client that maps `flock` to byte-range locks (Linux does since 2.6.12). SMB and FUSE file systems vary: prefer S3.

### Timing

Every instance polls or renews every `renew` interval (default 10 s); the lease lives for `ttl` (default 30 s). `renew` must be below `ttl/2`.

No instance trusts another's clock, and no standby reads the wall clock for a decision. Each measures time with its own monotonic clock:

- **Active.** It renews with `If-Match` on the version it wrote last. The renewal's validity is measured from when the request was **sent**, since the write may take effect any time between sending and the response. If no renewal succeeds for `ttl - renew` (20 s by default), it steps down. A renewal that fails with `ErrPreconditionFailed` means someone else wrote the lease: it steps down at once. Each request is bounded by the time left, so a hung request cannot delay the step-down.
- **Standby.** It reads the lease every `renew`. When the version (or epoch) changes, it notes the time of the observation. When a lease has not changed for `ttl` (or the lease's own `ttl_seconds`, whichever is larger) since the standby first saw its current version, the standby replaces it with `If-Match` on that version: holder itself, epoch + 1. A released lease (empty holder) is taken at once; a missing lease is created with `If-None-Match`.

Why this is safe: the standby's takeover write succeeds only if the lease still has version V, which it first observed at time `O ≥ C(V) ≥ S(V)`, where `C` is when V was committed and `S` when the renewal that wrote it was sent. The takeover happens at `O + ttl` or later. The active instance stops at `S(V') + ttl - renew` for its last successful renewal V'. If V' is newer than V, the takeover write fails (wrong version). Otherwise V' = V and `S(V) + ttl - renew < O + ttl`. The margin `renew` covers clock rate differences and the time to stop work; clock rate drift on real hardware is far below 1 %.

### Fencing

Timing alone does not cover a process that is paused (GC, VM freeze, `SIGSTOP`) between a check and a write. The epoch covers it:

- **Before a run starts** (the admission check of the run manager, which every backup, restore and PITR base backup passes) and **before a backup or restore is marked completed** (a completion guard in the engines), the active instance checks that it still holds the lease. The check is local and live: role active and the step-down deadline not reached. A fenced-off instance never starts or completes a run after it lost the lease: the run fails with `ha.ErrNotActive`.
- **Background work checks it too, before it writes or deletes**: the PITR collector before each chunk upload and commit (and it never removes an uncommitted chunk without the lease, since the new active instance may write the same key), retention and the purge, the copy queue, and integrity sweeps, scans and restore tests.
- **A suspend is fenced too.** The monotonic clock may stand still while a machine or VM is suspended. The active instance's check therefore also fails when its own **wall clock** shows `ttl − renew` or more since the last renewal was sent (a wall clock set back never shortens anything), and the scheduler, when the wall clock moved more than 30 seconds apart from the monotonic clock since its last fire, renews the lease (or steps down) and checks it again before anything fires.
- **The step-down is never late.** The lease manager wakes at the step-down deadline when that comes before its next renewal, so after a renewal that fails fast the active instance stops exactly `ttl − renew` after its last successful renewal, not up to a renew interval later.
- **A failed renewal is retried before the deadline.** One renewal request may take at most half the time left to the deadline (at least half a renew interval, at most one), so a request that stalls on a dead connection leaves room for another; after a failure the lease manager tries again every tenth of a renew interval until it succeeds or the deadline comes.
- Records written under the lease carry `ha_epoch`.
- Metadata snapshots carry the epoch in their key and, authenticated, in their encrypted header. The standby restores the verified snapshot with the **highest epoch**, then the newest, so a late snapshot from a fenced-off instance never wins over the current one, and a copy renamed to a higher epoch is refused.

## Roles

```
            acquire lease                      lose lease / storage unreachable
 standby ───────────────────► taking over ───► active ─────────────────────────► standby
    ▲                              │                         (cancel runs, stop components)
    └── snapshot cannot be used ───┘
        (release lease, wait ttl)
```

### Standby

- Serves no data and accepts no sign-in: its database is only as current as its last takeover, so neither its records nor its users, sessions and API keys are trusted. It serves only `GET /api/v1/health` (with `role` and `ha`: cluster, epoch, holder, lease health), `GET /metrics` without credentials but limited to the process, Go runtime, build and HA series, and, in place of the dashboard, a static page that says it is the standby and names the active instance (`standby.html` with `ha.js`, in all eight languages).
- Answers every other request (the whole API, signing in and out, single sign-on, MCP) with `503`, `Retry-After` and the error "this instance is the standby of HA cluster X; it serves no data and accepts no sign-ins: use the active instance".
- Runs none of the active-only components: scheduler, PITR collector, copy queue, integrity sweeps and storage scans, re-encryption, metadata snapshots, RPO checker, the global heartbeat ping, the notification outbox, the audit log forwarder, and the startup recovery of interrupted runs. The run manager refuses new runs.
- Its metadata is the database it last had as active (or empty). It is not refreshed while it waits.

### Takeover

When the standby acquires the lease:

1. It stops its standby components and closes its database (the HTTP listener is down for about a second). Until the App of the active role serves, the standby App keeps reporting `standby` and refusing the API and sign-ins, so a health check never sends a client to it as the active instance.
2. **Metadata handover.** It lists the snapshots under `_mongorescue/metadata/ha/<cluster>/` and, in the order of their names (the highest epoch, then the newest), decrypts the start of each until one is verified: a snapshot whose authenticated header names another cluster, epoch or time than its key, or that does not decrypt, is refused (see [Security](#security)). It checks that restoring it cannot roll the metadata back ([Rollback protection](#rollback-protection)), then downloads it to a temporary file in the data directory, decrypts it, and opens it as a metadata database. Opening checks `secret.key` and applies migrations, so a snapshot from an older version is upgraded. If that fails, it tries again (four attempts, with a pause of 1, 2 and 4 seconds) and then gives up: an older snapshot never stands in for the newest. Then it swaps the file in, crash-safe and under the data directory lock: both databases are checkpointed (`wal_checkpoint(TRUNCATE)`) and closed, a journal (`mongorescue.db.replace.json`) records the swap, the previous database is linked to its backup name, the snapshot is renamed over it, and the directory is synced after each step; the next start settles a swap a crash interrupted (before the rename the previous database stays, after it the new one) and refuses to start when the database is missing. It keeps the previous database as `mongorescue.db.bak-<UTC time>-epoch<N>` (the newest three such copies are kept, so no previous database is ever lost). If the newest verified snapshot was written by this instance under the epoch it last held (`<data dir>/ha.json`), its own database is at least as new and is kept.
3. It opens the App in the active role. Before the components start, it marks the runs the snapshot shows in progress as interrupted (the existing startup recovery). After they start, it scans every storage target and imports the orphan archives written since the snapshot (the existing scan and import, which never deletes anything).
4. **Missed schedules.** Each enabled job whose most recent cron fire fell between its last recorded scheduled run and now runs once, as a scheduled run, unless a backup of its databases started at or after that fire is already recorded (an imported archive counts), or the next fire is less than half an interval away. At most one run per job, never one per missed fire. **Unknown means covered**: the fire runs only when it is certain that no backup was taken for it, that is, a snapshot was restored, the scan of the job's storage target succeeded and imported every newer archive, and the job's databases are known (a multi-database job's known databases). Otherwise it does not run, and the event `ha.catch_up_skipped` ("missed fire not caught up: could not verify", sent to every channel) asks for a manual run.

If no snapshot can be used, the instance releases the lease, stays standby and tries again after `ttl`; `mongorescue_ha_takeover_failures_total` grows and the `MongoRescueHATakeoverFailed` and `MongoRescueHANoActiveInstance` alerts fire. **No snapshot at all** is accepted only on a genuine first activation: no lease existed (it was created, not taken over), no snapshot of any kind is under the cluster's prefix, and the instance's `ha.json` has no HA history for the cluster (its lease, its snapshots or an epoch it held). That covers a new cluster and HA enabled on an existing single instance: the instance keeps its own database, populated or not, and confirms the takeover. A lease that existed (expired, released or unreadable) had a holder whose metadata would be lost, so the takeover is refused as above ("no metadata snapshot to take over from"); with no lease but an instance with HA history for the cluster, the cluster's objects were deleted, and it is refused as a suspected rollback.

### Rollback protection

Whoever can write to the HA target can delete objects as well as copy them. Deleting the newest snapshots (or the lease) would make an older genuine snapshot the newest left, and a takeover would silently roll the metadata back: revoked API keys and removed users return, records written since are lost. The takeover therefore refuses, with `mongorescue_ha_rollback_suspected_total{reason}` and the `MongoRescueHARollbackSuspected` alert, when:

| Reason | Condition |
| :--- | :--- |
| `lease_untrusted` | The lease it took over is unreadable, its `mac` is wrong, or its `mac` is missing although the HA target holds a snapshot with an authenticated header or this instance has seen an authenticated lease (trust on first use, see [Lease](#lease)) |
| `lease_replayed` | A verified snapshot was written under a higher epoch than the lease's: an older genuine lease was put back (seen by fresh instances too) |
| `lease_stale` | The last snapshot the lease names is older than the two newest verified snapshots: the lease lags more than one write (a crash between a snapshot and the lease update accounts for one) |
| `lease_missing` | No lease existed although the HA target holds snapshots, or although this instance has seen the cluster or its database is populated |
| `snapshot_behind_lease` | The newest verified snapshot is older (epoch, then time) than the last snapshot the lease names, or than one an authentic lease this instance saw named (a replayed older lease does not lower it) |
| `snapshot_behind_local` | It is older than this instance's high-water mark: the newest snapshot it wrote or restored, kept per cluster in `ha.json` |
| `legacy_after_upgrade` | Only snapshots without a header are left, but the lease or the high-water mark names a snapshot with one |

A snapshot that sorts before the newest verified one but cannot be checked (a storage error, or a format of a newer version) may itself be the newest: the takeover tries again four times and then gives up instead of restoring the older one, and a new lease is not acquired below it.

The instance stays standby, releases the lease and tries again after `ttl`, so the alerts keep firing until an operator acts.

### Forcing a takeover

After checking the HA target (who can write to it, which snapshots and lease are there, what the log says), an operator forces the next takeover of one instance, on its host:

```sh
mongorescue ha force-takeover --from-snapshot _mongorescue/metadata/ha/<cluster>/mongorescue-<time>Z-e<epoch>.db.age
mongorescue ha force-takeover --keep-local-database
```

`--data-dir` and `--cluster` default to `MONGORESCUE_DATA_DIR` and `MONGORESCUE_HA_CLUSTER`. The command writes `<data dir>/ha-override.json` (mode `0600`): a standby serves no API, and write access to its data directory is the authority. At its next takeover (within the lease TTL) the instance uses the request once and removes it. It restores the named snapshot, whose header is still verified when it has one (a renamed copy stays refused; a header-less snapshot of an earlier version is accepted), or keeps its own database; skips the rollback checks; resets its high-water mark to that snapshot; and signs the lease. The forced takeover is logged as an error, recorded in the audit log (`SYSTEM ha.force_takeover`, with the operator's user name, the snapshot and the epoch) and sent to every channel as `security.destructive_action` (`ha_force_takeover`). The new active instance writes a snapshot at once, under an epoch above every one on record.

### Step-down

When the active instance loses the lease (or cannot renew in time), it cancels its runs (recorded as cancelled with the reason "lost the HA lease"), stops all components and reopens as standby. If runs do not stop within the shutdown budget, the process exits so that its restart policy starts it fresh. It then sends `ha.stepped_down` to every enabled channel directly, one attempt each within 5 seconds (it no longer runs the notification outbox), and counts it in `mongorescue_ha_step_downs_total`.

A graceful shutdown of the active instance (`SIGTERM`) drains like a single instance, writes a last snapshot, and releases the lease, so the standby takes over at its next poll instead of after `ttl`.

## Metadata snapshots

- Written by the active instance only, through the fencing check: right after it becomes active (so its epoch has a snapshot at once), after every finished backup, restore or job run (debounced to one per 10 s) and at least every `MONGORESCUE_HA_SNAPSHOT_INTERVAL` (default 5 minutes).
- Key: `_mongorescue/metadata/ha/<cluster>/mongorescue-<UTC time>Z-e<epoch>.db.age`. It does not collide with the per-installation self-backups under `_mongorescue/metadata/<install_id>/`.
- `VACUUM INTO` a temporary file, then age encryption with a passphrase derived from `secret.key` (an HKDF subkey; scrypt work factor 10, since the passphrase is a random 256-bit key), then upload. A standby needs nothing but the shared key, and an instance with another key refuses to start: at startup it checks the lease's `key_id` and decrypts the start of the cluster's snapshots, in takeover order, until one decrypts. The data never passes through memory as a whole.
- **Authenticated header.** The plaintext starts with `mongorescue-ha-snapshot\n` and one line of JSON, `{"format":1,"cluster":…,"epoch":…,"instance_id":…,"created_at":…}`, before the database. age authenticates the whole plaintext (ChaCha20-Poly1305 per 64 KiB chunk, the first chunk holding the header), so only a holder of `secret.key` can write a header, and a changed byte makes the snapshot fail to decrypt. Every decision verifies it against the object key: the takeover, the epoch floor of a new lease, retention and the startup check read headers in the order of the names, through the whole listing, until they have what they need. Each instance remembers every outcome per stored object version (key, size, modification time, ETag), so a snapshot is downloaded once however many decisions read it, and planted objects cost one download each.
- **Snapshots without a header** were written before it existed. They are restored only when every snapshot on the HA target was checked and none of the cluster's has an authenticated header (a verified one, a renamed copy or a newer format), with a warning in the log; their names are not authenticated. Writers always write the header, and retention deletes header-less snapshots once a verified one exists, so they last until the first snapshot after the upgrade.
- Retention decides on verified snapshots only: it keeps the first 12 (highest epoch, then newest), the 12 newest of the current epoch and the newest by time whatever its epoch, and never deletes the newest snapshot nor the current epoch's newest ones. It reads the whole listing. A name nobody authenticated never displaces a genuine snapshot: refused snapshots do not count toward the kept ones, and they are kept for investigation, logged once and alerted (`mongorescue_ha_refused_snapshots`) until someone deletes them (see [Security](#security)). A snapshot whose check failed (a storage error, or a format of a newer version) may be the newest: retention deletes nothing that sorts before it, and prunes the older snapshots as usual, so the prefix stays bounded.
- Storage targets, connections, jobs, users and settings, the backup encryption keys among them (sealed with `secret.key`), travel with the snapshot.

## Configuration

| Variable | Flag | Default | |
| :--- | :--- | :--- | :--- |
| `MONGORESCUE_HA_MODE` | `-ha-mode` | `off` | `off` or `active-passive` |
| `MONGORESCUE_HA_TARGET` | `-ha-target` | | Storage target ID or name, or a location URL: `s3://bucket/prefix?endpoint=…&region=…&path_style=true` (credentials from the AWS default chain) or `file:///path` |
| `MONGORESCUE_HA_CLUSTER` | `-ha-cluster` | `default` | Cluster name: lowercase letters, digits, `.`, `_`, `-` |
| `MONGORESCUE_HA_LEASE_TTL` | `-ha-lease-ttl` | `30s` | 5 s to 10 min |
| `MONGORESCUE_HA_RENEW_INTERVAL` | `-ha-renew-interval` | `10s` | At least 1 s, below `ttl/2` |
| `MONGORESCUE_HA_SNAPSHOT_INTERVAL` | `-ha-snapshot-interval` | `5m` | 30 s to 24 h |

HA mode requires a shared secret key: `MONGORESCUE_SECRET_KEY`, or the same `secret.key` mounted on both instances.

A target given by ID or name must exist in the instance's own database when it starts, which a fresh standby does not have; the URL form needs nothing but the bootstrap configuration and is the recommended one. The target is resolved once at startup.

## Failure matrix

| Failure | What happens | Bound |
| :--- | :--- | :--- |
| Active process crashes, is killed, or its node dies | Standby takes over after the lease expires | `ttl + renew` + takeover work |
| Active instance cannot reach the lease storage | It steps down; the standby takes over if it can reach it | Active stops at `ttl - renew` after its last renewal; standby takes over at `ttl` after it last saw a change |
| Standby cannot reach the lease storage | Nothing changes; `mongorescue_ha_lease_errors_total` grows | |
| Both cannot reach the storage | Active steps down; no instance is active; the `MongoRescueHANoActiveInstance` alert fires | Backups to that storage would fail anyway |
| Network partition between the instances only | Nothing: they never talk to each other | |
| Active process paused (GC, VM freeze, `SIGSTOP`) longer than `ttl` | Standby takes over. On resume the paused instance's fencing check fails before any run starts or completes, and its next renewal fails | A run that was uploading may still finish its upload; it is recorded as failed, and its archive stays an orphan for the next storage scan |
| Wall clock jumps on either side | A standby ignores it. The active instance steps down when its wall clock jumped forward past the deadline (a suspend looks like that); a jump back has no effect | |
| Active machine or VM suspended longer than `ttl − renew` | On resume the fencing check fails on the wall clock before any run starts or completes; the scheduler renews before it fires, and the instance steps down | |
| Both instances start at once on an empty bucket | `If-None-Match: *` lets exactly one create the lease | |
| Lease object deleted by hand | Active's renewal fails and it steps down; the standby creates a new lease with a higher epoch than any it saw, but refuses the takeover while snapshots exist (`lease_missing`) | No active instance until an operator forces the takeover; alerted |
| Lease replaced by one without a valid MAC | The takeover is refused (`lease_untrusted`), by every instance that has seen an authenticated lease of the cluster | No active instance until an operator forces the takeover; alerted |
| Newest snapshots deleted | The takeover is refused (`snapshot_behind_lease` or `snapshot_behind_local`) | No active instance until an operator forces the takeover; alerted |
| Storage provider ignores conditional headers | HA refuses to start (startup probe) | |
| Standby has a different `secret.key` | It refuses to start | |
| No usable snapshot at takeover | Takeover is abandoned; the lease is released; retried after `ttl` | Alerted |
| A snapshot is copied or renamed on the HA target (another epoch, time or cluster), or altered | Refused: never restored, never deleted, never raises the epoch; the next verified snapshot is used. Logged as an error and counted in `mongorescue_ha_snapshots_rejected_total` | Alerted (`MongoRescueHASnapshotRejected`) |
| Old active restarts | Starts as standby | |

## RTO and RPO

With the defaults (`ttl` 30 s, `renew` 10 s, snapshots every 5 minutes):

- **Detection**: at most `ttl + renew` = 40 s after the active instance's last successful renewal; a graceful shutdown hands over within one poll (10 s).
- **Takeover work**: a snapshot download and swap (seconds for a database of a few MB), the scan of the storage targets, then the components start. Typically under 30 s.
- **Scheduling RTO**: about 70 s. A missed fire runs once on takeover.
- **Metadata RPO**: run records are snapshotted after every finished run, so a crash loses at most what changed since (normally nothing but the running runs). Settings and job edits made in the last snapshot interval can be lost; edit through the active instance and check the dashboard after a takeover. Archives written after the last snapshot are imported by the scan.
- **PITR**: the collector resumes from the last chunk the snapshot records and reads the gap from the oplog again. The oplog window must be longer than the takeover (it is, by orders of magnitude). Orphan chunk objects from the old active are removed by the collector's retention. The pins that keep retention away from the base and chunks of a running point-in-time restore or chain test live in each process's memory; that suffices because only the active instance runs restores and retention, and an old active instance that lost the lease is fenced and stops its runs.
- **Notifications**: delivered at least once. Deliveries pending in the snapshot's outbox are sent again by the new active instance; receivers can de-duplicate on the event ID.

## Observability

- `mongorescue_ha_role{role="active"|"standby"}` (1 for the current role), `mongorescue_ha_epoch`, `mongorescue_ha_lease_errors_total`, `mongorescue_ha_takeovers_total`, `mongorescue_ha_takeover_duration_seconds` (the last takeover, from acquiring the lease to serving as active), `mongorescue_ha_lease_healthy`, `mongorescue_ha_snapshots_rejected_total` (snapshots refused because their authenticated header did not match their name or cluster, or they did not decrypt; once per stored object version and process), `mongorescue_ha_refused_snapshots` (how many such snapshots the HA target holds, as of the last check).
- `/api/v1/health` adds `role` and an `ha` object (cluster, epoch, holder, lease health).
- Events `ha.took_over` and `ha.stepped_down`, sent to every enabled notification channel.
- `mongorescue_ha_rollback_suspected_total{reason}`: takeovers refused because they could roll the metadata back ([Rollback protection](#rollback-protection)).
- Alert rule `MongoRescueHANoActiveInstance`: no instance reports `role="active"` for twice the lease TTL (1 minute with the defaults). `MongoRescueHARollbackSuspected`: a takeover was refused in the last 15 minutes as a suspected rollback. `MongoRescueHASnapshotRejected`: the HA target holds a refused snapshot, or one was refused in the last 15 minutes.
- A standby serves a static standby page instead of the dashboard; a dashboard open on an instance that becomes standby reloads into it.

## Security

- The lease holds no secret. Its `key_id` is an HKDF subkey of `secret.key`, which reveals nothing about the key.
- Snapshots are encrypted with a passphrase derived from `secret.key`; credentials inside stay sealed with `secret.key` as in every metadata copy.
- A snapshot's cluster, epoch and time are authenticated by its encrypted header, not taken from its object key. Whoever can write the snapshot prefix but does not hold `secret.key` cannot replay an older snapshot under a higher epoch or a later time, nor bring in a snapshot of another cluster that shares the key: the copy is refused and alerted, and the newest genuine snapshot is restored (#189). They can still delete snapshots, so that an older genuine one is the newest left, or keep a takeover from succeeding; and while a cluster has only snapshots written before the header existed, the names of those are not authenticated.
- Refused snapshots are never deleted by MongoRescue: they are evidence. The log names each one (`HA metadata snapshot refused`, with its key). After the investigation, delete them with the storage's own tools, for example `aws s3 rm s3://<bucket>/<prefix>_mongorescue/metadata/ha/<cluster>/<name>`; `mongorescue_ha_refused_snapshots` drops at the next check (the next snapshot on the active instance) and the alert resolves.
- The lease is authenticated (`mac`) and names the cluster's last snapshot; deleting snapshots or the lease, or replacing the lease, makes the next takeover refuse instead of rolling the metadata back ([Rollback protection](#rollback-protection)). Only an operator with access to an instance's data directory forces it past that ([Forcing a takeover](#forcing-a-takeover)).
- Whoever can write the lease prefix can stop the cluster (by holding the lease, or by deleting or altering the HA objects so that every takeover is refused) but cannot make an instance run anything it would not run anyway, nor roll its metadata back. Give the HA target's credentials only to MongoRescue.

## Testing

- `internal/storage`: the conditional writes of the local driver and of S3 against the hermetic fake (and a fake that ignores the headers, which the probe must catch); MinIO in the integration suite.
- `internal/ha`: the lease manager against an in-memory store and a fake clock: takeover after expiry, a renewal delayed past the takeover, step-down on a failed or conflicting renewal, flapping storage, wall clock skew, release.
- `internal/integration`: two real processes in HA mode on one MinIO and one MongoDB with a job every minute; `SIGKILL` the active one; the standby must become active within `ttl + renew + 30 s`, the next scheduled backup must run there, no fire may produce two completed backups, and the old instance must come back as standby.
