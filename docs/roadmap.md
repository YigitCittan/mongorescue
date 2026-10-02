# Roadmap

This is the plan for taking MongoRescue from "works well" to "the backup system a team can rely on in production". It is ordered by what matters most when a database has to be recovered: first that every backup can be restored and every recovery key can be recovered, then who may do what, then point-in-time recovery, then storage hardening, and only then running several instances.

Versions follow [versioning.md](versioning.md): every phase ships as one or more minor releases, and 1.0 is cut when phases 1 and 2 are done. Sizes are rough: S is a day, M a few days, L about a week, XL several weeks.

## Where we are (v0.13)

Already in place, so not on this list:

- Streaming backups and restores with checksums, post-upload verification, a scheduled integrity sweep and scheduled restore tests that compare counts and indexes ([verification.md](verification.md)).
- Safe-clone restores by default, restores of selected collections, and restores to another connection.
- Cancelling runs, a log for every run, live progress, pause and resume.
- Jobs over several databases (list, all, pattern) with auto-include of new databases.
- Retention with pins, a preview, and protection of each database's last good and last verified backup.
- Bulk actions with dry runs, API key scopes (read, operator, admin), CSRF, an audit of MCP and bulk actions, and age encryption.
- Versioned metadata migrations that refuse a newer schema.
- Unit, security, fuzz and integration tests on MongoDB 5.0 to 8.0 and a replica set ([testing.md](testing.md)).

## Phase 1: recovery you can prove (v0.14, v0.15)

The goal: after a disaster, nothing MongoRescue needs to restore is missing, and the dashboard shows how ready each database is to be recovered.

