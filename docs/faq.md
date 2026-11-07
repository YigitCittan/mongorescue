# Frequently asked questions

Short answers, with a link to the page that has the whole story. Terms in *italics* are defined in the [glossary](glossary.md). Something missing? Look at [troubleshooting](troubleshooting.md) and the [incident runbooks](runbooks.md), or [open an issue](https://github.com/YigitCittan/mongorescue/issues).

## Scope and fit

### Who is MongoRescue for, and who is it not for?

For teams running one or a few standalone servers or replica sets who want scheduled, verified, optionally encrypted *logical* backups (`mongodump` archives) and a restore they can rehearse. It is not for sharded clusters, for multi-terabyte deployments that need physical or snapshot backups, or for point-in-time recovery of anything but a replica set. [How it compares](comparison.md) with `mongodump` + cron, Percona Backup for MongoDB, Atlas Backup, Ops Manager and restic/Velero is honest about this.

### Which MongoDB versions and deployments work?

MongoDB 5.0, 6.0, 7.0 and 8.0, as a standalone server or a replica set; the CI runs all four versions both ways. [Point-in-time recovery](pitr.md) needs a replica set. The [support matrix](https://github.com/YigitCittan/mongorescue/blob/main/README.md#support-matrix) lists the Database Tools versions and storage providers.

### Does it support sharded clusters?

No ([#58](https://github.com/YigitCittan/mongorescue/issues/58)). A `mongodump` through a `mongos` is not consistent across shards: the balancer and chunk migrations can leave documents missing or duplicated in an archive that still passes its checksum. MongoRescue therefore refuses to back up through a `mongos`, refuses restore preflights into one, and refuses point-in-time streams on one. A connection can opt in with `allow_mongos` and gets a warning on every backup; only use it with writes stopped and the balancer off. Details: [sharded clusters](production.md#sharded-clusters-mongos).

### Does it work with MongoDB Atlas?

Yes for the logical backups: add the `mongodb+srv://` string from *Connect → Drivers*, allow the MongoRescue host in the project's IP access list, and give the database user the [roles](connections.md#privileges) it needs. It is not tested in CI against real Atlas; a [manual checklist](connections.md#atlas-manual-test-checklist) exists. Atlas's own snapshots and its point-in-time restore are independent of MongoRescue, and many teams keep both. See [Atlas](connections.md#mongodb-atlas).

### Is it a replacement for replication or high availability of MongoDB?

No. A replica set protects against a lost node; a backup protects against a dropped collection, a bad deploy, ransomware and a deleted cluster, all of which replicate instantly. Keep both.

## Restores

### Why does a restore go into a safe clone instead of the original database?

Because the most dangerous moment in a backup tool is the restore: you are stressed, the data is half broken, and an "overwrite" that is a little wrong makes it worse. A restore therefore creates `<db>_rescue_<timestamp>_<id>` next to the original and never touches existing data. You compare, copy back what you need, or rename. Overwriting is possible and deliberate: `--in-place --confirm` in the [CLI](cli.md#restore), or the explicit option in the dashboard and API (it needs an admin key). See the [safe clone](glossary.md#safe-clone) definition and [restore safety](api.md#restore-preflight).

### How do I restore without the dashboard?

Three ways, in order of convenience:

1. **The CLI** from any machine that can reach the server: `mongorescue restore BACKUP_ID --wait --verify-restore` ([walkthrough](getting-started-cli.md#6-restore-into-a-safe-clone), [reference](cli.md#restore)).
2. **The REST API** (`POST /api/v1/restore`) with an API key ([API](api.md)).
3. **Without MongoRescue at all**: the archive is an ordinary `mongodump` archive, so `mongorestore` reads it. A local-disk archive is under `<backups>/<database>/<yyyy>/<mm>/<backup id>.archive.gz` (S3: the same key). Plain archives: `mongorestore --uri=... --gzip --archive=FILE --nsFrom='shop.*' --nsTo='shop_manual.*'` (the `--nsFrom/--nsTo` pair restores into a new database name, like the safe clone). Encrypted archives (keys ending in `.age`) are an age file around that archive: decrypt first, for example `age -d -i identity.txt FILE.age | mongorestore --gzip --archive ...` with the private key you escrowed. This is why the tool does not lock you in.

### What do I do if MongoRescue itself is gone?

Your archives are still in storage and are standard `mongodump` files (above). To bring MongoRescue back with its jobs, history, users and settings you need the **recovery kit** and a metadata snapshot: [restore MongoRescue from a snapshot](production.md#restore-mongorescue-from-a-snapshot). Without a snapshot you can start a fresh instance with the kit's `secret.key`, recreate the storage targets from the kit and import the archives with a [storage scan](verification.md#storage-scans).

### Can I restore a single collection, or to a point in time?

A single or several collections: `--collections a,b` ([CLI](cli.md#restore)). To a point in time (to the second, before an accidental `dropDatabase`): [point-in-time recovery](pitr.md), replica sets only, and it needs backup encryption turned on because the oplog contains deleted data too.

### How do I know a backup will actually restore?

A checksum proves the archive is intact, not that it restores. MongoRescue verifies after every upload, can re-verify on a schedule, and runs [automated restore tests](verification.md#automated-restore-tests) into a temporary database. Rehearse a real restore as well: the [walkthrough](getting-started-cli.md) takes five minutes.

## Keys, secrets and encryption

### Where does `secret.key` live, and what happens if I lose it?

In the data directory (`/data` in Docker, `./data` or `/var/lib/mongorescue/data` for a binary), as `secret.key` with mode `0600`; or in your secret manager if you set `MONGORESCUE_SECRET_KEY`; or wrapped by a KMS as `secret.key.wrapped`. It encrypts every stored credential (connection URIs, S3 keys, notification secrets, the age identity). Lose it and those credentials are unrecoverable and MongoRescue refuses to start against the database. The backup archives themselves are not affected. Keep a copy apart from your backups: download the [recovery kit](production.md#recovery-kit) and store it offline. [Data directory](production.md#data-directory), [key wrapping](security.md#wrapping-secretkey-with-a-kms).

### Are my backups encrypted?

Credentials stored by MongoRescue always are. The backup archives are encrypted only when you turn on [age encryption](encryption.md) (recommended whenever they leave your own disk; required for point-in-time recovery). An age key that is lost means lost backups: *escrow* it ([key management](encryption.md#key-management-and-loss)).

### What should I back up for MongoRescue itself?

The data directory (or turn on [metadata backups](production.md#metadata-backups), which write an encrypted snapshot of the database to a storage target), and the recovery kit. Losing the database does not lose the archives, but it loses the records that point at them, your schedules and your users.

## Running it

### Windows: what is different?

- **Easiest:** the [desktop app](desktop.md), which bundles the server and the Database Tools and keeps running in the tray when the window is closed. Installer and portable zip; Scoop and winget packages exist too ([install options](https://github.com/YigitCittan/mongorescue/blob/main/README.md#install-options)).
- **Server binary:** `mongorescue.exe -dashboard -data-dir <folder>`. The default data directory is `.\data` relative to the current folder, so always set it. `mongodump.exe` and `mongorestore.exe` are looked up in `<install dir>\tools`, then on `PATH` ([order](configuration.md#mongodb-database-tools)). Windows releases are signed.
- **CLI:** `mongorescue.exe status` and friends work from PowerShell against any server ([walkthrough, PowerShell tabs](getting-started-cli.md)). The CLI does not check key-file permissions on Windows; keep the key under `%APPDATA%`.
- **Temporary credentials:** the short-lived config files that pass connection strings to the tools inherit the folder's ACL; the default `%TEMP%` is private to your user ([details](connections.md#temporary-credentials-on-disk)).
- **Docker Desktop** works with the same compose files as Linux. Prefer named volumes: bind mounts must be writable by the container's UID 10001 ([container images](production.md#container-images)).

### Does the Docker image run as root?

No: UID/GID 10001, with a read-only root filesystem, no capabilities and `no-new-privileges` in the provided compose files. Named volumes get the right ownership; for bind mounts, create the directories owned by 10001 first.

### Do I need a reverse proxy?

Yes before anyone but you reaches it: MongoRescue serves plain HTTP. Put a TLS-terminating proxy in front, publish MongoRescue's port only to the proxy, and set **Trusted proxies**, or every client shares one login throttle and the session cookie is not `Secure`. [Caddy recipe](deployment-recipes.md#caddy-with-automatic-https), [TLS and the reverse proxy](production.md#tls-and-the-reverse-proxy).

### Everybody got locked out of the login. Why?

Almost always a proxy without **Trusted proxies**: MongoRescue sees every request coming from the proxy's address, so five bad passwords from one person lock the proxy's address for all. Set `security.trusted_proxies` to the proxy's address or network ([the recipe explains why](deployment-recipes.md#trusted-proxies-the-step-that-is-easy-to-forget)).

### Is high availability a substitute for backups?

No, and the reverse. HA ([active-passive](design/ha.md)) keeps *MongoRescue* running when a node dies, so scheduled backups and PITR collection do not stop; it is a second instance sharing a storage target, not a copy of your data. Backups protect your *MongoDB data*. A restart policy is enough for most single-instance setups; add a standby when a missed backup window would break your *RPO*. [High availability](production.md#high-availability).

### How do I get told when backups stop working?

Three layers: [notifications](notifications.md) for failed backups, a [heartbeat](monitoring.md#global-heartbeat) to an external service that alerts when MongoRescue goes silent (a dead process cannot report its own death), and [Prometheus metrics and alert rules](monitoring.md#prometheus-alert-rules) with a [runbook](runbooks.md) per alert.

### Can the CLI or an AI assistant do everything the dashboard does?

The dashboard, the [CLI](cli.md), the [REST API](api.md) and the [MCP server](mcp.md) go through the same API and the same permissions. The CLI covers backup, restore, list, verify and status; MCP cannot delete anything or restore in place by design.

## The documentation

### Is the documentation available in other languages?

The documentation is English only. The dashboard itself speaks eight languages (English, Turkish, German, Spanish, French, Chinese, Japanese and Russian). See the [translation policy](https://github.com/YigitCittan/mongorescue/blob/main/CONTRIBUTING.md#translations).

### Where do I start?

[The documentation index](index.md) groups every page by what you are trying to do. New here: the [Docker quick start](index.md#quick-start-with-docker) or the [five-minute CLI walkthrough](getting-started-cli.md).
