# Point-in-time recovery (experimental)

> **Experimental.** This release collects what point-in-time recovery needs: the oplog of a replica set and base backups of the whole instance. **Restoring to a point in time is not available yet**; it comes with [#57](https://github.com/YigitCittan/mongorescue/issues/57). The stream API and the dashboard panel may change without a deprecation period. The design is in [design/pitr.md](design/pitr.md).

Point-in-time recovery (PITR) will restore a replica set, or one of its databases, to any moment within a window. It needs two things, which MongoRescue now collects for every enabled **PITR stream**:

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
  | Point-in-time restores (#57) | `restore`, `readWriteAnyDatabase` and `dbAdminAnyDatabase` (or `anyAction`) on the target, ideally through a separate user and connection. |

  Creating or enabling a stream checks the oplog access through `connectionStatus` and refuses it with a clear message otherwise.
- **MongoDB Database Tools** as for every backup.

## Enabling a stream

In the dashboard, open **Connections**: the **Point-in-time recovery** panel lists the streams and offers a form to enable one for a connection (admin). Through the API, `POST /api/v1/pitr/streams` with at least `{"connection_id": "…"}`; see [api.md](api.md#point-in-time-recovery-streams-experimental) for every field.

| Setting | Default | |
| --- | --- | --- |
| Chunk interval | 60 s | 15 to 900 s. Each chunk holds the oplog entries of one interval; above 4 GiB the collector halves it for the next chunks. |
| Base schedule | `0 2 * * *` | A cron schedule. The first base is taken as soon as the collector has started the chain. |
| Base retention | 7 bases or 14 days | Pinned bases and the newest eligible base are always kept. |
| Base on gap | on | Take a base backup as soon as a gap breaks the chain. |
| `oplog_max_days` | unset | Delete chunks older than this many days, even if that shortens the window (privacy). |
| Read preference | `secondaryPreferred` | Where oplog reads go. |

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
- **Readiness.** Point-in-time restores are not available yet (#57): a PITR RPO only means that the oplog up to that point is captured, and the readiness row says so next to its RPO. The [readiness report](api.md#recovery-readiness) lists the streams. A database row's effective RPO is the better of its job RPO and the stream's durable lag (now minus the end of the last stored chunk) while the stream's window is open and its collector healthy. New reasons: `pitr_chain_broken` (a gap, a divergence or a chunk that failed verification and no newer base) and `pitr_collector_down` (fail), `pitr_lag_high`, `pitr_window_low` and `pitr_no_window` (no eligible base for the current chain yet) (warn).

## Not there yet

- Restoring to a point in time, whole-instance or per database (#57), the restore wizard and the CLI and MCP tools.
- The scheduled chain test and the operation browser.
- Sharded clusters (#58) and high availability of the collector.
