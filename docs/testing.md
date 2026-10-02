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
| Large data | `TestThroughputAndMemory` with `MONGORESCUE_TEST_LARGE=1` | nightly, 03:00 UTC, on every MongoDB version | About 2 GiB streams through backup and restore with peak process memory below 256 MiB; throughput is reported. |

## What the integration suite proves

| Test | Guarantee |
| :--- | :--- |
| `TestRestoreFidelity` | A backup restores faithfully. The source database holds every BSON type (Decimal128, dates before 1970 and after 3000, binary subtypes, ObjectId, regex, code with scope, timestamps, min/max keys, deprecated types, NaN and -0.0), nested arrays, unicode and dotted keys, 10,500 documents in one collection and documents of 15 MiB; compound, unique, partial, TTL, text, 2dsphere, collation and hidden indexes; a JSON schema validator with `validationLevel`/`validationAction` and a document that predates it, a capped collection with `size`/`max`, a collection default collation, a time-series collection with a secondary index and views. After a backup and a restore into the safe clone, the clone matches the source: the collection list and options (view definitions included), the normalised index specifications, and per collection the document count and a SHA-256 of the canonical extended JSON of every document in `_id` order. This runs with gzip on and off, age encryption on and off, a backup that includes several collections, one collection, excluded collections and a restore of selected collections. The source is unchanged afterwards. |
| `TestCorruptedBackupsFailLoudly` | A damaged artifact never restores silently, on local disk and every S3 provider: a changed byte in the archive metadata (which `mongorestore` accepts) fails with *backup checksum mismatch* from the checksum computed while streaming and the partial clone is dropped; the same damage restored in place (with `drop_target`) leaves the target untouched, since in-place restores are always verified first; with verification the same damage stops the restore before anything is written; changed bytes in documents, gzip and age data, truncations, a missing object, the wrong key and a missing key all fail with a failed record and a reason. |
| `TestRestoreNeverTouchesExistingDataWithoutOptIn` | Restores go into `<db>_rescue_<timestamp>` by default and never write to the source. Naming an existing database, `safe_clone: false` or `drop_target` without `confirm_in_place` is refused and writes nothing. A confirmed in-place restore over existing data without `drop_target` fails (every document collides) instead of reporting success. |
| `TestInterruptedBackupsLeaveNothingBehind` | A backup cancelled mid-dump, a `mongodump` killed with SIGKILL mid-stream (plain and encrypted) and an S3 upload that fails mid-way (a proxy rejects parts) each end with a failed record carrying the reason, no checksum and no size, and leave no object, no temporary file and no incomplete multipart upload. Retention keeps the last good backup although newer runs failed. |
| `TestConcurrentRuns` | A second backup of a database that is being backed up is refused with `ErrBusy` (`409`), and so is a second restore into a target that is being restored. A backup and a restore of the same database run side by side and both succeed. |
| `TestSelectiveRestore` | The archive prelude of a real `mongodump` (plain and gzip) lists every collection and view. Restoring one of three collections into a safe clone creates only that collection. An in-place restore of one collection with `drop_target` drops and restores that collection and keeps the target's other collections. |
| `TestUsersAndRolesAreNotBackedUp` | Pins the default: without `include_users_and_roles`, users and roles defined on a database are neither in the archive nor in the restored clone. |
| `TestUsersAndRolesSurviveInPlaceRestore` | A backup with `include_users_and_roles` restores a dropped user and role in place with `restore_users_and_roles`; a safe clone refuses the option. |
| `TestRestoreWithReadWriteUser` | A user with only `readWrite` can back up and restore; a document a validator would reject fails the restore with a hint instead of disappearing. |
| `TestPostBackupVerification` | A real backup re-reads its archive after the upload (`verification: "ok"`) and captures a manifest with the document count and the indexes of every collection; a damaged copy of the archive is reported as a *mismatch* by an on-demand verification. Local disk and every S3 provider. |
| `TestRestoreTestEndToEnd` | The automated restore test restores a real backup into `<db>_rescue_verify_<timestamp>_<random>`, matches it against the manifest, drops the temporary database and leaves the source untouched; a manifest that expects other counts is reported as a mismatch, and a read-only user is refused with a privileges error before anything is created. |
| `TestPatternJobOverRealDatabases` | A `pattern` job over three real databases backs up each into its own verified backup of one run. After a fourth matching database is created, the next run backs it up and records it as known when `auto_include_new` is on; when it is off, the run backs up only the three and reports the fourth as new. |
| `TestOrphanImportEndToEnd` | An archive whose record is lost is found by a storage scan, imported with its original ID, size and SHA-256, restores correctly and is no orphan afterwards. |
| `TestThroughputAndMemory` | Reports MB/s and the peak memory of the MongoRescue process for backup and restore, plain and encrypted, on every target, and checks the clone with `dbHash`. The nightly run fails above 256 MiB. |
| `TestBackupRestoreRoundTrip`, `TestServerEndToEnd`, `TestMCPStdioBridgeEndToEnd`, `TestStorageConformance` | Round trips through the engines, the full HTTP API in-process, the MCP stdio bridge against the real binary, and the storage contract every driver must pass. |

