# Point-in-time recovery

MongoRescue collects the oplog of a replica set and base backups of the whole instance, and restores a replica set, or some of its databases, to a point in time **into new safe-clone databases**. In-place point-in-time restores and sharded clusters are not offered. Point-in-time recovery is a supported feature since #141: the stream API, the point-in-time body of restores and the dashboard panel follow the [versioning policy](versioning.md). The design is in [design/pitr.md](design/pitr.md); what is tested is [below](#what-is-tested), and the [runbook](#runbook-restore-to-the-minute-before-an-accident) walks through a restore.

Point-in-time recovery (PITR) restores a replica set, or one of its databases, to any moment within a window. It needs two things, which MongoRescue collects for every enabled **PITR stream**:

- **base backups** of the whole instance, taken with `mongodump --oplog` on a schedule;
- a continuous **oplog chain**: the replica set's oplog, read every chunk interval and stored as encrypted chunks.

A window runs from the consistent point of the oldest base the chain covers to the end of the newest chunk. Per-database jobs keep running next to a stream and keep their own RPO.

## Requirements

- **A replica set.** A standalone server has no oplog; sharded clusters are not supported.
- **Backup encryption with age keys** (Settings → Encryption). The oplog keeps every write, deleted data included, so a stream cannot be enabled while encryption is off.
- **Privileges** of the connection's user (spike S3, MongoDB 5.0 and 8.0):

  | What | Needs |
  | --- | --- |
  | The collector | `find` on `local.oplog.rs`: the `backup` role, `read` on the `local` database, or a custom role. `readAnyDatabase` does **not** grant it: its wildcard excludes `local`. |
  | Base backups | The `backup` role (`mongodump --oplog` reads the whole instance and the oplog). |
  | Point-in-time restores | `restore`, `readWriteAnyDatabase` and `dbAdminAnyDatabase` on `admin` (or `anyAction`, e.g. `root`) on the target, ideally through a separate user and connection. Replaying the oplog checks privileges per operation: `restore` alone fails at the first update, and `restore` + `readWriteAnyDatabase` fails at `dropDatabase`. |

  Creating or enabling a stream checks the oplog access through `connectionStatus` and refuses it with a clear message otherwise.
- **MongoDB Database Tools** as for every backup; point-in-time restores need **100.12 or newer**. CI tests point-in-time recovery with 100.12.2 and 100.19.1, on single-node replica sets of MongoDB 5.0, 6.0, 7.0 and 8.0 (every pull request) and on three-member replica sets of 5.0 and 8.0 (nightly).

## Enabling a stream