| # | Item | Size | Notes |
| :--- | :--- | :--- | :--- |
| 1.1 | **Self-backup of MongoRescue's metadata** | M | Scheduled, encrypted online snapshot of `mongorescue.db` (SQLite backup API) to a storage target, with retention and a "restore MongoRescue from this snapshot" path. Shipped in v0.14.0 (Settings → Recovery, see [production.md](production.md)). |
| 1.2 | **Recovery kit** | M | One guided export of everything needed to recover elsewhere: `secret.key`, age identities or passphrase hints, storage target settings and the latest metadata snapshot location. It is sealed with a passphrase the admin chooses, and the dashboard reminds the admin until a kit has been downloaded. Shipped in v0.14.0 (`POST /api/v1/recovery-kit`, see [production.md](production.md)). |
| 1.3 | **Users and roles in backups** | S–M | A per-job option that runs `mongodump --dumpDbUsersAndRoles` and restores them with `--restoreDbUsersAndRoles`, opt-in and never applied to a safe clone by default. Shipped in v0.14.0 (`include_users_and_roles` on jobs and backups, `restore_users_and_roles` for in-place restores, see [api.md](api.md#users-and-roles)). |
| 1.4 | **Restore preflight** | M | Before a restore starts, check the server version against the version the archive was dumped from, whether the target exists, the user's privileges, the archive size against free space where it can be known, and the collections that would be replaced. Show a single go/no-go summary in the restore dialog and the API. |
| 1.5 | **Restore verification** | M | After every restore, optionally compare counts and indexes against the backup's manifest (the code restore tests already use) and record the result on the restore. A full document diff stays a test-only tool. |
| 1.6 | **RPO and RTO** | M | A per-job recovery point objective ("a good backup every 24 h") with alerts when it is missed; the recovery time measured by restore tests; a **Recovery readiness** view per database: last good backup, last verified, last restore test, RPO met, estimated RTO, keys escrowed. |
| 1.7 | **Audit log for every action** | M–L | Record dashboard and API actions, not only MCP and bulk ones: who, what, target, result and source IP. The log is append-only and hash-chained so tampering is detectable. It gets a retention setting, export to JSON Lines, and an optional forward to syslog or a webhook. |
| 1.8 | **Browser tests in CI** | M | A Playwright suite against a real server: sign in, create a connection, back up, restore, bulk delete with dry run, update banner. The ad-hoc headless checks we run today become this suite. |
| 1.9 | **Migration checksums** | S | Record each applied migration's checksum and refuse to start if an applied migration was changed. Shipped in v0.14.0. |

## Phase 2: who may do what (v0.16, v0.17)

| # | Item | Size | Notes |
| :--- | :--- | :--- | :--- |
| 2.1 | **Roles for users** | L | Viewer, operator and admin for dashboard users, mirroring the API key scopes, enforced in `internal/auth`. A later step adds per-connection access, so an operator can be limited to some servers. Needs a migration and a review of every route in the scope table. |
| 2.2 | **Single sign-on (OIDC)** | L | Sign in with an OIDC provider (Entra ID, Google, Okta, Keycloak), map groups to roles, and keep local accounts as a break-glass option. Depends on 2.1. |
| 2.3 | **CLI** | M | `mongorescue backup`, `restore`, `list`, `verify` and `status`, talking to a running instance through the REST API with an API key, for scripts and CI. |

**1.0** is cut after phases 1 and 2, together with the criteria in [versioning.md](versioning.md#what-10-means).

## Phase 3: point-in-time recovery (1.x)

This changes the backup model, so it starts with a design document that has to be agreed before any code.

| # | Item | Size | Notes |
| :--- | :--- | :--- | :--- |
| 3.1 | **PITR design** | M | Point-in-time recovery needs a replica set and works per server, not per database: a base backup of the whole instance taken with `--oplog`, plus a continuous oplog stream. Open questions: how this fits per-database jobs, where oplog chunks are stored and for how long, encryption and verification of chunks, what happens when the oplog window is lost, and support for sharded clusters (out of scope at first). |
| 3.2 | **Oplog collector** | L | A long-running tailer of `local.oplog.rs` that resumes from its last position, writes compressed and encrypted chunks to the storage target, verifies them, and alerts when the gap to the primary grows or the oplog window is about to be overrun. |
| 3.3 | **PITR restore** | XL | Restore to a chosen time: the nearest base backup, then the oplog chunks replayed with `mongorestore --oplogReplay --oplogLimit`, into a safe clone by default, with the same preflight and verification as other restores. |
| 3.4 | **Sharded clusters** | XL | Research first: consistent backups across shards need cluster-wide coordination, which is a separate project. |

## Phase 4: storage you can't lose (1.x)

| # | Item | Size | Notes |
| :--- | :--- | :--- | :--- |
| 4.1 | **Immutable backups** | M | S3 Object Lock in governance or compliance mode with a retention period per target. Retention and deletes respect the lock and say why something can't be deleted yet. |
| 4.2 | **Copy to a second target** | M–L | After a backup is verified, copy it to another target (another region or provider), verify the copy, and track both copies. Restores fall back to the copy when the primary is unavailable. |
| 4.3 | **KMS** | M–L | Keep `secret.key` and age identities wrapped by AWS KMS, Google Cloud KMS or Azure Key Vault (envelope encryption), so the key material on disk is useless on its own. |
| 4.4 | **HashiCorp Vault** | M–L | Read connection credentials and storage keys from Vault instead of storing them, with lease renewal. |
| 4.5 | **Cost tracking** | S–M | Stored size per job, database and target over time, an estimated monthly cost per target from a price you enter, and egress used by verification and copies. |

## Phase 5: more than one instance (2.x, on demand)

| # | Item | Size | Notes |
| :--- | :--- | :--- | :--- |
| 5.1 | **Shared metadata store** | XL | A PostgreSQL backend next to SQLite, so several instances can share state. Today the data directory lock enforces one instance ([architecture.md](architecture.md)). |
| 5.2 | **High availability** | XL | Leader election for the scheduler and the oplog collector on top of 5.1, so a standby takes over. |

These change deployment fundamentally and are only worth doing when users need them; until then, phase 1's self-backup and recovery kit cover the "MongoRescue itself is gone" case.

## How this list changes

Each phase is broken into issues before it starts. Items move between phases when real use shows a different priority, and the changelog records what shipped. Large or architectural items (2.1, 2.2, phase 3 and phase 5) start with a short design note that is agreed before implementation, as [AGENTS.md](../AGENTS.md) asks.
