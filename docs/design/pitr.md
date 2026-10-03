# Design: point-in-time recovery for replica sets

Status: agreed (#55, roadmap 3.1). It shapes the oplog collector (#56) and PITR restore (#57). Sharded clusters (#58) are out of scope.

Point-in-time recovery (PITR) restores a replica set, or one database of it, to any moment within a window. It needs two parts:

- a **base backup** of the whole instance, taken with `mongodump --oplog`;
- a **continuous oplog chain** that starts before the base and runs up to "now".

This changes the backup model: until now every backup covered one database.

## Facts about the tools that shape the design

Spikes S1–S3 checked these on MongoDB 5.0.33 and 8.0.32 with Database Tools 100.12.2 and 100.16.0, which behaved the same.

1. **`mongodump --oplog`** works only for a whole instance on a replica set member. It refuses `--db`, `--collection` and `--query`. Writes made during the dump land in the archive's `oplog` namespace, so the dump is consistent as of its last oplog entry.
2. **`mongorestore --oplogReplay`** replays the `oplog` namespace of an archive, or `oplog.bson` in a directory.
   - `--oplogLimit=<secs>[:<ord>]` is exclusive.
   - Entries are applied through `applyOps`, and inserts become upserts, so replaying entries that are already in the base is harmless.
3. **mongorestore refuses `--nsFrom`/`--nsTo`, `--nsInclude` and `--nsExclude` together with `--oplogReplay`** (S1, exit 1). The oplog cannot be renamed or filtered by the tool, so MongoRescue rewrites and filters it itself for safe clones and single-database restores.
4. **Replay checks privileges per operation** (S1). `restore` alone fails at the first update, and `restore` + `readWriteAnyDatabase` fails at `dropDatabase`. `restore` + `readWriteAnyDatabase` + `dbAdminAnyDatabase` replayed CRUD, DDL and transactions on 5.0 and 8.0. That is the documented minimum; `anyAction` also works.
5. **Change streams are not used.** They deliver change events, not oplog entries, so `--oplogReplay` cannot consume them.

## Model

- **PITR stream.** One per connection, which must be a replica set. It holds the storage target, the base schedule (cron), base retention, the chunk interval and an enabled flag. It is configured separately from jobs.
- **Base backups.**
  - Scheduled `mongodump --oplog --archive` runs of the whole instance, through the backup engine with a new `Scope=instance`: `database_name` is empty and `--oplog` replaces `--db`.
  - The engine records `T_before` and `T_after` from `hello.lastWrite.opTime` before and after the dump. `T_after` is the base's consistent point.
  - A base is PITR-eligible only if the chain covers `[T_before, T_after]` without a break.
  - Key: `_mongorescue/base/<conn_id>/<rs>/<yyyy>/<mm>/<id>.archive.gz.age`.
  - Run key: `pitr-base:<conn>`, so per-database jobs keep running.
- **Coexistence with jobs.** Per-database jobs keep their own RPO. The dashboard warns when a job and a base backup are scheduled within 15 minutes of each other on one connection. Job retention never touches base backups.

## Collector (#56)

- **Reading.**
  - Bounded range reads go through the driver in `internal/mongoconn`. At each tick, `B = hello.lastWrite.majorityOpTime` and the collector reads `find local.oplog.rs {ts: {$gt: A, $lte: B}}` in `$natural` order as raw BSON into an `io.Writer`: `mongoconn.ReadOplog(ctx, uri, A, B, w)`.
  - Memory use is bounded by one cursor batch.
  - Only majority-committed entries are stored, so writes that are rolled back after a failover are never kept.
  - Read preference is `secondaryPreferred` by default.
  - One long-lived client per stream.
- **Chunks.**
  - The pipeline is the backup engine's: driver → pipe → scanner (counts entries, records the first and last `ts` and `t`) → gzip → age → SHA-256 → `Storage.Save`.
  - Key: `_mongorescue/oplog/<conn_id>/<rs>/<chain>/<from>-<to>.bson.gz.age`. Timestamps are written as `%010d.%010d`, so keys sort in time order.
  - The interval defaults to 60 s (15–900 s). During catch-up each chunk is capped at one interval; above 4 GiB the interval is halved.
  - Empty ranges still write a chunk, so coverage can be proven.
  - A crash in the middle of a chunk leaves no object, and the range is read again.
- **State.** After each upload, one transaction writes the chunk row and `pitr_state` (`last_to`, `last_term`, chain). A restart resumes from `last_to`.
- **Verification.** The integrity sweep gains a chunk item: hash, decrypt, gunzip, walk the BSON, then compare the count, timestamps and SHA-256. It also checks continuity: each chunk's `from` must equal the previous chunk's `to`.
- **Gaps.**
  - The window is overrun when the oldest oplog entry is newer than `A`. A gap is also raised when the replica set name or ID changes.
  - A gap ends the chain, starts a new one from the oldest entry still available, takes an immediate base backup (on by default, can be turned off per stream) and raises a critical alert.
  - Headroom is `A − oldest entry`. The collector warns when it drops below `max(6h, 3 × lag)`.
- **Divergence.** At start, the entry at `ts == A` must still exist and have the stored term `t`. If it doesn't (forced reconfig, majority data loss), the collector:
  - ends the chain (`diverged`);
  - marks the later chunks `superseded`;
  - starts a new chain;
  - raises a critical alert.

  `h` is always 0 since MongoDB 4.2, so `(ts, t)` is the check.
- **Lag.**
  - Lag is the primary's `lastWrite − last_to`.
  - The durable RPO is `now − wallclock(last_to)`, about the interval plus upload time.
  - Breaches are persisted like `rpo_breaches`.

## Retention

- **Bases:**
  - Default: keep 7, or 14 days.
  - Pins protect a base.
  - The newest eligible base is never deleted.
- **Chunks:**
  - A chunk is deleted once its `to` is older than `T_before` of the oldest kept eligible base in its chain.
  - A chain with no kept base is deleted whole.
  - The optional `oplog_max_days` shortens the window for privacy.
- **PITR window** per chain: from the earliest consistent point of its eligible bases to the newest chunk's `to`. The UI lists windows, because a gap splits them.
- **Locking:** deletion takes `runs.LockDeletion("pitr:<stream>")`.

## Restore (#57)

- **Target time.**
  - A time S maps to `--oplogLimit=(S+1):0`.
  - An exact timestamp can come from an operation browser (later).
- **Base selection.**
  - Use the newest eligible base with `T_after ≤ target`, where every chunk from `T_before` up to the target is committed.
  - Record the selection on the restore.
- **Pass 1 (data):**
  - The existing streaming restore of the base, without `--oplogReplay`.
  - A whole-instance safe clone renames `$db$.$coll$` to `$db$_rescue_<ts>.$coll$`.
  - `admin.*`, `config.*` and `local.*` are always excluded.
  - For a single database: `--nsInclude=<db>.*` plus today's rename.
- **Pass 2 (oplog):**
  - `mongorestore --archive --oplogReplay --oplogLimit=…`, reading a **synthetic oplog-only archive** from stdin.
  - The archive is built on the fly by `internal/oplog`: chunks are fetched in order, hashed while streaming, decrypted, gunzipped and filtered.
  - No temporary copy is made.
  - S2 confirmed that mongorestore accepts an oplog-only archive on stdin when the namespace is database `""`, collection `oplog`. `local.oplog.rs` is refused. No temp file is needed.
- **Namespace filter (`internal/oplog`, fuzzed):**
  - keeps the entries of the selected databases;
  - rewrites `ns` and the namespace-carrying arguments of command entries (`create`, `drop`, `renameCollection`/`to`, `createIndexes`, `startIndexBuild`, `commitIndexBuild`), recursing into `applyOps`, which covers transactions and `partialTxn`;
  - drops `ui` and no-op entries;
  - refuses renames across the selection boundary;
  - refuses unknown command entries;
  - enforces `--oplogLimit` itself and ends the synthetic archive before the limit. In archive mode mongorestore exits 1 ("archive reading interrupted") when entries at or past the limit are still in the stream, even though the replay was correct (S1);
  - turns each `startIndexBuild`/`commitIndexBuild` pair into one `createIndexes` at the commit's position, and drops `abortIndexBuild`. mongorestore defers `commitIndexBuild` to the end of the replay, so a build followed by a rename or drop would otherwise recreate an empty collection (S1);
  - also handles `collMod`, `dropDatabase`, `dropIndexes`, transactions (`admin.$cmd` `applyOps` with `partialTxn`/`count`) and cross-database renames, which appear as a create of `tmp*.renameCollection` in the target, copy inserts, a rename within the target and a drop in the source. It drops `config.system.indexBuilds` writes, `fromMigrate` entries and retryable-write fields (`lsid`, `txnNumber`, `stmtId`, `prevOpTime`);
- **Preflight additions:**
  - chain coverage, and keys for every encryption mode in the chain;
  - `restore`, `readWriteAnyDatabase` and `dbAdminAnyDatabase` (or `anyAction`) on the target;
  - disk space;
  - Database Tools ≥ 100.12.
- **Verification:**
  - Chunk hashes are checked inline.
  - The filter counts the operations it emits, with `applyOps` expanded and skipped entries left out, and compares that with mongorestore's "applied N oplog entries". The chunk entry count is not comparable (S1).
  - A scheduled **chain test** PITR-restores base B1 up to the consistent point of B2 into a `_rescue_verify_` clone, compares the result with B2's manifest, then drops the clone.

## Spike results (S1–S3)

Also confirmed:

- `--oplogFile` is refused together with `--archive`.
- `--oplogLimit` is exclusive, so a time S maps to `(S+1):0`.
- Replaying entries that are already in the base is idempotent.
- The range read on `local.oplog.rs` is a bounded scan: 27 entries examined for 25 returned, even with an oplog of 807k entries on 5.0.
- `hello.lastWrite.majorityOpTime` has the shape `{ts: Timestamp, t: int64}`.

Not yet tested:

- a real secondary or a 3-node replica set (covered by the nightly job);
- whether mongorestore checks the archive CRC;
- the privileges for writes to `admin.*` and `config.*`.

## Data model, API, UI, metrics

- **Migration `0021_pitr.sql`** (STRICT; timestamps are stored as `*_t`/`*_i` integer pairs):
  - `pitr_streams`, `pitr_chains`, `oplog_chunks` (status `committed|superseded|pruned`, `UNIQUE(target_id, storage_key)`), `pitr_state`.
  - Base backups keep `scope`, `pitr_stream_id`, `t_before` and `t_after` in the JSON `data` column.
- **API:**
  - `/api/v1/pitr/streams` (read scope to list, admin to change), `/{id}` (state, windows, lag, headroom), `/{id}/chunks`, `POST /{id}/base` (operator);
  - restores take `pitr: {stream_id, at | ts}`, and so does preflight.
- **CLI and MCP:** `mongorescue restore --pitr <conn> --at <RFC3339>`, plus MCP `pitr_status` and `pitr_restore` (admin).
- **UI:** a Point-in-time tab per connection: the window timeline with gaps, the collector's state, lag and headroom, the base backups, and a restore wizard.
- **Readiness:**
  - Streams appear in the report.
  - A database row's effective RPO is the better of its job RPO and the PITR RPO.
  - New reasons: `pitr_chain_broken` and `pitr_collector_down` (fail); `pitr_lag_high` and `pitr_window_low` (warn).
- **Events:** `pitr.chain_broken`, `pitr.diverged`, `pitr.lag_high`, `pitr.lag_recovered`, `pitr.window_low`, `pitr.collector_failed` and `pitr.collector_recovered`.
- **Metrics** (`mongorescue_pitr_*`, label `stream`): `collector_up`, `lag_seconds`, `last_chunk_timestamp_seconds`, `chunks_total{result}`, `chunk_bytes_total`, `oplog_headroom_seconds`, `window_start_timestamp_seconds`, `window_end_timestamp_seconds` and `chain_breaks_total{reason}`.

## Security

- **Privileges:**
  - The collector needs `find` on `local.oplog.rs`: `backup`, `read` on `local`, or a custom role.
  - Base backups need the `backup` role.
  - Restores need `restore`, `readWriteAnyDatabase` and `dbAdminAnyDatabase` (or `anyAction`) on the target, ideally through a separate user and connection.
  - `readAnyDatabase` does **not** grant `find` on `local.oplog.rs`; `backup`, `read` on `local`, or a custom role does (S3).
  - Preflight checks each of these through `connectionStatus`.
- **Oplog contents.** The oplog holds every write, including data that was later deleted. Deleted data survives in the chunks until retention removes it, which matters for erasure requests. Therefore:
  - **encryption is required** for PITR streams: a stream can't be enabled on a target without age keys;
  - `oplog_max_days` exists;
  - the API shows chunk metadata only;
  - configuration and PITR restores are admin-only at first;
  - `docs/privacy.md` explains all of this.

## Driver use (AGENTS.md change)

The rule "the MongoDB driver only in `internal/mongoconn`" keeps every database connection in one adapter. `internal/oplog` must parse and rewrite nested oplog entries, and a hand-written walker for `applyOps` would be riskier than the driver's codec. So `internal/oplog` may import **only** `go.mongodb.org/mongo-driver/v2/bson` (a codec that opens no connections). The CI import guard is narrowed to match in the PR that adds `internal/oplog`.

## Decisions

| # | Decision |
| :--- | :--- |
| 1 | The collector uses bounded range reads up to `majorityOpTime` through `mongoconn`. A tailable cursor and mongodump slices were rejected. |
| 2 | Chunk keys follow `_mongorescue/oplog/<conn_id>/<rs>/<chain>/<from>-<to>.bson.gz.age`. |
| 3 | `internal/oplog` may use the driver's `bson` package and nothing else from the driver. |
| 4 | Pass 2 reads a synthetic oplog-only archive (namespace `"".oplog`) from stdin; S2 confirmed it. |
| 5 | The base's consistent point is recorded through `hello` (`T_before`/`T_after`). |
| 6 | A gap triggers an automatic base backup and a critical alert. It can be turned off per stream. |
| 7 | Encryption is required for PITR streams. |
| 8 | PITR restore is admin-only in the first release. Operators for safe clones are reconsidered later. |
| 9 | The default chunk interval is 60 s (15–900 s). |
| 10 | The collector's read preference is `secondaryPreferred`. |
| 11 | Base retention keeps 7 bases or 14 days, plus pins. `oplog_max_days` is unset by default. |
| 12 | Per-database jobs and PITR streams coexist, with a warning when their schedules overlap. |

## Testing

- **Unit (hermetic):**
  - the BSON filter, with fixtures for MongoDB 5.0–8.0 (CRUD, DDL, transactions, index builds, time series) and fuzzing;
  - a round trip of the synthetic archive;
  - the collector against a fake reader (gaps, divergence, catch-up, interval halving, a crash in the middle of a chunk);
  - retention planning;
  - the migration step.
- **Integration (single-node 8.0 replica set job):**
  - base backup, writes, `dropDatabase`, more writes; restore to just before the drop into a safe clone, and assert the **source database is untouched**;
  - per-database PITR;
  - the chain test;
  - a gap, produced with `replSetResizeOplog` and a paused collector.
- **Nightly 3-node replica set (docker compose):**
  - failover during writes: no gap and no duplicate;
  - rollback through network isolation: the rolled-back writes appear in no chunk;
  - divergence after a forced reconfig;
  - Database Tools 100.12.2 and the latest version.

## Phasing

| Release | Work |
| :--- | :--- |
| **1.1** | Spikes S1–S3; migration and store; `mongoconn.ReadOplog`/`OplogWindow`; `internal/oplog` walker and chunk writer; collector service (experimental, off by default); instance-scope base backups. |
| **1.2** | Chunk verification, retention, readiness, the dashboard tab and alerts, the nightly 3-node job. |
| **1.3** | Whole-instance PITR restore into safe clones (synthetic archive, preflight, API, CLI). |
| **1.4** | Per-database PITR with the namespace filter, chain tests, MCP tools, the operation browser. |

## Risks

- **Tools behaviour can differ between versions.** Mitigation: a minimum tools version, tested on two.
- **Replay is single-threaded**, so RTO grows with the oplog span. The RTO estimate includes oplog bytes.
- **The oplog format can drift between MongoDB versions.** Mitigation: a fixture corpus per version, and unknown command entries are refused.
- **Many small objects.** The chunk interval is configurable.
- **The collector has no high availability before roadmap 5.2.** Mitigation: alerts on `collector_up` and on headroom.
- **Wall-clock targets use the primary's clock.** The UI shows the server time it used.
