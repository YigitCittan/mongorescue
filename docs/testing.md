# Testing

MongoRescue exists to give back the data it was given. This page lists what the test suites prove, how to run them, and what they do not cover.

## Test layers

| Layer | Where | Runs | Guarantees |
| :--- | :--- | :--- | :--- |
| Unit | `*_test.go` next to the code | every push and pull request, with `-race`, on Linux, macOS (Go 1.26 and 1.27) and Windows; 60% coverage gate | Engines, storage drivers, auth, scheduler, API and MCP behave as specified with fakes. Hermetic: no network, no external binaries. |
| Security | unit tests in `internal/auth`, `internal/server`, `internal/notify`, `internal/redact`, `internal/storage`, `internal/secretbox`; every integration test | every push and pull request | Credentials never reach logs, errors, records or API responses; path traversal is refused; sessions, CSRF, scopes and login throttling hold; secrets are sealed at rest. The integration tests use a random MongoDB password and fail if it appears anywhere. |
| Fuzz | `Fuzz*` targets, `make fuzz` | nightly, 5 minutes per run (once the Makefile has the target) | Parsers of untrusted input do not panic or misbehave on arbitrary bytes. |
| Integration | `internal/integration`, `integration` build tag | every push and pull request, MongoDB 5.0, 6.0, 7.0 and 8.0, an 8.0 replica set, MinIO and LocalStack | Real `mongodump`/`mongorestore` against a real server and real S3 implementations; see below. |
| Cloud | the same suites against AWS S3, R2, B2, Spaces, Wasabi | pushes to `main` (maintainer secrets) | Storage conformance, round trips, the HTTP API, fidelity and corruption detection on real providers. |
| Large data | `TestThroughputAndMemory` with `MONGORESCUE_TEST_LARGE=1` | nightly, 03:00 UTC, on every MongoDB version | About 2 GiB streams through backup and restore with peak process memory below 256 MiB; throughput is reported. |

## What the integration suite proves

| Test | Guarantee |
| :--- | :--- |
| `TestRestoreFidelity` | A backup restores faithfully. The source database holds every BSON type (Decimal128, dates before 1970 and after 3000, binary subtypes, ObjectId, regex, code with scope, timestamps, min/max keys, deprecated types, NaN and -0.0), nested arrays, unicode and dotted keys, 10,500 documents in one collection and documents of 15 MiB; compound, unique, partial, TTL, text, 2dsphere, collation and hidden indexes; a JSON schema validator with `validationLevel`/`validationAction` and a document that predates it, a capped collection with `size`/`max`, a collection default collation, a time-series collection with a secondary index and views. After a backup and a restore into the safe clone, the clone matches the source: the collection list and options (view definitions included), the normalised index specifications, and per collection the document count and a SHA-256 of the canonical extended JSON of every document in `_id` order. This runs with gzip on and off, age encryption on and off, a backup that includes several collections, one collection, excluded collections and a restore of selected collections. The source is unchanged afterwards. |
| `TestCorruptedBackupsFailLoudly` | A damaged artifact never restores silently, on local disk and every S3 provider: a changed byte in the archive metadata (which `mongorestore` accepts) fails with *backup checksum mismatch* from the checksum computed while streaming; with verification the same damage stops the restore before anything is written; changed bytes in documents, gzip and age data, truncations, a missing object, the wrong key and a missing key all fail with a failed record and a reason. |
| `TestRestoreNeverTouchesExistingDataWithoutOptIn` | Restores go into `<db>_rescue_<timestamp>` by default and never write to the source. Naming an existing database, `safe_clone: false` or `drop_target` without `confirm_in_place` is refused and writes nothing. A confirmed in-place restore over existing data without `drop_target` fails (every document collides) instead of reporting success. |
| `TestInterruptedBackupsLeaveNothingBehind` | A backup cancelled mid-dump, a `mongodump` killed with SIGKILL mid-stream (plain and encrypted) and an S3 upload that fails mid-way (a proxy rejects parts) each end with a failed record carrying the reason, no checksum and no size, and leave no object, no temporary file and no incomplete multipart upload. Retention keeps the last good backup although newer runs failed. |
| `TestConcurrentRuns` | A second backup of a database that is being backed up is refused with `ErrBusy` (`409`), and so is a second restore into a target that is being restored. A backup and a restore of the same database run side by side and both succeed. |
| `TestUsersAndRolesAreNotBackedUp` | Pins current behaviour: users and roles defined on a database are neither in the archive nor in the restored clone. |
| `TestRestoreWithReadWriteUser` | A user with only `readWrite` can back up and restore; a document a validator would reject fails the restore with a hint instead of disappearing. |
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

- **Users and roles** are not part of backups: a backup covers one database and runs `mongodump --db` without `--dumpDbUsersAndRoles`, so users and roles defined on the database (and everything in `admin`) must be recreated or backed up separately after a disaster.
- **No point-in-time recovery.** Backups are `mongodump` snapshots; the oplog is not captured (`--oplog` needs a full-instance dump), so a restore returns the data as of the backup and writes that happen during a backup of a busy database may be partially included.
- **Sharded clusters** and `mongos` are not tested; the matrix covers standalone servers and a single-node replica set.
- **Restore privileges.** With the `bypassDocumentValidation` privilege (the `restore`, `dbAdmin`, `dbOwner` or `root` role) documents are restored even when a validator would reject them; with only `readWrite` such a document fails the restore.
- **Safe clones are named by the second.** Two restores of the same database started within one second share `<db>_rescue_<timestamp>`: a concurrent one is refused with `ErrBusy`, a sequential one fails because its documents collide.
- **Views on excluded collections** are still backed up (a view is excluded only by its own name) and restore as views on a missing collection.
- **Legacy records** without a stored SHA-256 are restored without the checksum check.
- **Integration tests run on Linux only**; Windows and macOS builds are unit-tested. The dashboard has no browser tests.
- **Cloud providers** run the conformance, round-trip, API, fidelity and corruption suites; interruption tests (which inspect incomplete multipart uploads) run against MinIO and LocalStack only.