## Running the tests

```bash
make test-race                  # unit tests
make test-integration-docker    # integration suite against disposable containers
```

`make test-integration-docker` needs Docker, curl, Go and the MongoDB Database Tools (100.3 or newer) on `PATH`. It starts MongoDB with a random root password, MinIO and LocalStack on random loopback ports and removes them afterwards. Knobs:

| Variable | Default | Purpose |
| :--- | :--- | :--- |
| `MONGO_IMAGE` | `mongo:7` | Server image, e.g. `mongo:5.0` or `mongo:8.0` |
| `MONGO_TOPOLOGY` | `standalone` | `replset` starts a single-node replica set (keyfile, `rs.initiate`, `directConnection=true`) |
| `IT_PROVIDERS` | `minio localstack` | S3 emulators to start (`none` for local disk only) |
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

## Performance

`TestThroughputAndMemory` generates incompressible 256 KiB documents, backs them up without gzip, restores them into a clone and checks the clone with `dbHash`. It logs throughput and the peak memory of the test process (RSS on Linux, memory held by the Go runtime elsewhere), sampled every 20 ms while each phase runs; in CI the numbers are also written to the job summary. `mongodump` and `mongorestore` run as separate processes and are not included.

Streaming keeps MongoRescue's memory flat: on a laptop (Apple silicon, MongoDB 7, 512 MiB of data) backups ran at 47 to 124 MB/s and restores at 53 to 69 MB/s, with a peak of 15 to 43 MiB whatever the size; the S3 uploader's part buffers (2 × 5 MiB in flight) account for the higher figure. The nightly run enforces the 256 MiB limit with about 2 GiB of data and without the race detector, which inflates memory. Throughput depends on the disk, the network and the server far more than on MongoRescue.

## Known limits

- **Users and roles** are not part of backups by default: a backup covers one database and runs `mongodump --db` without `--dumpDbUsersAndRoles` unless the job or backup sets `include_users_and_roles`, and they are restored only in place with `restore_users_and_roles` (see [api.md](api.md#users-and-roles)). Without it, users and roles defined on the database (and everything in `admin`) must be recreated or backed up separately after a disaster.
- **No point-in-time recovery.** Backups are `mongodump` snapshots; the oplog is not captured (`--oplog` needs a full-instance dump), so a restore returns the data as of the backup and writes that happen during a backup of a busy database may be partially included.
- **Sharded clusters** and `mongos` are not tested; the matrix covers standalone servers and a single-node replica set.
- **Restore privileges.** With the `bypassDocumentValidation` privilege (the `restore`, `dbAdmin`, `dbOwner` or `root` role) documents are restored even when a validator would reject them; with only `readWrite` such a document fails the restore.
- **Restore tests compare counts and indexes, not documents.** The manifest records `estimatedDocumentCount` (metadata-based) and index specifications per collection; document contents are covered by the archive checksum, which every restore checks while it streams. Views, `system.*` collections and time-series buckets are not in the manifest.
- **Safe clones are named by the second.** Two restores of the same database started within one second share `<db>_rescue_<timestamp>`: the second is refused (`ErrBusy` while the first runs, `ErrCloneExists` afterwards) and has to be retried.
- **Views on excluded collections** are still backed up (a view is excluded only by its own name) and restore as views on a missing collection.
- **Legacy records** without a stored SHA-256 are restored without the checksum check.
- **Integration tests run on Linux only**; Windows and macOS builds are unit-tested. The dashboard has no browser tests.
- **Cloud providers** run the conformance, round-trip, API, fidelity and corruption suites; interruption tests (which inspect incomplete multipart uploads) run against MinIO and LocalStack only.
