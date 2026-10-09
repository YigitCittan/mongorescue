# Testing

MongoRescue exists to give back the data it was given. This page lists what the test suites prove, how to run them, and what they do not cover.

## Test layers

| Layer | Where | Runs | Guarantees |
| :--- | :--- | :--- | :--- |
| Unit | `*_test.go` next to the code | every push and pull request, with `-race`, on Linux, macOS (Go 1.26 and 1.27) and Windows; 60% coverage gate | Engines, storage drivers, auth, scheduler, API and MCP behave as specified with fakes. Hermetic: no network, no external binaries. The [trust features](verification.md) are covered there too: post-upload verification ok and mismatch, the sweep order, restore tests (success, count mismatch, missing privileges, an existing temporary database, two concurrent tests of one database dropping only their own temporary database, and the temporary database dropped after a failure, a cancellation and a panic, never the source), parallel imports of one key, the revival of a pruned record's archive, shared archives kept on delete and retention, the last verified backup re-checked when pruning, the manifest capture, a retention preview that equals the actual deletion, pins respected by retention and deletion, storage drift (orphans, import, missing) and the route scope matrix. |
| Security | unit tests in `internal/auth`, `internal/server`, `internal/notify`, `internal/redact`, `internal/storage`, `internal/secretbox`; every integration test | every push and pull request | Credentials never reach logs, errors, records or API responses; path traversal is refused; sessions, CSRF, scopes and login throttling hold; secrets are sealed at rest. The integration tests use a random MongoDB password and fail if it appears anywhere. |
| Fuzz | `Fuzz*` targets, `make fuzz` | nightly, `make fuzz FUZZTIME=5m` (the job fails if the target is missing) | Parsers of untrusted input do not panic or misbehave on arbitrary bytes. |
| Integration | `internal/integration`, `integration` build tag | every push and pull request, MongoDB 5.0, 6.0, 7.0 and 8.0, an 8.0 replica set, MinIO and LocalStack | Real `mongodump`/`mongorestore` against a real server and real S3 implementations; see below. |
| Cloud | the same suites against AWS S3, R2, B2, Spaces, Wasabi | pushes to `main` (maintainer secrets) | Storage conformance, round trips, the HTTP API, fidelity and corruption detection on real providers. |
| PITR replica set | `internal/replset`, `replset3` build tag, `make test-pitr-replset` | nightly, 02:30 UTC (workflow "PITR", not required): MongoDB 5.0 and 8.0, Database Tools 100.12.2 and 100.19.1 | On a three-member replica set: failovers during collection and during a restore, a rollback through network isolation and divergence after a forced reconfiguration keep the oplog chain unbroken (or end it exactly where the surviving history ends), lose and duplicate no entry, and never store a rolled-back write. See [below](#pitr-on-a-three-member-replica-set). |
| PITR soak | `TestSoak`, build tags `replset3` and `soak`, `make test-pitr-soak` | weekly, Saturday 01:00 UTC, 5.5 hours (workflow "PITR", not required); 7 days by hand with `scripts/soak-pitr.sh` | Hours of collection under a write load: no gap and no false break, a bounded number of chunk objects and bases, retention deleting and purging what falls out of the window, and a passing chain test. See [below](#pitr-soak-test). |
| Large data | `TestThroughputAndMemory` with `MONGORESCUE_TEST_LARGE=1` | nightly, 03:00 UTC, on every MongoDB version | About 2 GiB streams through backup and restore with peak process memory below 256 MiB; throughput is reported. |
| Fault injection | `internal/chaos`, `chaos` build tag, `make test-chaos-docker` | nightly, job "Chaos" (not required, not on pull requests) | A storage outage or partition, a MongoDB connection drop, a primary stepdown (during dumps and PITR collection), a full disk, SIGKILL (backup, purge, migration, key rotation) and clock steps never leave a broken run `completed` or an object that looks complete; the next run succeeds and the alert fires; see [below](#fault-injection-suite). |
| Load | `internal/load`, `load` build tag, `make test-load-docker` | weekly, job "Load" (not required) | 5 GiB, 500 connections and 10,000 scheduled jobs: scheduler tick latency, memory, API p95, SQLite contention, backup throughput and the impact of a dump on a busy primary stay within the committed baseline; see [below](#load-test). |
| Single sign-on | Unit: `internal/auth/oidc` against the fake provider `oidctest`, `internal/auth`, `internal/server`. Integration: `TestKeycloakSingleSignOn` | unit tests on every push and pull request; Keycloak in the job "Integration (Keycloak SSO)" on pushes to `main` and nightly (not required) | Forged, replayed, misdirected and stale tokens and states are refused (the full list is in [design/oidc.md](design/oidc.md#tests)); a real Keycloak sign-in through its login form maps groups to roles, applies the domain filter and logs out at the provider, without tokens in logs or the audit log. |

## What the integration suite proves

| Test | Guarantee |
| :--- | :--- |
| `TestRestoreFidelity` | A backup restores faithfully. The source database holds every BSON type (Decimal128, dates before 1970 and after 3000, binary subtypes, ObjectId, regex, code with scope, timestamps, min/max keys, deprecated types, NaN and -0.0), nested arrays, unicode and dotted keys, 10,500 documents in one collection and documents of 15 MiB; compound, unique, partial, TTL, text, 2dsphere, collation and hidden indexes; a JSON schema validator with `validationLevel`/`validationAction` and a document that predates it, a capped collection with `size`/`max`, a collection default collation, a time-series collection with a secondary index and views. After a backup and a restore into the safe clone, the clone matches the source: the collection list and options (view definitions included), the normalised index specifications, and per collection the document count and a SHA-256 of the canonical extended JSON of every document in `_id` order. This runs with gzip on and off, age encryption on and off, a backup that includes several collections, one collection, excluded collections and a restore of selected collections. The source is unchanged afterwards. |
| `TestCorruptedBackupsFailLoudly` | A damaged artifact never restores silently, on local disk and every S3 provider: a changed byte in the archive metadata (which `mongorestore` accepts) fails with *backup checksum mismatch* from the checksum computed while streaming and the partial clone is dropped; the same damage restored in place (with `drop_target`) leaves the target untouched, since in-place restores are always verified first; with verification the same damage stops the restore before anything is written; changed bytes in documents, gzip and age data, truncations, a missing object, the wrong key and a missing key all fail with a failed record and a reason. |
| `TestRestoreNeverTouchesExistingDataWithoutOptIn` | Restores go into `<db>_rescue_<timestamp>_<id>` by default and never write to the source. Naming an existing database, `safe_clone: false` or `drop_target` without `confirm_in_place` is refused and writes nothing. A confirmed in-place restore over existing data without `drop_target` fails (every document collides) instead of reporting success. |
| `TestInterruptedBackupsLeaveNothingBehind` | A backup cancelled mid-dump, a `mongodump` killed with SIGKILL mid-stream (plain and encrypted) and an S3 upload that fails mid-way (a proxy rejects parts) each end with a failed record carrying the reason, no checksum and no size, and leave no object, no temporary file and no incomplete multipart upload. Retention keeps the last good backup although newer runs failed. |
| `TestConcurrentRuns` | A second backup of a database that is being backed up is refused with `ErrBusy` (`409`), and so is a second restore into a target that is being restored. A backup and a restore of the same database run side by side and both succeed. |
| `TestSelectiveRestore` | The archive prelude of a real `mongodump` (plain and gzip) lists every collection and view. Restoring one of three collections into a safe clone creates only that collection. An in-place restore of one collection with `drop_target` drops and restores that collection and keeps the target's other collections. |
| `TestUsersAndRolesAreNotBackedUp` | Pins the default: without `include_users_and_roles`, users and roles defined on a database are neither in the archive nor in the restored clone. |
| `TestUsersAndRolesSurviveInPlaceRestore` | A backup with `include_users_and_roles` restores a dropped user and role in place with `restore_users_and_roles`; a safe clone refuses the option. |
| `TestRestoreWithReadWriteUser` | A user with only `readWrite` can back up and restore; a document a validator would reject fails the restore with a hint instead of disappearing. |
| `TestPostBackupVerification` | A real backup re-reads its archive after the upload (`verification: "ok"`) and captures a manifest with the document count and the indexes of every collection; a damaged copy of the archive is reported as a *mismatch* by an on-demand verification. Local disk and every S3 provider. |
| `TestRestoreTestEndToEnd` | The automated restore test restores a real backup into `<db>_rescue_verify_<timestamp>_<random>`, matches it against the manifest, drops the temporary database and leaves the source untouched; a manifest that expects other counts is reported as a mismatch, and a read-only user is refused with a privileges error before anything is created. |
| `TestPatternJobOverRealDatabases` | A `pattern` job over three real databases backs up each into its own verified backup of one run. After a fourth matching database is created, the next run backs it up and records it as known when `auto_include_new` is on; when it is off, the run backs up only the three and reports the fourth as new. |
| `TestOplogSyntheticArchiveReplay`, `TestOplogFilterReplaysIntoSafeClone` | Replica set only (groundwork for point-in-time recovery). `mongorestore --archive --oplogReplay` replays a synthetic oplog-only archive from `internal/oplog` on stdin, with and without `--oplogLimit`, and an empty one. After a base backup (`mongodump --oplog`), writes, renames (one after an index build), index builds, a drop, a transaction across two databases, a rename from another database, views, a time-series collection, `convertToCapped`, `cloneCollectionAsCapped`, `$out` over an existing collection, a rename with `dropTarget` and a 20 MB transaction split into `partialTxn` entries whose first entry is in the other database, the oplog filtered and renamed to `<db>_rescue` and replayed onto the restored base gives a clone equal to the source (documents, indexes, views); the source and the other database are untouched, the filter stops at the limit, and its operation count equals mongorestore's "applied N". |
| `TestOrphanImportEndToEnd` | An archive whose record is lost is found by a storage scan, imported with its original ID, size and SHA-256, restores correctly and is no orphan afterwards. |
| `TestThroughputAndMemory` | Reports MB/s and the peak memory of the MongoRescue process for backup and restore, plain and encrypted, on every target, and checks the clone with `dbHash`. The nightly run fails above 256 MiB. |
| `TestOplogWindowAndRangeReads` | Replica set only. The oplog window has the replica set name and ID, its oldest entry is not after its newest, and the majority optime has a term. Two consecutive range reads up to the majority optime return exactly the inserted entries in order, with no gap or duplicate, and match one read of the whole range; an inclusive read also returns the start entry; a start that no longer exists or has another term fails with `ErrOplogGap` instead of returning a range that starts later; a range past the member's newest entry fails with `ErrOplogBehind`; the entry at the end of a range has the majority term. |
| `TestCanReadOplog` | Replica set with access control only. `CanReadOplog` is true for a user with `read` on `local` and false for `readAnyDatabase`, and agrees with the server: the first can read the oplog, the second is refused, and errors never carry the password. |
| `TestTLSWithCustomCA` | A MongoDB with `--tlsMode requireTLS`, its certificate signed by a CA generated in Go (`internal/integration/gencerts`), is backed up and restored into a safe clone by the real tools with the CA as `tls_ca_pem`; the driver's connection test succeeds with the CA and fails without it, and a backup without it does not complete. The tools' private TLS directory is gone afterwards and no secret reaches records or logs. |
| `TestX509ClientCertificate` | The same server, as an `$external` user named after a client certificate (`MONGODB-X509`) whose key is encrypted PKCS#8, its password passed through the tools' `--config` file: backup and restore into a safe clone, then the connection service end to end (save with the key sealed and masked, connection test, database listing); without the client certificate the x509 URI cannot authenticate. |
| `TestBackupRestoreRoundTrip`, `TestServerEndToEnd`, `TestMCPStdioBridgeEndToEnd`, `TestStorageConformance` | Round trips through the engines, the full HTTP API in-process, the MCP stdio bridge against the real binary, and the storage contract every driver must pass. |

## Running the tests

```bash
make test-race                  # unit tests
make test-integration-docker    # integration suite against disposable containers
make test-chaos-docker          # fault-injection suite (MongoDB, MinIO, Toxiproxy)
make test-load-docker           # load test (a replica set with a secondary, MinIO)
```

`make test-integration-docker` needs Docker, curl, Go and the MongoDB Database Tools (100.3 or newer) on `PATH`. It starts MongoDB with a random root password, MinIO, LocalStack and Keycloak (dev mode, with the test realm of `internal/integration/testdata/keycloak-realm.json`) on random loopback ports and removes them afterwards. Knobs:

| Variable | Default | Purpose |
| :--- | :--- | :--- |
| `MONGO_IMAGE` | `mongo:7` | Server image, e.g. `mongo:5.0` or `mongo:8.0` |
| `MONGO_TOPOLOGY` | `standalone` | `replset` starts a single-node replica set (keyfile, `rs.initiate`, `directConnection=true`) |
| `IT_PROVIDERS` | `minio localstack keycloak` | Services to start: the S3 emulators and Keycloak for the single sign-on test (`none` for local disk only) |
| `IT_RACE` | `1` | `0` runs without the race detector |
| `GOTESTFLAGS` | | Extra `go test` flags, e.g. `-run TestRestoreFidelity -v` |
| `MONGORESCUE_TEST_LARGE` | | `1` runs the large-data test with about 2 GiB |
| `MONGORESCUE_TEST_LARGE_MB` | 64, or 2048 with `LARGE=1` | Size of the generated data in MiB |

For example, the nightly job for MongoDB 8.0 as a replica set is:

```bash
MONGO_IMAGE=mongo:8.0 MONGO_TOPOLOGY=replset IT_PROVIDERS=minio IT_RACE=0 \
  MONGORESCUE_TEST_LARGE=1 GOTESTFLAGS="-timeout 100m" make test-integration-docker
```

To run against services you manage, set `MONGORESCUE_TEST_MONGO_URI` and the `MONGORESCUE_TEST_S3_*` variables described in [CONTRIBUTING.md](../CONTRIBUTING.md#test-commands-and-variables) and run `make test-integration`. Tests whose services are not configured are skipped.

## Fault-injection suite

`internal/chaos` (build tag `chaos`) runs the real binary against MongoDB and MinIO behind [Toxiproxy](https://github.com/Shopify/toxiproxy) and breaks things in the middle of an operation: it cuts, slows down or black-holes connections, steps down the primary, fills the disk, sends SIGKILL and steps the system clock. It runs nightly in the job "Chaos" (not on pull requests, not required) and on demand (`workflow_dispatch` with `suite: chaos`); the test output and the server log of every process it started are uploaded as the `chaos-report` artifact.

Every scenario checks what applies of: a broken run ends `failed` (or `cancelled`), never `completed`, with the reason and without a checksum or size; no object that looks complete is left under its key; the next run succeeds (a backup, and a restore of it); and the alert fires (`backup.failed`, `restore.failed` or `security.key_rotated` through a webhook). MongoDB passwords never appear in errors.

| Scenario | Fault | Expected outcome |
| :--- | :--- | :--- |
| `TestStorageOutageMidUpload` | MinIO cut off for 10 s in the middle of a multipart upload (connections closed, new ones refused), longer than the S3 client's retries | The backup fails; no object; the multipart upload is aborted once the storage answers (30-second abort window); `backup.failed`; the next backup completes and restores. |
| `TestStoragePartitionLongerThanTimeout` | MinIO partitioned (connections kept open, no byte through in either direction) for longer than `storage_stall_timeout` (1 minute) | The backup fails with *no upload progress* within the stall timeout plus the 30-second abort window instead of hanging until `backup_timeout`; no object; `backup.failed`; the next backup completes. |
| `TestMongoDropDuringDump` | Every connection to MongoDB closed in the middle of a dump, new ones refused for 5 s | The backup fails; no object, no incomplete upload; `backup.failed`; the next backup completes. |
| `TestMongoDropDuringRestore` | The same during a restore into a safe clone: 5 s, then 45 s with `restore_timeout` at 30 s | A restore never completes with missing documents: across 5 s `mongorestore` waits for the server and completes with every document (checked); across 45 s it fails on the timeout, `restore.failed` fires, the source is untouched and the partial clone is dropped (or named by the failed record). The next restore completes. |
| `TestPrimaryStepdownDuringDump` | `replSetStepDown` (forced, 10 s) in the middle of a dump | Either the backup fails cleanly (as above) or it completes with an archive that restores to exactly the source's documents; never a completed backup with partial data. The next backup completes. |
| `TestPrimaryStepdownDuringPITR` | A stepdown while a PITR stream collects 15-second chunks and a writer inserts a document every 50 ms, retrying through the election | One chain (no chain break, no `pitr.chain_broken`), contiguous chunks (each starts where the previous one ended), and a point-in-time restore to just after the last write holds exactly the documents written: no gap, no duplicate. |
| `TestFullDiskOnStorageTarget` | The data directory and the local target on a 48 MiB filesystem; a 96 MiB backup fills it mid-write | The backup fails with *no space left on device*; no archive and no temporary file left; `backup.failed`; the metadata database passes `PRAGMA integrity_check`; the next backup completes. |
| `TestFullDiskOnDataDir` | The data directory's filesystem completely full while backups to S3 run | The server keeps answering and does not crash; no run reports a success the metadata does not record, none stays in progress; integrity check; recovery once space is freed. With the disk full, new backups are refused with `503` (the free-space check; the chaos processes use `MONGORESCUE_MIN_FREE_SPACE_MB=1` so the 48 MiB filesystem is usable), and a backup whose final record cannot be saved is reported as failed with its archive kept ([#152](https://github.com/YigitCittan/mongorescue/issues/152)). |
| `TestKillDuringBackup` | SIGKILL of the server mid-upload, then a restart | The record is failed as *interrupted*, without checksum or size; no object; `mongodump` exits with the server; `backup.failed` fires after the restart; integrity check; the next backup completes. The killed multipart upload cannot be aborted by a dead process (see the lifecycle rule in [production.md](production.md#least-privilege-storage-credentials)). |
| `TestKillDuringPurge` | Ten backups deleted (soft, as retention deletes), the purge that removes their archives killed after three, each S3 request slowed by a second | Integrity check; purged records have no object, deleted ones keep theirs except at most the one being purged at the kill, which the next purge finishes; the kept backup is untouched and restores; a second purge leaves exactly the kept backup's object. The purge runs every ten minutes in the server; the scenario runs the same code (`scheduler.PurgeDeleted`) in a helper process on the stopped server's data directory, so it can be killed without waiting for the schedule. |
| `TestKillDuringMigration` | A metadata database at schema 12 (built from the repository's own migrations, a plaintext connection URI and 20,000 backups), upgraded by the current binary and killed at points over its start-up, then finely where the migrations run | Integrity check; the next start applies exactly the missing migrations (none half-applied); every old backup is listed; the connection's credentials are sealed (no plaintext left in any file) and connect; a backup and a restore work afterwards. At least one kill must land between two migrations. |
| `TestKillDuringKeyRotation` | SIGKILL at seven points of a `secret.key` rotation request | Every start settles the rotation (rolled back or completed), every stored credential (connection, S3 target) still works, integrity check, a later rotation succeeds. Every committed rotation is announced (`security.key_rotated`), also when the kill came right after it: deliveries are stored in the outbox ([#150](https://github.com/YigitCittan/mongorescue/issues/150)). |
| `TestClockStepForward` | The system clock stepped an hour forward while a job runs every minute (Linux, `MONGORESCUE_CHAOS_CLOCK=1`) | Health stays 200 for three minutes; the missed hour is not replayed (one to four runs in three minutes); runs keep succeeding; no `job.rpo_missed` (RPO two hours). |
| `TestClockStepBack` | The clock stepped an hour back | Health stays 200; no slot runs twice (the next slot is an hour away on the stepped clock, as with cron); no `job.rpo_missed`. |

The replica set is a single node, in CI and locally: the stepdown scenarios see a primary that steps down, has no primary for ten seconds and is elected again, which is the failover a dump or a collector sees, without a second member to continue on. The clock scenarios step the real system clock (`date -s`, as root or with passwordless `sudo`), because Go reads the wall clock from the kernel and `libfaketime` cannot fake it; they run only when `MONGORESCUE_CHAOS_CLOCK=1` on a disposable Linux machine, which the nightly job is (it stops time synchronisation for the run).

```bash
make test-chaos-docker                                     # every scenario
GOTESTFLAGS="-v -run TestKillDuringBackup" make test-chaos-docker   # one scenario
```

`scripts/test-chaos-docker.sh` needs Docker, curl, Go and the MongoDB Database Tools (100.12 or newer). It starts MongoDB (`MONGO_IMAGE`, default `mongo:8.0`) as a single-node replica set, MinIO and Toxiproxy (pinned by digest) on random loopback ports, creates a small filesystem for the full-disk scenarios (a tmpfs with passwordless `sudo` on Linux, a RAM disk on macOS; `CHAOS_SMALL_FS_MB`, default 48; the scenarios are skipped without it) and writes the reports to `CHAOS_REPORT_DIR` (default `./chaos-report`).

## Load test

`internal/load` (build tag `load`) runs the real binary at scale and measures what an operator feels: scheduler tick latency, memory, dashboard API latencies, SQLite contention under concurrent backups, backup throughput and the impact of a full dump on a busy primary. It runs weekly (Sunday 04:00 UTC, job "Load", not required) and on demand (`workflow_dispatch` with `suite: load`); the JSON report and the server log are uploaded as the `load-report` artifact and the metrics go to the job summary.

1. **Data.** A database of `MONGORESCUE_LOAD_DATA_MB` MiB (5120 by default, 51200 for the issue's 50 GB) of incompressible 256 KiB documents, written by eight parallel writers (reruns against the same server reuse it), and `MONGORESCUE_LOAD_CONCURRENT` databases of 16 MiB.
2. **Scale.** `MONGORESCUE_LOAD_CONNECTIONS` connections (500) with `max_concurrent_backups` 4 and `MONGORESCUE_LOAD_JOBS` scheduled jobs (10,000), created through the API by 16 clients at once; the jobs are daily at hours away from the run, so they are registered with the scheduler but do not fire.
3. **Idle at scale.** For 100 seconds: every scheduler tick (from `/api/v1/health`), and the latency of the dashboard's calls (jobs with `limit=50`, all jobs, backup page, connections, overview, history, readiness, health), 50 sequential requests each and 200 from 16 clients at once.
4. **Primary impact.** A probe inserts a document and reads it back on the primary every 5 ms: 20 s idle, then during a full backup of the large database with the default read preference (this is also the throughput measurement), during a backup through a connection to the secondary with `read_preference: secondary` (60 s), and during a job throttled to `max_upload_mbps: 80` with `num_parallel_collections: 1` (60 s). The load suite runs a primary and a secondary for this.
5. **Concurrent backups.** One backup of each small database started at once (four at a time on the connection); the wall time, the API p95 during them and the number of `SQLITE_BUSY` / *database is locked* errors in the log (which must stay zero).

The report has every measurement; `metrics` holds the flat values the baseline checks. `internal/load/testdata/baseline.json` has one profile per scale (`MONGORESCUE_LOAD_PROFILE`, `ci` by default): the scale it was recorded at (a run at another scale fails instead of comparing), and per metric the value, which way is better and a tolerance: a lower-is-better metric fails above `value × tolerance + slack`, a higher-is-better one below `value / tolerance`. The tolerances are wide (3× for latencies and throughput, 2× plus 32 MiB for memory) because runners differ; `concurrent.sqlite_busy_errors` allows none and `scheduler.tick_lag_max_s` five seconds. Record or refresh a profile with `MONGORESCUE_LOAD_UPDATE_BASELINE=1` and commit the file.

```bash
make test-load-docker                         # the ci profile: 5 GiB, 500 connections, 10,000 jobs
MONGORESCUE_LOAD_PROFILE=small MONGORESCUE_LOAD_DATA_MB=512 MONGORESCUE_LOAD_JOBS=1000 \
  MONGORESCUE_LOAD_CONNECTIONS=50 MONGORESCUE_LOAD_CONCURRENT=8 make test-load-docker
```

### Load test numbers

The `ci` profile, recorded 2026-10-09 on a laptop (Apple silicon, 8 CPUs, Docker Desktop with MongoDB 8.0 primary and secondary, 1 GiB WiredTiger cache each, and MinIO in one VM); this run is the committed `ci` baseline:

| Measurement | Value |
| :--- | :--- |
| Scale | 5,120 MiB, 500 connections, 10,000 jobs, 20 concurrent backups |
| Creating jobs / connections through the API (16 clients) | 1,902 jobs/s, 6,408 connections/s |
| Scheduler tick lag beyond 30 s (worst, idle at scale) | 0.1 s |
| Memory idle at scale: RSS / Go heap in use (peak) | 539 / 463 MiB |
| Memory during the large backups | 291 / 258 MiB |
| Memory during the concurrent backups | 414 / 305 MiB |
| API p95: Jobs with `limit=50` (ignored), alone / 16 clients | 80.9 / 1,487.2 ms |
| API p95: All jobs, alone / 16 clients | 84.1 / 1,337.6 ms |
| API p95: Backup page, alone / 16 clients | 0.2 / 2.5 ms |
| API p95: Connections, alone / 16 clients | 7.1 / 40.1 ms |
| API p95: Overview (`/stats`), alone / 16 clients | 83.5 / 1,371.3 ms |
| API p95: Overview history, alone / 16 clients | 1,454.3 / 6,443.4 ms |
| API p95: Readiness, alone / 16 clients | 207.2 / 1,710.6 ms |
| API p95: Health, alone / 16 clients | 0.1 / 0.2 ms |
| Backup throughput (no gzip, to MinIO) | 63.7 MB/s (5,369 MB) |
| Concurrent backups: wall time / API p95 meanwhile / SQLite busy errors | 13.6 s / 156.7 ms / 0 |
| Primary insert+read p95, idle | 5.7 ms (1.00× idle, 106 ops/s) |
| Primary insert+read p95, during a dump, read preference primary | 17.4 ms (3.06× idle, 80 ops/s) |
| Primary insert+read p95, during a dump from the secondary | 19.7 ms (3.46× idle, 75 ops/s) |
| Primary insert+read p95, during a throttled dump (80 Mbit/s, one collection at a time) | 7.1 ms (1.24× idle, 112 ops/s) |

The `small` profile on the same laptop, a run checked against the committed `small` baseline (no regression):

| Measurement | Value |
| :--- | :--- |
| Scale | 512 MiB, 50 connections, 1,000 jobs, 8 concurrent backups |
| Creating jobs / connections through the API (16 clients) | 2,241 jobs/s, 5,350 connections/s |
| Scheduler tick lag beyond 30 s (worst, idle at scale) | 0.0 s |
| Memory idle at scale: RSS / Go heap in use (peak) | 102 / 61 MiB |
| Memory during the large backups | 180 / 120 MiB |
| Memory during the concurrent backups | 242 / 103 MiB |
| API p95: Jobs with `limit=50` (ignored), alone / 16 clients | 7.5 / 121.8 ms |
| API p95: All jobs, alone / 16 clients | 7.4 / 127.0 ms |
| API p95: Backup page, alone / 16 clients | 0.2 / 2.4 ms |
| API p95: Connections, alone / 16 clients | 0.8 / 5.6 ms |
| API p95: Overview (`/stats`), alone / 16 clients | 6.0 / 134.9 ms |
| API p95: Overview history, alone / 16 clients | 138.2 / 678.9 ms |
| API p95: Readiness, alone / 16 clients | 19.3 / 151.5 ms |
| API p95: Health, alone / 16 clients | 0.1 / 0.2 ms |
| Backup throughput (no gzip, to MinIO) | 89.0 MB/s (537 MB) |
| Concurrent backups: wall time / API p95 meanwhile / SQLite busy errors | 3.0 s / 16.1 ms / 0 |
| Primary insert+read p95, idle | 4.9 ms (1.00× idle, 112 ops/s) |
| Primary insert+read p95, during a dump, read preference primary | 10.5 ms (2.15× idle, 97 ops/s) |
| Primary insert+read p95, during a dump from the secondary | 12.4 ms (2.56× idle, 93 ops/s) |
| Primary insert+read p95, during a throttled dump (80 Mbit/s, one collection at a time) | 6.0 ms (1.24× idle, 117 ops/s) |

What the numbers say:

- **The scheduler holds 10,000 jobs** without a late tick (0.1 s worst), and 20 concurrent backups produced no SQLite busy error.
- **The job list is not paged**: `GET /api/v1/jobs` ignores `limit`, so `limit=50` costs as much as the whole list (81 against 84 ms at 10,000 jobs, 1.5 s with 16 clients). The overview (`/stats`) and readiness grow with the number of jobs in the same way, and the overview history is the slowest call (1.5 s alone, 6.4 s with 16 clients). Every other call stays in single-digit milliseconds. The idle memory peak (539 MiB RSS) comes from 16 clients reading the 10,000 jobs at once; the large backup itself peaked at 291 MiB.
- **Primary impact.** A full dump at about 64 MB/s triples the p95 of the probe on the primary; throttling to 80 Mbit/s with one collection at a time keeps it at 1.24×. Reading from the secondary did not help here because both members share one machine's CPU and disk (the secondary even replays the probe's writes); on separate hosts it takes the dump's reads off the primary.
- The weekly job runs the `ci` profile on a GitHub-hosted runner, which is slower than the laptop; the tolerances absorb that, and its report (artifact `load-report`) is where the baseline is refreshed from.

## PITR on a three-member replica set

`make test-pitr-replset` (`scripts/test-replset-docker.sh`) needs only Docker (with compose v2) and Go. For every scenario it starts a fresh replica set from [scripts/replset/compose.yml](../scripts/replset/compose.yml): `m1`, `m2` and `m3` on one network, with a keyfile and a random root password, initiated with `rs.initiate`. The tests inject failures with the docker CLI (kill, network disconnect), so they run in a container on that network with the docker socket mounted ([runner.Dockerfile](../scripts/replset/runner.Dockerfile): Ubuntu, the Database Tools and the docker CLI); the test binary is built on the host. Each test runs a real collector with one-second chunks, the backup and restore engines and the operations service in the test process.

| Test | Failure | Asserts |
| :--- | :--- | :--- |
| `TestFailoverDuringCollection` | The primary steps down, then the new primary is killed, while a writer inserts with majority write concern | One open chain, no superseded chunk, no `pitr.chain_broken` or `pitr.diverged`; the chunks hold exactly the final primary's oplog (no entry lost, added or duplicated, same terms), and every acknowledged insert exactly once. |
| `TestFailoverDuringRestore` | The primary is killed while a point-in-time restore restores the base, and again while it replays the oplog | The restore completes with clones identical to the target state, or fails with a message and no unrecorded clone; a retry restores the target state exactly; the chain survives. |
| `TestRollbackThroughNetworkIsolation` | The primary is cut off the network, takes 50 `w:1` writes, the others elect a new primary and take more writes, then it rejoins and rolls back | The rolled-back writes appear in no chunk; the chain is unbroken and equals the new primary's oplog. |
| `TestDivergenceAfterForcedReconfig` | A secondary is cut off, the other two commit and the collector stores 50 writes, both die, and the cut-off member is forced into a one-member set | The chain ends where the survivor's history ends (`diverged`, one `pitr.diverged`), the chunks with the lost writes are superseded, the new chain continues from that point, both together equal the survivor's oplog, and a point-in-time restore from the new chain's base matches the survivor's data. |

| Variable | Default | Purpose |
| :--- | :--- | :--- |
| `MONGO_IMAGE` | `mongo:8.0` | Server image of the three members |
| `TOOLS_VERSION` | `100.12.2` | Database Tools in the runner |
| `RS_RUN` | all | Regular expression of the tests to run |
| `RS_LOG_DIR` | | Where the member logs of failed tests go (otherwise their last lines are printed) |
| `RS_OPLOG_MB` | `1024` | Oplog size of each member |

## PITR soak test

`make test-pitr-soak` (`RS_SUITE=soak`) runs `TestSoak` on the same replica set: the collector with base backups on a schedule and retention (by count only, `MONGORESCUE_SOAK_KEEP_BASES`; the 14-day rule is off), under a steady load of inserts and deletes (the data stays bounded, the oplog does not), for `MONGORESCUE_SOAK_DURATION`. Every `MONGORESCUE_SOAK_SAMPLE` it checks that the collector is running without an error and no more than max(5 min, 5 × the interval) behind, that the stream still has its one chain without a superseded chunk or a break event, and that the chunk objects in storage and the live bases stay within what retention keeps. At the end it applies retention, checks that every remaining chunk object belongs to a live chunk and that no live chunk ends before the oldest kept base's `t_before`, compares the stored entries with the primary's oplog where both still have them, and runs a chain test.

| Variable | Default | Purpose |
| :--- | :--- | :--- |
| `MONGORESCUE_SOAK_DURATION` | `10m` | How long the collector runs under load |
| `MONGORESCUE_SOAK_CHUNK_SECONDS` | `15` | Chunk interval |
| `MONGORESCUE_SOAK_BASE_EVERY` | a fifth of the duration, at least `2m` | Base interval (a cron schedule in whole minutes or hours) |
| `MONGORESCUE_SOAK_KEEP_BASES` | `2` | Bases retention keeps |
| `MONGORESCUE_SOAK_RATE` | `200` | Writes per second |
| `MONGORESCUE_SOAK_SAMPLE` | `30s` | How often the invariants are checked |
| `MONGORESCUE_SOAK_REPORT` | | A file for the JSON report (samples, largest object count and lag, chain test rates) |

The weekly CI run collects for 5.5 hours, the most a GitHub-hosted job allows with the set-up and the final checks. **The 7-day run is manual:** on a machine with Docker that stays up for a week, run

```bash
nohup ./scripts/soak-pitr.sh > soak.log 2>&1 &
```

It collects for 168 hours with 60-second chunks, a base every 6 hours, 4 bases kept, 400 writes per second and an 8 GiB oplog per member, samples every 5 minutes, and writes `pitr-soak-<date>.json` and, for a failure, the member logs to `pitr-soak-logs/`. Override any `MONGORESCUE_SOAK_*` variable, `SOAK_DURATION`, `MONGO_IMAGE` or `TOOLS_VERSION`. It stops at the first broken invariant with the reason in `soak.log`; a passing run ends with `all 1 test(s) of the soak suite passed` and the report.

## Performance

`TestThroughputAndMemory` generates incompressible 256 KiB documents, backs them up without gzip, restores them into a clone and checks the clone with `dbHash`. It logs throughput and the peak memory of the test process (RSS on Linux, memory held by the Go runtime elsewhere), sampled every 20 ms while each phase runs; in CI the numbers are also written to the job summary. `mongodump` and `mongorestore` run as separate processes and are not included.

Streaming keeps MongoRescue's memory flat: on a laptop (Apple silicon, MongoDB 7, 512 MiB of data) backups ran at 47 to 124 MB/s and restores at 53 to 69 MB/s, with a peak of 15 to 43 MiB whatever the size; the S3 uploader's part buffers (2 × 5 MiB in flight) account for the higher figure. The nightly run enforces the 256 MiB limit with about 2 GiB of data and without the race detector, which inflates memory. Throughput depends on the disk, the network and the server far more than on MongoRescue.

## Known limits

- **The crash window of notifications.** An event reaches the durable outbox through the in-process bus a few milliseconds after its action committed; a kill inside that window still loses it ([notifications.md](notifications.md#delivery)). `TestKillDuringKeyRotation` passes because the window is far shorter than its kill points.
- **Multipart uploads of killed processes** cannot be aborted by the dead process: their parts stay in the bucket until a lifecycle rule removes them ([production.md](production.md#least-privilege-storage-credentials)).
- **Failover is a single-node stepdown.** The stepdown scenarios run against a single-node replica set; a three-member set with an election onto another member is not tested.
- **Users and roles** are not part of backups by default: a backup covers one database and runs `mongodump --db` without `--dumpDbUsersAndRoles` unless the job or backup sets `include_users_and_roles`, and they are restored only in place with `restore_users_and_roles` (see [api.md](api.md#users-and-roles)). Without it, users and roles defined on the database (and everything in `admin`) must be recreated or backed up separately after a disaster.
- **No point-in-time recovery.** Backups are `mongodump` snapshots; the oplog is not captured (`--oplog` needs a full-instance dump), so a restore returns the data as of the backup and writes that happen during a backup of a busy database may be partially included.
- **Sharded clusters** and `mongos` are not tested; the matrix covers standalone servers and a single-node replica set.
- **Restore privileges.** With the `bypassDocumentValidation` privilege (the `restore`, `dbAdmin`, `dbOwner` or `root` role) documents are restored even when a validator would reject them; with only `readWrite` such a document fails the restore.
- **Restore tests compare counts and indexes, not documents.** The manifest records `estimatedDocumentCount` (metadata-based) and index specifications per collection; document contents are covered by the archive checksum, which every restore checks while it streams. Views, `system.*` collections and time-series buckets are not in the manifest.
- **Views on excluded collections** are still backed up (a view is excluded only by its own name) and restore as views on a missing collection.
- **Legacy records** without a stored SHA-256 are restored without the checksum check.
- **Integration tests run on Linux only**; Windows and macOS builds are unit-tested. The dashboard has no browser tests.
- **Cloud providers** run the conformance, round-trip, API, fidelity and corruption suites; interruption tests (which inspect incomplete multipart uploads) run against MinIO and LocalStack only.
- **S3 Object Lock** runs against MinIO only, on a second bucket the tests create with Object Lock enabled (`MONGORESCUE_TEST_S3_MINIO_LOCK_BUCKET`, set by the docker script). A lock cannot be waited out in a test, so the integration tests check the lock headers, that S3 refuses to delete a locked version, legal holds and reads of a version behind a delete marker; the purge once a lock has ended is covered by unit tests with a fixed clock.