In the dashboard, open **Connections**: the **Point-in-time recovery** panel lists the streams and offers a form to enable one for a connection (admin). Through the API, `POST /api/v1/pitr/streams` with at least `{"connection_id": "…"}`; see [api.md](api.md#point-in-time-recovery-streams) for every field.

| Setting | Default | |
| --- | --- | --- |
| Chunk interval | 60 s | 15 to 900 s. Each chunk holds the oplog entries of one interval; above 4 GiB the collector halves it for the next chunks. |
| Base schedule | `0 2 * * *` | A cron schedule. The first base is taken as soon as the collector has started the chain. |
| Base retention | 7 bases or 14 days | Pinned bases and the newest eligible base are always kept. |
| Base on gap | on | Take a base backup as soon as a gap breaks the chain. |
| `oplog_max_days` | unset | Delete chunks older than this many days, even if that shortens the window (privacy). |
| Read preference | `secondaryPreferred` | Where oplog reads go. |
| `chain_test_cron` | unset | A cron schedule of [chain tests](#chain-tests); unset turns them off. |
| `chain_test_connection_id` | the stream's connection | The connection chain tests restore into: another server spares production their load. |

## Restoring to a point in time

A point-in-time restore picks the **newest eligible base** whose consistent point (`t_after`) is before the target, checks that the live chunks from the base's `t_before` up to the target form one unbroken run (no gap, no divergence, no chunk that failed verification) and that a key is configured for the encryption of the base and of every chunk, and records that choice on the restore (`pitr` in the restore record). Then it runs in two passes, both streamed from storage into `mongorestore` without temporary copies:

Before anything is written, a **checksum pre-pass** downloads every chunk of the range that the integrity sweep has not verified in the last 24 hours, hashes it (without decrypting it) and refuses the restore when one does not match its recorded SHA-256; a chunk without a recorded checksum is refused too. Unverified chunks are therefore downloaded twice (once for the pre-pass, once for the replay); keep the integrity sweep running to avoid it.

1. **The base** is restored with `mongorestore` without `--oplogReplay`. A whole-instance restore renames every database with `--nsFrom '$db$.$coll$' --nsTo '$db$_rescue_<timestamp>.$coll$'` and always excludes `admin.*`, `config.*` and `local.*`; a restore of some databases uses `--nsInclude=<db>.*` and the same rename per database.
2. **The oplog.** The chunks are fetched in order, each hashed while it streams and checked against its recorded SHA-256, decrypted, gunzipped and passed through the namespace filter (the same renaming and selection, up to the limit), which writes a synthetic oplog-only archive to the stdin of `mongorestore --archive --oplogReplay --oplogLimit=<t>:<i>`.

Afterwards the number of operations the filter wrote is compared with the "applied N oplog entries" line of `mongorestore`. A mismatch fails the restore and keeps the clones for inspection; a missing line adds a warning and sets `pitr.ops_unverified: true` on the record. A restore that fails otherwise, or is cancelled, drops its clones.

Last, the target connection's [post-restore commands](api.md#post-restore-commands) (such as re-applied erasures, see [privacy.md](privacy.md#re-applying-erasures-after-a-restore)) run against the clones; `"*"` covers every clone, those of databases first seen in the oplog included. A failing command fails the restore and keeps the clones. Chain tests run none.

**Recorded clones.** The clone names are recorded on the restore (`pitr.clones`) before anything is written: the databases the base listed after its dump (`instance_databases`), and, while the restore runs, each database that first appears in the oplog, stored before its first entry is replayed. Clean-up drops exactly these names, never a pattern. When the server stops in the middle of a restore or a chain test, the next start marks the record failed, drops its recorded clones (temporary for a chain test, partial and untrustworthy for a restore) and says so in its message.

**The target.** A time `S` (RFC 3339, on the primary's clock, UTC in the dashboard) restores every write up to and including the second `S`: the replay stops at `(S+1):0`. An exact oplog position `{t, i}` restores every write *before* it. The target must lie in a window: from the consistent point of its base to one second before the newest collected entry.

**Where it goes.** Every restored database is a new database named `<db>_rescue_<YYYYMMDD_HHMMSS>_<id>` (the start of the restore in UTC and a random 4-character ID, so two restores in the same second never collide with each other or with a backup's safe clone); nothing that exists is overwritten, and the restore refuses to start when such a name is taken. In-place point-in-time restores are refused in this release. A clone name may not exceed MongoDB's 63 bytes, so databases longer than 35 bytes cannot be restored this way: the preflight computes every clone name from the base's database list and refuses the restore, naming them; restore those databases from a database backup instead. Databases MongoRescue restored into (names containing `_rescue_`) are never cloned again: a whole-instance restore leaves them out of both passes, and they cannot be selected. `mongodump --oplog` cannot exclude databases, so they still take room in base backups; drop clones you no longer need. A database selected in `databases` must be one the base holds; to get a database created after the base, restore the whole instance.

**Who may.** Point-in-time restores and their preflight need the **operator** role or an operator API key, like other safe-clone restores; a restore into another connection than the stream's (`target_connection_id`) needs admin, and chain tests stay admin. A caller limited to some connections only sees, and restores, the streams of those connections. They only add databases, so the two-person rule of [delete protection](security.md) does not hold them back, like other safe-clone restores.

**Dashboard.** In **Connections → Point-in-time recovery**, **Restore to a time** on a stream opens the wizard: choose a window and a time inside it (UTC) and optionally the databases, then **Check** runs the preflight and shows the base it chose, the number and size of the chunks and the estimated duration, and **Restore** asks for a confirmation and starts the restore.

**CLI.** `mongorescue restore --pitr <stream-or-connection> --at <RFC3339> [--database a,b]` runs the preflight and starts the restore; `--wait` waits for it. See [cli.md](cli.md).

**API and MCP.** `POST /api/v1/restore` and `POST /api/v1/restores/preflight` take `"pitr": {"stream_id": "…", "at": "…"}` (or `"ts": {"t": …, "i": …}`) and an optional `"databases"`; see [api.md](api.md#point-in-time-restores). MCP offers `pitr_status` (read) and `pitr_restore` (operator keys); see [mcp.md](mcp.md).

**Preflight.** Besides the connection and the server versions, the preflight of a point-in-time restore checks:

| Check | Fails when |
| --- | --- |
| `pitr_chain` | No base and unbroken, verified chain reach the target, a chunk has no recorded checksum, or a key for the encryption of the range is missing. Chunks that were never verified are accepted; the pre-pass and the replay check them. |
| `privileges` | The target's user certainly lacks `restore`, `readWriteAnyDatabase` or `dbAdminAnyDatabase` on `admin` and holds neither `anyAction` nor `root` (custom roles only warn). |
| `disk_space` | The server reports less free space than the base archive plus the oplog of the range (it warns below four times the base plus the oplog). |
| `target_database` | A clone name is longer than 63 bytes, or taken. |
| `tools_version` | `mongorestore` is missing or older than 100.12. |

The result carries the plan (`pitr` in the preflight result) with an estimate of the duration (`estimated_seconds`, `estimate_from`): from the rates measured on the stream's recent restores and chain tests (`measured`), or else from default rates (`default`: 50 MiB/s for the base, 4 MiB/s for the oplog, which is replayed on one thread); see [Restore time](#restore-time-rto).

**Limits.** No in-place restores, no sharded clusters, no restore across a gap, `admin`, `config` and `local` are never restored (users and roles stay as they are), and a view or time-series collection follows the filter's rules in [design/pitr.md](design/pitr.md#restore-57). Indexes that were dropped or hidden in the window stay as they were in the base.

## Chain tests

A chain test proves that a stream can actually be restored: it restores the eligible base before the newest base that has a manifest to the consistent point of that newer base (every write up to and including its `t_after`) into temporary `<db>_rescue_cv<time in base 36><id>` databases (20 bytes of suffix, so database names up to 43 bytes fit), compares their document counts and indexes with the newer base's manifest, records the outcome on the restore (`pitr.chain_test: true`, `verification`) and drops the clones. Base backups capture an instance manifest (every database but `admin`, `config`, `local` and the clones) for this.

Chain tests are opt-in: they run on the stream's `chain_test_cron` schedule (unset by default) and on demand with `POST /api/v1/pitr/streams/{id}/chain-test` (admin). They need two eligible bases in one window. **A chain test is a full restore**: it downloads a base and the oplog between two bases and writes a copy of every database into the target, so it costs the disk space, I/O and time of a whole-instance restore. Point `chain_test_connection_id` at another server to spare production; the restore preflight applies like to any restore (no force), so a target without the privileges or the free space refuses the test. A failed chain test adds `pitr_chain_test_failed` (warn) to readiness, and successful ones, like completed restores, feed the measured rates of the [RTO estimate](#restore-time-rto).

## Restore time (RTO)

A point-in-time restore takes the time of its two passes: the base restore, which `mongorestore` runs like any restore (tens of MB/s, see [testing.md](testing.md#performance)), and the oplog replay, which applies one entry at a time with majority write concern, so on a replica set every entry waits for a majority of the members. The replay usually dominates, and it is bound by the **number of entries** more than by their size: plan for the writes between the base and the target, not for the oplog's size on disk.

**Measured rates.** Every completed point-in-time restore and chain test records how long each pass took (`pitr.base_seconds`, `pitr.replay_seconds`) and logs its replay rate ("pass 2 replayed N oplog entries … in S s: E entries/s, M MiB/s"). The estimate of the preflight and of the [readiness report](api.md#recovery-readiness) restores the base at its measured rate (total bytes over total seconds of the stream's five newest completed restores and chain tests) and estimates the replay from the same runs with a least-squares fit of their replay times to their entries and stored bytes (the plan knows both for the chunks to replay): a cost per entry plus a cost per byte when the runs tell the two apart, per entry alone when they cannot (one workload, whose bytes grow with its entries) and per byte alone when that fits better (`replay_model`: `entries_and_bytes`, `entries` or `bytes`). So a history of small entries does not inflate the estimate of an oplog of large ones. A pass never measured uses the default rates: 50 MiB/s for the base, and for the oplog the slower of 4 MiB/s and 2,000 entries per second. Schedule chain tests (`chain_test_cron`), ideally while the replica set is busy, so the estimate follows the stream's real workload. The readiness report's `streams[].rto` estimates a whole-instance restore to the newest point of the open window: the newest eligible base plus the stored oplog after its `t_before`, with `source` (`measured`, `chain_test` for a chain test of an earlier release, or `default`), `samples` and the rates; the dashboard shows it under the RTO of the connection's rows.

**Typical rates.** Replay measured by the test suites on one laptop (Apple silicon, Docker Desktop, MongoDB 8.0, Database Tools 100.12.2), with an oplog of small inserts and updates (a few hundred bytes each):

| Setup | Entries | Replay time | Entries per second | Stored oplog per second |
| :--- | ---: | ---: | ---: | ---: |
| Single-member replica set, idle (integration suite, `TestPITRChainTest`) | 10,036 | 4.3 s | 2,345 | 0.02 MiB/s |
| Three members, idle (replica set scenarios, `TestFailoverDuringRestore` retries) | 40,080 | 15.0 to 16.2 s | 2,470 to 2,670 | 0.03 MiB/s |
| Three members under 200 writes per second (soak test, final chain test) | 26,800 | 54.3 s | 493 | 0.01 MiB/s |

So an hour of oplog at 200 small writes per second (720,000 entries) replays in about five minutes on an idle replica set, and in about 25 minutes while the same load goes on; the stored oplog of such entries compresses so well that a byte rate alone would promise seconds. Restore close to a base (take bases more often), select only the databases you need, or restore into a quieter target (`target_connection_id`, admin) when the replay time matters.

## Copies in another region

A stream's `copy_targets` (up to three storage targets other than its own) keep the whole stream restorable in another region. Base backups are copied like a job's backups, and the copy queue copies every live oplog chunk to each copy target, checks it against the chunk's SHA-256 on the way, uploads it with the copy target's own Object Lock, retries a failed copy with backoff and purges the copy once its chunk is purged and the copy's lock ended. A point-in-time restore reads a **copy chain** when you name its target (`source_target_id`, *Read from* in the restore wizard), or by itself when the primary base or a chunk is missing or cannot be read. A chain is used only when the base and every chunk in the range have a completed copy on that target; otherwise the restore is refused naming the first missing chunk. See [cross-region disaster recovery](production.md#cross-region-disaster-recovery).

## Runbook: restore to the minute before an accident

Someone dropped a collection, ran an `updateMany` without a filter or deleted the wrong documents. Production keeps running; you want the data as it was just before, next to it, and then copy back what was lost. An operator can do all of this (an admin only for a restore into another server).

1. **Contain it.** Stop the job or the user that does the damage, so the window you restore from is not overrun by more of it. Do **not** disable the PITR stream: the collector keeps capturing the oplog, which is what you restore from.
2. **Find the time.** Take it from the application's or MongoDB's logs, or find the bad operation in the oplog on the primary (times are the primary's clock, in UTC):

   ```javascript
   // mongosh, as a user that may read local.oplog.rs
   db.getSiblingDB("local").oplog.rs.find(
     { ns: /^shop\./, op: { $in: ["d", "u", "c"] }, wall: { $gte: ISODate("2026-10-09T14:00:00Z") } },
     { ts: 1, wall: 1, op: 1, ns: 1, "o.drop": 1, "o.dropDatabase": 1 }
   ).sort({ $natural: 1 }).limit(20)
   ```

   A `dropDatabase` or `drop` shows up as `op: "c"`; a mass update or delete as many `u` or `d` entries with one `wall` second.
3. **Check the window.** In **Connections → Point-in-time recovery**, the stream's newest window must cover the time, its collector must be running, and no gap may lie between the window's start and the time (`GET /api/v1/pitr/streams/{id}` lists `windows`). A time after the newest stored chunk is not restorable yet: the collector stores a chunk every interval (60 s by default), so wait for the next one.
4. **Pick the target.** "The minute before" is a time `S` (RFC 3339): every write up to and including the second `S` is restored. If writes you need happened in the same second as the accident, restore to the exact entry instead: `"ts": {"t": …, "i": …}` of the bad entry restores everything before it (API and MCP).
5. **Check.** In the wizard (**Restore to a time**), pick the window, enter `S`, select the databases (fewer databases restore faster) and press **Check**: the preflight shows the base it chose, the chunks to replay, the [estimated duration](#restore-time-rto) and every check. From the command line:

   ```bash
   mongorescue restore --pitr <stream-or-connection-id> --at 2026-10-09T14:31:00Z --database shop
   ```

   runs the same preflight first and refuses a failing one. Fix what it reports: missing privileges on the target, free disk space, a missing key.
6. **Restore.** Press **Restore** and confirm, or add `--wait` to the command. Every restored database becomes `<db>_rescue_<YYYYMMDD_HHMMSS>_<id>`; nothing that exists is touched. Follow it in **Restores** or with `GET /api/v1/restores/{id}`.
7. **Verify the clone.** The restore record must be `completed`, with `pitr.ops_applied` equal to `pitr.ops_replayed` and no warning that the replay could not be cross-checked. Look at the data: the documents of the minute before must be there, the effect of the accident must not.
8. **Put the data back.** Copy what was lost from the clone into production, for example one collection with `mongodump --db shop_rescue_… --collection orders --archive | mongorestore --archive --nsFrom 'shop_rescue_….orders' --nsTo 'shop.orders'` (add `--drop` only if you mean to replace the collection), or with an aggregation that `$merge`s the documents back. Or point the application at the clone for a while. In-place point-in-time restores are not offered ([#142](https://github.com/YigitCittan/mongorescue/issues/142)).
9. **Clean up.** Drop the clones once you are done: they take room on the server and in the next base backups.

## How it works

- **Collector.** One goroutine and one long-lived connection per enabled stream. Every interval it reads the majority-committed oplog entries after its last position, so writes that are rolled back after a failover are never kept, and streams them through gzip, age and SHA-256 to the stream's storage target as `_mongorescue/oplog/<connection>/<replica set>/<chain>/<from>-<to>.bson.gz.age`. The chunk and the new position are committed in one transaction; a restart resumes from it, and a chunk interrupted by a crash leaves no object and is read again. While it is behind, the collector stores one-interval chunks back to back until it has caught up.
- **Base backups** are `mongodump --oplog --archive --gzip` runs of the whole instance, always encrypted, stored as `_mongorescue/base/<connection>/<replica set>/<yyyy>/<mm>/<id>.archive.gz.age`. They record `t_before` (`hello.lastWrite.opTime` before the dump) and `t_after`, their consistent point: `hello.lastWrite.majorityOpTime` with its term, once the majority has reached the newest write after the dump, so a write the dump holds cannot be rolled back (a base whose writes are not majority-committed within 2 minutes fails). A base is eligible when one run of verified chunks covers `[t_before, t_after]` and holds the entry at `t_after` in its term. They appear in the backups list with `scope: "instance"`, run under their own concurrency key so the connection's database backups keep running, are never touched by job retention and cannot be restored as database backups.
- **Gaps.** When the oplog no longer holds the collector's position (the window was overrun while it was stopped or too slow) or the replica set changed, the chain ends, a new one starts at the oldest entry still there, a base backup is taken (unless turned off) and `pitr.chain_broken` is raised, at most once per 10 minutes per stream. After a gap the collector waits one interval before it looks again, doubling with every gap in a row up to 10 minutes. A gap, a replica set change or a divergence is only acted on once the primary confirms it: every tick reads one member, and a secondary that is behind or freshly synced never ends a chain. A restore cannot cross a gap; the windows of a stream show it.
- **Divergence.** When the entry at the collector's position vanished or changed term (a forced reconfiguration, majority data loss), the chain ends at the newest chunk that is still part of the history, the later chunks are marked `superseded` and never replayed, and `pitr.diverged` is raised.
- **Retention.** Chunks that end before the `t_before` of the oldest kept eligible base of their chain, ended chains without a kept base and superseded chunks are deleted, like bases, with the [delete grace period](security.md): their objects stay until it ends and the purge removes them.
- **Orphans.** A crash between storing a chunk and committing it leaves an object no chunk names (its key cannot be reused: the end of the range moves between attempts). Storage scans report such objects as orphans once they are an hour old, and retention deletes them after the delete grace period.
- **Verification.** The integrity sweep re-reads every chunk not checked in the last 7 days (hash, decryption, decompression, entry count, first and last positions and terms) and checks that each chunk starts where the previous one ends. A failure raises `verification.failed`.

## Monitoring

- **Events** (selectable by notification rules): `pitr.chain_broken` and `pitr.diverged` (critical), `pitr.collector_failed` / `pitr.collector_recovered`, `pitr.lag_high` / `pitr.lag_recovered` (the lag exceeds max(5 min, 5 × the interval); raised once per episode, also across restarts) and `pitr.window_low` (the oplog headroom, how long the collector may stop before entries are lost, is below max(6 h, 3 × lag); also raised once per episode, across restarts too). The replica set ID seen first is stored, so a set re-initiated under the same name while the collector was stopped is treated as a gap.
- **Metrics** `mongorescue_pitr_*` with the label `stream`: see [metrics.md](metrics.md).
- **Readiness.** A PITR RPO means that the oplog up to that point is captured and can be restored to a point in time; the readiness row says so next to its RPO. The [readiness report](api.md#recovery-readiness) lists the streams. A database row's effective RPO is the better of its job RPO and the stream's durable lag (now minus the end of the last stored chunk) while the stream's window is open and its collector healthy. New reasons: `pitr_chain_broken` (a gap, a divergence or a chunk that failed verification and no newer base) and `pitr_collector_down` (fail), `pitr_lag_high`, `pitr_window_low`, `pitr_no_window` (no eligible base for the current chain yet) and `pitr_chain_test_failed` (the newest chain test failed) (warn). Each stream of the report carries its [RTO estimate](#restore-time-rto) (`rto`).

## What is tested

- **Every pull request** ([testing.md](testing.md)): the collector, the filter, retention and the plan in unit tests; on single-member replica sets in the integration suite, the restore to just before a `dropDatabase` (whole instance and one database, the sources untouched), a restore stopped inside a split transaction, restarts of the collector, gaps, and a scheduled chain test (base one restored to base two's point and compared with base two's manifest).
- **Every night** on a three-member replica set ([testing.md](testing.md#pitr-on-a-three-member-replica-set)): failovers during collection and during a restore, a rollback through network isolation (the rolled-back writes are in no chunk) and divergence after a forced reconfiguration, with the stored oplog compared entry by entry with the members' oplogs.
- **Every week**, a soak test collects for 5.5 hours under a write load (no gap, no false break, bounded storage, retention working); the 7-day run is a [manual procedure](testing.md#pitr-soak-test).

## Not there yet

- In-place point-in-time restores (#142).
- The operation browser (restoring to just before one oplog entry is possible through the API with `ts`).
- Sharded clusters (#58) and high availability of the collector.
