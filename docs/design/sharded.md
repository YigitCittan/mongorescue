# Research: consistent backups of sharded clusters

Status: research, not agreed (#58, roadmap 3.4). Nothing here is implemented. Phase 0 (detect and refuse) shipped in #222; the rest is a proposal for the maintainer to accept, change or reject. Statements about other tools are cited; statements marked **(analysis)** are this document's own reasoning and must be confirmed by the spikes in [Test strategy](#test-strategy) before they shape code.

## Problem statement

MongoRescue backs up one replica set at a time. A base backup is `mongodump --oplog` of a whole member, and point-in-time recovery (PITR) adds a continuous oplog chain per replica set ([design/pitr.md](pitr.md), [pitr.md](../pitr.md)). A sharded cluster is several replica sets that must agree: a config server replica set (CSRS) that holds the routing metadata (`config.databases`, `config.collections`, `config.chunks`, `config.shards`) and N shard replica sets that hold the data, fronted by `mongos` routers.

A backup of such a cluster is consistent only if all of these describe the same moment:

- every shard's data,
- the CSRS metadata that says which chunk lives on which shard,
- transactions that span shards (each shard commits its part at its own timestamp).

Taking `mongodump` through a `mongos` gives none of that. MongoDB's own tools documentation says `--oplog` has no effect through a `mongos` and cannot be used on a sharded cluster, and tells you to pause cross-shard transactions, DDL and chunk balancing for a consistent dump ([mongodump][mongodump]). Chunk migrations in particular can leave documents missing or duplicated in an archive that still verifies. That is why MongoRescue refuses such backups by default today ([production.md](../production.md#sharded-clusters-mongos)).

Goal of this research: decide what a consistent, streaming, single-binary sharded backup would be, in which phases, at what cost, and what we should not build.

## What MongoDB and other tools do

### MongoDB's documented logical procedure (quiesce)

The manual's procedure for a self-managed sharded cluster is: stop the balancer (`sh.stopBalancer()`, which waits for a running round), lock the cluster with `fsyncLock` on `mongos`, run `mongodump` against `mongos`, unlock, restart the balancer. It requires MongoDB 7.1, or 7.0.2, 6.0.11 or 5.0.22 and later, where `fsync` and `fsyncUnlock` can run on `mongos`; the user needs the `fsync` privilege (built into `hostManager`). The cluster must stay locked for the whole dump, and the page says it "can only produce a consistent backup if they are followed exactly and no operations are in progress", with no chunk migration, resharding or schema transformation running ([backup with dumps][mongo-dump-backup], [fsync][fsync]). Atlas, Cloud Manager and Ops Manager are the ones documented to back up "with the balancer and running transactions" ([backup with dumps][mongo-dump-backup]). Two details matter to us:

- A locked cluster blocks all writes (`lockCount` above zero blocks writes on the server and cluster), and a primary that goes down loses the lock: "the resulting backup may be inconsistent or unusable" ([fsync][fsync]).
- The result is one point in time with no PITR; the page does not mention the oplog ([backup with dumps][mongo-dump-backup]).

Restoring such a dump into a sharded cluster means stopping the balancer, creating and sharding each collection on the destination with the source's shard key **before** restoring, running `mongorestore` through `mongos` with `--nsExclude='config.*'`, and not using `--drop` (it removes the sharding configuration) ([restore from dumps][mongo-dump-restore]).

### Physical: `$backupCursor` and `$backupCursorExtend`

Ops Manager opens a `$backupCursor` on a node of each shard or replica set to copy data files "in a consistent state while still accepting writes", and also tails the oplog of the primary ([Ops Manager backup overview][opsm-overview]). Percona documents the same stages for Percona Server for MongoDB: run both on one member of each shard and of the config server replica set, wait until the cursor is complete everywhere, take the latest `oplogEnd` across all of them as the common target time and pass it to `$backupCursorExtend` ([Percona backup cursor][psmdb-cursor]). Percona's blog notes that the extended WiredTiger log can contain operations beyond that target, so a restore has to trim the oplog during startup ([Percona blog][psmdb-extend-blog]). The stages work only where you can read data files (filesystem access to the `mongod` host), and Percona says its own tool uses them for physical backups ([Percona backup cursor][psmdb-cursor]). Whether MongoDB's own `$backupCursor` is Enterprise-only should be confirmed against the MongoDB manual (the page was not reachable during this research, and it is an open question below); Percona Server implements them in the open-source distribution.

### Percona Backup for MongoDB (PBM)

- **Architecture.** A `pbm-agent` runs on every `mongod` node, including the config servers; the `pbm` CLI instructs the agents through state kept in MongoDB; agents write straight to remote storage and `mongos` is not involved ([PBM overview][pbm-intro], [PBM sharding blog][pbm-blog]).
- **Consistency point.** PBM reports the time at which the cluster state was captured, in MongoDB's logical clock ([PBM overview][pbm-intro]); it advertises "cluster-wide consistent backups" and distributed-transaction consistency with MongoDB 4.2+ ([PBM comparison][pbm-comparison]).
- **Logical backup, from source.** In `doLogical`, a leader node drives the state machine while followers wait; each replica set records its first write timestamp, an oplog slicer starts at that timestamp **concurrently with** the dump and keeps uploading slices, and when the dump is done each shard records its last saved timestamp; the leader then sets the cluster first and last write ([PBM `logical.go`][pbm-logical]). In other words: every shard is dumped without a pause, and the oplog from the dump's start gives each shard a path to a common time. For config servers on selective backups it filters `config.databases`, `config.collections` and `config.chunks` to the selected namespaces ([PBM `logical.go`][pbm-logical]).
- **PITR.** Slices are saved per node chosen by priority in each shard and config server replica set ([PBM PITR][pbm-pitr]). Valid restore ranges come from `pbm list`; after a restore, slices after the restore timestamp become invalid and a new backup is required ([PBM PITR restore][pbm-pitr-restore]).
- **Restore.** The tutorial says to stop the balancer and the `mongos` nodes, make sure nothing writes, and restore to a time inside a listed range; logical restore inserts data on each primary and lets replication propagate it ([PBM PITR restore][pbm-pitr-restore], [PBM sharding blog][pbm-blog]).
- **Balancer.** PBM's current docs we read do not say it pauses the balancer for a logical backup; an older v1.0 note told users to stop it. We did not find a primary source for the current behaviour, so it is an open question whether PBM relies on the oplog to reconcile migrations **(analysis)**.

### Oplog facts that matter

- Documents moved by a chunk migration are written on the recipient and deleted on the donor with oplog entries flagged `fromMigrate`. MongoDB's tickets describe the `showMigrationEvents` option for change streams, which returns entries with `fromMigrate: true` ([SERVER-88167][server-88167]); we found no manual page that defines the flag fully, so its exact scope (for example whether every cloner sets it) is a spike item. Consequence **(analysis)**: replaying all shards' entries as they are, directly into shards of the same topology, is self-consistent; replaying them through a `mongos` is not, because a donor-side `fromMigrate` delete would remove a document that now lives (legitimately) on the recipient.
- Cross-shard transactions commit independently on each shard, so a restore point has to be a time at which each shard's committed state is consistent with the others. Oplog entries carry cluster-time timestamps, so one timestamp is meaningful on all shards **(analysis, confirm in spike)**.
- Resharding and `moveCollection` (8.0) change the placement of whole collections; MongoDB tells you to pick a backup window when they are not running ([backup with dumps][mongo-dump-backup]).

## Options compared

| Option | Consistency | Needs Enterprise? | Downtime or balancer impact | Complexity | Fit with the streaming model |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **A. Quiesced dump through `mongos`** (documented procedure: stop balancer, `fsyncLock` via `mongos`, `mongodump`) | One moment, no PITR. Breaks if a primary fails over during the lock. | No; needs 5.0.22/6.0.11/7.0.2/7.1 or later and the `fsync` privilege | **Writes blocked for the whole dump**; balancer paused | S: a few driver commands and a guard that always unlocks | Perfect: the existing single `mongodump` stream. But a locked production cluster is a hard sell. |
| **B. Per-shard `mongodump --oplog` plus CSRS dump, common cut (recommended)** | Every shard and the CSRS restorable to one cluster timestamp T via oplog replay; transactions and migrations reconciled by the oplog **(analysis)** | No | No write pause; balancer pause optional (see below) | L: topology discovery, per-shard credentials, cut computation, planner | Good: N streams, each through the existing pipeline; needs network access to each shard member, not only `mongos` |
| **C. B plus continuous oplog per shard and CSRS (PITR)** | As B, for any T inside the intersection of all chains | No | None | L on top of B: group of streams, intersection logic, lag skew | Good: the collector already is one stream per replica set |
| **D. Physical (`$backupCursor`/`$backupCursorExtend`)** | Consistent at a common target time | Check; Percona Server has it | None | XL: needs file access on each host, agent-like process | Poor: copies data files, not a stream a remote binary can pull, and breaks "single binary, no agents" |
| **E. Volume snapshots with `fsyncLock`** | Consistent if all shards are locked at once | No | Writes blocked while snapshotting | M | Not MongoRescue's job: storage-layer, host-level |
| **F. Delegate to PBM, Atlas or Ops Manager** | Their guarantees | No (PBM) | Theirs | S to document, M to monitor | Outside the model; MongoRescue could at most watch and verify their output |

Notes:

- B and C read nothing but `mongodump` archives and bounded oplog ranges, the two primitives MongoRescue has already hardened (`mongoconn.ReadOplog`, `internal/oplog`).
- A is the only option that is consistent without any new restore logic, which makes it a possible interim ("quiesced mode", opt-in) but a poor default.
- D is what the commercial tools use for large data sets. It is the right answer for multi-terabyte shards and the wrong one for this product; [comparison.md](../comparison.md) already points those users to PBM.

## Recommendation

Build **B, then C, then the restore choreography**, in that order, as opt-in features behind a connection kind `sharded_cluster`, keeping phase 0's refusal for plain `mongos` connections without it.

### Model

- **Cluster connection.** A `sharded_cluster` connection holds the `mongos` URI(s) used for discovery and metadata, and per-member credentials. The shard member list comes from `config.shards` (`rs/host1,host2`) and the CSRS from `getShardMap` or the `mongos` `hello`. Credentials are stored through `internal/secretbox` like today's URIs; members are reached **directly**, so the host names in `config.shards` must be routable from MongoRescue **(open question: network and per-shard users)**.
- **Cluster backup.** A parent record with one child per replica set (CSRS plus each shard). Each child is an ordinary instance-scope base backup (`mongodump --oplog --archive`, pipeline unchanged). Keys: `_mongorescue/cluster/<conn_id>/<cluster_backup_id>/<rs>/…`.
- **Common cut T.** Each child records `T_before_i` and `T_after_i` as today. T is the maximum `T_after_i`. For each shard i the engine then reads the bounded oplog range `(T_after_i, T]` with `mongoconn.ReadOplog` (majority-committed only) and stores it next to the archive as an oplog chunk. A shard's base is "restorable to T" once that range is stored. This reuses the chunk format, so phase 2 is a continuation, not a new format.
- **Metadata.** The CSRS is dumped like a shard (`config` is included for it, which today is always excluded) and replayed to the same T. Restoring it requires rewriting `config.shards` (host strings, optionally names) for a new cluster; PBM's selective restore keeps router config collections for the same reason ([PBM `logical.go`][pbm-logical]).
- **Balancer.** Configurable `balancer: pause | leave`. Recommendation: default to `pause` for the dump window and restore the prior state in a deferred, context-bound step that also runs after a crash on the next start (a persisted marker), because it is the documented safe practice ([backup with dumps][mongo-dump-backup]) and the cheapest way to avoid in-flight migrations while the analysis of `leave` is unproven. Pausing needs `clusterManager`-class privileges and must be visible in the UI.
- **DDL and resharding.** Detect running resharding or `moveCollection` (config `reshardingOperations` and currentOp) and wait or fail with a sentinel error. `mongodump --oplog` itself fails when `renameCollection` and similar run during the dump ([mongodump][mongodump]); a failed child fails the parent and retries as a whole.
- **Verification.** Each child gets the existing archive verification. A cluster-level check adds: all children share T; per-collection counts summed across shards agree with an exact count at T taken in a restore test; every `config.chunks` range's owner holds a document count consistent with the chunk map; no collection appears on two shards outside the migration window. Orphaned documents on the donor are expected and not a failure **(analysis)**.

### Safe-clone restore for a sharded target

"Safe clone" in this codebase means a new namespace `<db>_rescue_<timestamp>` in the same server. For a sharded target there are two meanings and they differ in cost:

| | Clone in the same cluster (default) | Restore into another, empty cluster |
| :--- | :--- | :--- |
| What it does | Renames namespaces, pre-shards each clone with the source's shard key, restores **through `mongos`** | Rebuilds the same topology (CSRS, shards), then restores per shard directly and the CSRS |
| Touches production metadata | Adds `config.collections` entries, creates chunks on the target cluster; never changes existing ones | No: separate cluster |
| Oplog replay | Not possible with `mongorestore --oplogReplay` through `mongos` (the manual says `--oplog` does not work there); MongoRescue must replay entries as ordinary writes **and skip `fromMigrate` entries** | `mongorestore --oplogReplay --oplogLimit` per shard, as today, with entries unmodified |
| Per-shard consistency | Cross-shard transaction atomicity is lost in the clone (each shard's applyOps is applied as separate writes); the final state at T is right **(analysis)** | Preserved |
| Fit with AGENTS.md | Matches the safe-clone default | A disposable verification and DR target; in-place replacement stays an explicit administrator opt-in |

Pre-sharding follows the manual: create the collection on the destination and shard it with the source's key before `mongorestore`, and do not use `--drop` ([restore from dumps][mongo-dump-restore]). The key, uniqueness and collation come from the dumped `config.collections`. In-place restore of a whole cluster (stop routers and balancer, replace each shard and the CSRS in sequence, as PBM's tutorial prescribes ([PBM PITR restore][pbm-pitr-restore])) is out of scope until the first two paths are proven; it would be an explicit opt-in and needs its own design.

### What the single-binary model needs

- **Coordination in process.** An errgroup per cluster backup: N dump streams in parallel (bounded by a setting), a barrier that waits for all `T_after_i`, then the oplog range reads. No agents; the HA design ([design/ha.md](ha.md)) already gives the standby takeover, and a cluster backup must be resumable or restartable as a whole.
- **Network reach.** MongoRescue must reach every shard member and CSRS member, not only `mongos`. This is the biggest operational difference from PBM, whose agents sit on the nodes ([PBM overview][pbm-intro]).
- **Credentials per replica set.** Direct connections to shards use shard-local users, which is a deployment concern to document and test **(open question)**.
- **Driver boundary.** Discovery, balancer control, `fsync` and range reads live in `internal/mongoconn`; domain logic (cut computation, planner, restore ordering) in a new package that must not import `server` or `cmd` (AGENTS.md).
- **Streaming.** Nothing in B needs buffering: each archive and oplog range flows through the existing pipe pipeline.

## Phased plan

| Phase | Scope | Effort | Result |
| :--- | :--- | :--- | :--- |
| **0** | Detect `mongos`, refuse by default, `allow_mongos` with warning, readiness reason. **Done in #222.** | done | No silent inconsistent backup |
| **0.5 (optional)** | Quiesced mode (option A): balancer pause plus `fsyncLock` through `mongos` for a guarded `mongodump`, always unlocking, with a prominent warning and a hard timeout. Only if users ask for an interim. | M | Consistent dumps for small clusters that can tolerate a write pause |
| **S0** | Spikes (below): oplog replay of a shard to a common T under live migrations and transactions; `mongodump --oplog` against a shard member; per-shard credentials; noop writes on idle shards. | M | Evidence for phase 1 and a go or no-go |
| **1** | Cluster connection and topology discovery; cluster backup with N children and a common cut T; balancer pause; cluster-level verification; API, CLI, MCP and dashboard read-only views; readiness. | L to XL | Consistent logical cluster backups restorable to T |
| **2** | PITR across shards: one collector stream per replica set grouped as a cluster stream, restorable range as the intersection of chains, lag-skew and idle-shard alerts, retention on the group. | L | Restore to any T inside the common range |
| **3** | Restore choreography: preflight (topology, versions, free space), safe clone in the same cluster (pre-sharding, `fromMigrate` filter, replay as writes), restore into an empty cluster of the same topology, chain tests. In-place later, opt-in. | XL | Verified restores |
| **4 (separate decision)** | In-place full-cluster restore; cross-topology restore (different shard count); physical backups. | XL each | Only on demand |

The total matches roadmap 3.4's XL: roughly four to six engineer-months for phases 1 to 3 including test infrastructure, assuming the spikes pass. This is an estimate, not a commitment.

## Risks

- **Correctness of the common-cut argument (analysis).** The claim that per-shard states at one cluster timestamp, plus the CSRS at the same timestamp, form a consistent cluster is the heart of option B and of PBM's design as read from its source, but we have not verified the migration and transaction corner cases ourselves. Mitigation: spike S0 with invariant-checking workloads before any code.
- **Oplog window.** All shards must keep the oplog from the earliest `T_before` to T; slow shards or huge dumps overrun it. Mitigation: the existing headroom check, per shard.
- **Backup wall time.** The cut waits for the slowest shard; the oplog range grows with it.
- **Network and credential model.** Direct member access may be impossible in locked-down environments (Atlas-like or Kubernetes services that only expose `mongos`). Then B is not available and the answer is "use the provider's backups".
- **Version drift.** Resharding, `moveCollection`, `fromMigrate` semantics and time series on sharded clusters change between 5.0 and 9.0. PBM rejects sharded time series collections in selective logical backups ([PBM `logical.go`][pbm-logical]). Mitigation: a version matrix and refusal of unknown cases.
- **Restore complexity.** Replay through `mongos` is a new engine path that applies oplog entries as writes; a bug there silently corrupts a clone. Mitigation: it only ever writes clone namespaces, plus the invariant tests.
- **Cost of the test matrix.** A sharded cluster in CI is heavy; see below.
- **Scope creep.** Users will ask for physical backups and in-place cluster restore next; both are out of this plan.

## Open questions for the maintainer

1. Is requiring direct network access and per-shard credentials acceptable, or must `mongos` alone suffice (which restricts us to option A)?
2. Do we ship the quiesced interim (phase 0.5), or go straight to the common-cut design?
3. Default for the balancer during a cluster dump: `pause` (recommended) or `leave`, once the spike supports it?
4. Is the clone-in-the-same-cluster restore (pre-sharded, replayed as writes) the right meaning of "safe clone" for sharded targets, with a disposable empty cluster for verification and DR?
5. Minimum versions: 6.0 and later only, so fsync via `mongos` and modern oplog formats are available?
6. How are shard-local users and cluster users to be handled: one credential per member group, or a single cluster user mirrored to every shard?
7. Is `$backupCursor` really Enterprise-only? (Confirm in the MongoDB manual; it only affects whether option D is ever interesting.)
8. Should the CSRS and `admin` data (users, roles) be part of a cluster backup by default? Today `admin`, `config` and `local` are never restored.
9. Retention and storage layout: should a cluster backup be one retention unit (recommended) or per child?

## Test strategy

- **Docker-compose sharded cluster for CI.** A compose file (`scripts/sharded/`, next to `scripts/replset/`) with a 3-member CSRS, two or three shards (single-member for the PR tier, 3-member for nightly), and one `mongos`, initialised by a script that creates the replica sets, adds shards, enables sharding on a test database and shards a hashed and a ranged collection. A `make test-sharded-docker` target, modelled on `test-pitr-replset`, runs it as a nightly job rather than on every pull request; the existing replica-set PITR job is the template ([design/pitr.md](pitr.md#testing)).
- **Hermetic unit tests (no network or binaries):** common-cut computation, restorable-range intersection, topology parsing of `config.shards`, the `fromMigrate` filter in `internal/oplog` (fuzzed, like the namespace filter), restore planner ordering and the balancer guard (always restored).
- **Invariant workloads during backup, the heart of the suite:**
  - a "bank transfer" workload with cross-shard transactions whose total balance is constant;
  - an insert-only workload with unique sequence numbers (no gaps, no duplicates after restore);
  - a `moveChunk` loop running during the dump and the oplog read, with the balancer on;
  - DDL (`createIndex`) during the window; `renameCollection` and resharding must make the backup fail cleanly.
- **Assertions after restore to T:** invariants hold, every document exists exactly once per its chunk owner, restored counts equal the counts recorded at T, and chunk metadata matches the data.
- **Failure injection:** kill a shard primary during the dump (a failed child fails the parent, nothing is half-recorded), kill MongoRescue mid-cluster-backup (balancer restored on start), drop a shard's oplog window.
- **Spikes (S0), each with a written result in this document:** (1) replay each shard to T under live migrations and verify invariants; (2) confirm `mongodump --oplog` works against a shard member directly and what it does with `fromMigrate`; (3) shard-local users and `config.shards` host names; (4) oplog advancement on an idle shard (noop writes) so that T is reachable; (5) transaction handling across shards at T; (6) restoring the CSRS and rewriting `config.shards`.
- **Versions:** at least the lowest supported (6.0) and the newest (8.x, 9.0 when available), as for PITR.

## Follow-up issues (proposed, not created)

Listed in the issue as a checklist: spikes S0; cluster connection and discovery; cluster backup with a common cut; balancer guard; cluster verification; sharded test harness; PITR group streams; sharded safe-clone restore; empty-cluster restore; documentation and readiness.

[mongodump]: https://www.mongodb.com/docs/database-tools/mongodump/
[mongo-dump-backup]: https://www.mongodb.com/docs/manual/tutorial/backup-sharded-cluster-with-database-dumps/
[mongo-dump-restore]: https://www.mongodb.com/docs/manual/tutorial/restore-sharded-cluster-with-database-dumps/
[fsync]: https://www.mongodb.com/docs/manual/reference/command/fsync/
[opsm-overview]: https://www.mongodb.com/docs/ops-manager/current/core/backup-overview/
[psmdb-cursor]: https://docs.percona.com/percona-server-for-mongodb/backup-cursor.html
[psmdb-extend-blog]: https://www.percona.com/blog/experimental-feature-backupcursorextend-in-percona-server-for-mongodb/
[pbm-intro]: https://docs.percona.com/percona-backup-mongodb/intro.html
[pbm-comparison]: https://docs.percona.com/percona-backup-mongodb/features/comparison.html
[pbm-blog]: https://www.percona.com/blog/using-percona-backup-for-mongodb-in-replica-set-and-sharding-environment-part-one/
[pbm-logical]: https://github.com/percona/percona-backup-mongodb/blob/main/pbm/backup/logical.go
[pbm-pitr]: https://docs.percona.com/percona-backup-mongodb/features/point-in-time-recovery.html
[pbm-pitr-restore]: https://docs.percona.com/percona-backup-mongodb/usage/pitr-tutorial.html
[server-88167]: https://jira.mongodb.org/browse/SERVER-88167
