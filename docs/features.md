# Features

The full catalogue of what MongoRescue does. For who it is for and how it compares with other tools, see [Why MongoRescue](comparison.md).

## Backup and restore

- Manage many MongoDB servers from one instance: connections are tested, their databases and collections listed, and backups can be restored into another server
- Scheduled and on-demand backups with retention by age or count, a dry-run preview of what retention deletes, pins (legal hold) and a retention history
- Jobs that back up one database, a list of them, all databases of a connection or those matching glob patterns such as `prod_*`, each database into its own backup, with a live preview, optional automatic inclusion of new databases, collections chosen per database in a tree (Selected and All) and one notification per run ([jobs with several databases](api.md#jobs-with-several-databases), [collection filters per database](api.md#collection-filters-per-database))
- Several storage targets, managed in the dashboard: local disk and S3-compatible buckets (AWS S3, MinIO, Cloudflare R2, Backblaze B2, DigitalOcean Spaces, Wasabi); every backup remembers its target
- Safe-clone restores by default; in-place restores require explicit confirmation and are checksum-verified first
- A preflight before every restore (target reachable, server versions, clone name, privileges, free disk space, the collections an in-place restore replaces) that refuses a restore bound to fail unless forced, and an optional comparison of the restored database with the backup's manifest ([api](api.md#restore-preflight))
- Restore the whole database or only selected collections, picked from a list read from the backup's archive header

## Evidence that backups restore

- Evidence that backups restore: every archive is re-read and checksum-verified after upload, an optional integrity sweep re-verifies them at rest, automated restore tests restore a job's latest backup into a temporary database and compare collection counts and indexes, and weekly storage scans report orphan and missing archives ([verification](verification.md))
- Disaster recovery for MongoRescue itself: scheduled, encrypted snapshots of its metadata database to a storage target, and a passphrase-sealed recovery kit with `secret.key`, the age keys and the storage credentials ([runbook](production.md#restore-mongorescue-from-a-snapshot))
- A recovery point objective per job (by default twice the longest gap between runs plus an hour) that alerts once when missed, and a *Recovery readiness* view per database: last good, verified and restore-tested backups, RPO, estimated RTO and escrowed keys ([production](production.md#rpo-and-rto))

## Security and protection

- Delete protection: soft deletes with a grace period and undo, delayed lowering of retention and the grace period, storage targets locked while in use, and an optional two-person rule for destructive actions
- Optional [age](https://age-encryption.org) encryption, so the bucket only ever stores ciphertext
- User accounts with sessions (listed and revocable one by one) and CSRF protection, scoped API keys (read, operator, admin) for automation, and a first-run setup with a one-time code
- [Single sign-on](sso.md) through an OpenID Connect provider (Entra ID, Google, Okta, Keycloak) with group-to-role mappings, a domain filter and a local administrator as the break-glass way in
- A hash-chained [audit log](audit.md) of every action (who, what, target, result, source address) with chain verification, retention, JSON Lines export and webhook forwarding
- Connection strings and notification secrets are encrypted at rest, never logged and never returned by the API

## Operations and integrations

- Backup and restore history filtered and paginated on the server (status, database, trigger, date range, search), in the dashboard and the API
- A dashboard overview of the last 30 days (success rate, storage growth), the next 24 hours of scheduled runs and what needs attention; a command palette (<kbd>Ctrl</kbd>/<kbd>⌘</kbd>+<kbd>K</kbd>), keyboard shortcuts (<kbd>?</kbd> lists them) and shareable links to any backup, job or restore (`#/backups/<id>`)
- Notifications on success or failure via webhook, Telegram, email or SMS, with simple routing rules
- Prometheus metrics, including the time of the last successful backup per job, and [alert rules](https://github.com/YigitCittan/mongorescue/blob/main/deploy/prometheus/alerts.yml) tested with `promtool`
- Gentle on production: backups [read from a secondary](production.md#reading-from-a-secondary) (read preference and tag sets per connection or job), cap their upload rate, limit concurrent backups per connection and start only within a [backup window](production.md#backup-windows)
- Optional [active-passive high availability](production.md#high-availability): a standby instance takes over scheduled backups and PITR collection about 40 seconds after the active one dies, with its metadata handed over through encrypted snapshots, a lease with an epoch as fencing token so two instances never work at once, and missed schedules caught up once
- An outbound [heartbeat](monitoring.md) to a dead-man's switch such as healthchecks.io, globally and per job (`/start`, success, `/fail`), so a crashed instance or a hung scheduler raises an alert too
- Storage usage and cost: the stored size per storage target, job and database over time (backups, copies, retained copies and PITR chunks), an estimated monthly cost from a price you enter on the target, and the estimated egress of verification, restore tests and copies, in the dashboard, the API, MCP and a `mongorescue_storage_stored_bytes` gauge; estimates from MongoRescue's own records, no billing API ([cost](cost.md))
- An MCP server for AI assistants (Streamable HTTP and stdio) with read-only and safe-clone tools, per-key rate limits and an audit log
- A [command line](cli.md) for scripts and CI (`mongorescue backup`, `restore`, `list`, `verify`, `status`) with `--wait`, JSON output and stable exit codes
- One static binary for Linux, macOS and Windows, plus a multi-arch container image


## Using the REST API, the CLI and MCP

- [REST API](api.md): everything the dashboard does, with scoped API keys (`read`, `operator`, `admin`).
- [Command line](cli.md): `mongorescue backup`, `restore`, `list`, `verify` and `status` against a running instance, with `--wait`, JSON output and stable exit codes.
- [MCP server](mcp.md): lets AI assistants check backups and rehearse restores into safe clones; they can never delete or restore in place.

## Known limitations

As of v0.28.0. Run through the [production checklist](production.md#production-checklist) before you rely on MongoRescue.

- **One working instance at a time** ([#64](https://github.com/YigitCittan/mongorescue/issues/64), [#65](https://github.com/YigitCittan/mongorescue/issues/65)). Jobs, history, users and settings live in an embedded SQLite database (`mongorescue.db`) whose data directory is locked by one process. [Active-passive high availability](production.md#high-availability) adds a standby (it serves no data and accepts no sign-in) that takes over when the active instance dies, but there is no active-active mode and no shared metadata store: the standby receives the metadata through snapshots, so changes made in the last snapshot interval before a crash can be lost. Without HA, scheduled metadata snapshots and the recovery kit make a rebuild possible, and the [heartbeat](monitoring.md) tells you when the instance is down.
- **`secret.key` is the root of trust.** Stored credentials and encryption keys are sealed with one key: losing it makes them unrecoverable, and whoever reads it together with `mongorescue.db` holds every stored credential. Connection and S3 credentials can instead be [read from HashiCorp Vault](security.md#credentials-from-hashicorp-vault) when used ([#62](https://github.com/YigitCittan/mongorescue/issues/62)); notification secrets, TLS client keys and encryption keys stay sealed. It can be [wrapped by a KMS](security.md#wrapping-secretkey-with-a-kms) (AWS KMS, Google Cloud KMS, Azure Key Vault, Vault Transit), so that the data directory holds no plaintext key, but a running instance keeps it in memory, and deleting the KMS key loses it too. Keep the recovery kit offline. Keys can be [rotated](encryption.md#key-rotation-runbook); a key set by `MONGORESCUE_SECRET_KEY` is rotated by hand.
- **Sharded clusters are not supported** ([#58](https://github.com/YigitCittan/mongorescue/issues/58)). Dumps through `mongos` are not tested and not consistent across shards, so backups and restores through a connection that reaches a `mongos` are refused (`sharded clusters are not supported`) unless the connection sets `allow_mongos`; with it, backups carry a `sharded_unsupported` warning, shown in Recovery readiness too. Point-in-time recovery needs a replica set and refuses `mongos` always. See [production.md](production.md#sharded-clusters-mongos).
- **Point-in-time recovery: replica sets only, in place with downtime.** Point-in-time recovery (a supported feature since [#141](https://github.com/YigitCittan/mongorescue/issues/141)) restores replica sets only, never sharded clusters, into new safe-clone databases; replacing the original databases ([#142](https://github.com/YigitCittan/mongorescue/issues/142)) is an administrator's opt-in that restores into clones first, takes a safety backup and needs applications to stop writing until it completes ([procedure](pitr.md#replacing-the-original-databases-in-place)). The oplog replays on one thread, so the restore time grows with the oplog after the base ([measured rates](pitr.md#restore-time-rto)). See [docs/pitr.md](pitr.md#not-there-yet).
- **No independent security audit** ([#144](https://github.com/YigitCittan/mongorescue/issues/144)). The threat model is [docs/threat-model.md](threat-model.md); there has been no third-party review or pentest yet.
- **Single sign-on** sessions do not follow the identity provider's (no refresh tokens, no back-channel logout), and the desktop app has no single sign-on ([docs/sso.md](sso.md)).
- **Platforms.** The macOS app is not signed or notarized yet, a [1.0 criterion](versioning.md#what-10-means) ([desktop](desktop.md#install)). Windows binaries are unit-tested in CI, but the integration tests (real MongoDB, S3 emulators) run on Linux only. The standalone server binary needs the [MongoDB Database Tools](configuration.md#mongodb-database-tools) on the host; the Docker image and the desktop app include them.

The full list of production gaps is tracked with the [production-gap](https://github.com/YigitCittan/mongorescue/issues?q=is%3Aissue+is%3Aopen+label%3Aproduction-gap) label.
