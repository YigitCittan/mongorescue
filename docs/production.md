# Production deployment

MongoRescue holds credentials for your databases and your backup storage, and can overwrite databases on request. Treat the instance like a database administrator account.

## Checklist

- [ ] TLS terminated by a reverse proxy in front of MongoRescue, with **Settings → Security → Trust proxy headers** on (or *Secure cookies: always*) so session cookies are marked `Secure`.
- [ ] Setup completed right after the first start (until then, anyone who can reach the port and read the logs can claim the instance).
- [ ] The HTTP port reachable only from the proxy and your monitoring, not from the internet.
- [ ] One user per person; API keys (not user passwords) for automation, one per consumer, with the smallest scope that works (`read` for dashboards and assistants that only report, `operator` for backup automation).
- [ ] The [MCP endpoint](mcp.md) switched off under Settings → Security if no AI assistant uses it.
- [ ] Backups encrypted with X25519 recipients; private key stored off the backup host.
- [ ] [Metadata backups](#metadata-backups) on (Settings → Recovery), encrypted, to a target other than the one the instance runs on, with a `metadata_backup.failed` notification rule.
- [ ] A [recovery kit](#recovery-kit) downloaded and stored offline, apart from the backups (it holds `secret.key`); a new one downloaded whenever the dashboard asks for it.
- [ ] Container image pinned to a release tag.
- [ ] An alert on stale backups ([metrics.md](metrics.md#alerting)) and a `backup.failed` notification rule ([notifications.md](notifications.md)).
- [ ] An [RPO](#rpo-and-rto) set on every job whose default does not match what you promised, a `job.rpo_missed` notification rule, and no `fail` row under Overview → *Recovery readiness*.
- [ ] Busy replica sets backed up from a secondary, with an upload cap and a backup window where the dump competes with production traffic; databases past the [practical limits of mongodump](#large-databases) covered by snapshots or your cloud provider's backups instead.

## Production checklist

The checklist above secures the instance. This one makes sure you can still restore after losing a database, a bucket, a cloud account or MongoRescue itself. Review it at every minor release, together with the README's [known limitations](../README.md#known-limitations).

- [ ] **Object Lock in compliance mode, with versioning**, on every S3 target that holds backups ([Object Lock](configuration.md#immutable-backups-s3-object-lock), [threat model](security.md#immutable-backups-s3-object-lock)). Why: without it, anyone holding the bucket's credentials can delete every archive, and governance mode can be bypassed.
- [ ] **A separate IAM account and credentials for copy targets** ([least-privilege credentials](#least-privilege-storage-credentials), [copies](configuration.md#copies-on-a-second-target-3-2-1)). Why: one leaked key, or one compromised account, must not reach the primary and its copies at once.
- [ ] **A cross-region copy** of every important job, on its own locked target ([3-2-1 backups](#3-2-1-backups)). Why: a region outage or a lost account takes the primary target with it; restores then fall back to a healthy copy by themselves.
- [ ] **Locked copies required** with **Settings → Security → Require locked copies** (`security.require_locked_copies`) and on every job with copies ([cross-region disaster recovery](#cross-region-disaster-recovery)). Why: a copy target without Object Lock leaves the copy deletable by whoever holds its key, and the policy refuses such a job instead of letting it slip.
- [ ] **A DR drill from the cross-region copy** on every important job, passing at least monthly, and the [region failure runbook](#region-failure) walked through once with the recovery kit stored outside the primary's region ([DR drills](#dr-drills)). Why: a copy you never restored from is a hope, not a plan; the drill proves it restores without region A.
- [ ] **No `dr_same_region` or `dr_same_credentials` warning** under Overview → *Recovery readiness*, and a region set on every storage target ([region awareness](#region-awareness)). Why: a copy in the primary's region, or reachable with the primary's key, fails together with it.
- [ ] **A heartbeat and the alert rules** ([heartbeats](monitoring.md#global-heartbeat), [Prometheus alert rules](monitoring.md#prometheus-alert-rules)). Why: a crashed instance or a hung scheduler cannot report its own failure.
- [ ] **The recovery kit downloaded and stored offline**, apart from the backups, with metadata backups on ([recovery kit](#recovery-kit), [metadata backups](#metadata-backups)). Why: without `secret.key` the stored credentials and encryption keys are unrecoverable.
- [ ] **The two-person rule on, with at least two administrators** ([two-person rule](security.md#the-two-person-rule)). Why: one stolen administrator credential then cannot delete backups, lower protections or make another administrator alone; the rule cannot be turned on with fewer than two.
- [ ] **Read preference `secondary` (or `secondaryPreferred`), an upload cap and a backup window** on busy replica sets ([reading from a secondary](#reading-from-a-secondary), [throttling](#throttling), [backup windows](#backup-windows)). Why: a dump from the primary competes with production traffic and evicts its working set.
- [ ] **Scheduled restore tests, and chain tests for PITR streams** ([restore tests](verification.md#automated-restore-tests), [chain tests](pitr.md#chain-tests)). Why: a checksum proves the archive is intact, only a restore proves it is usable, and its duration is your measured RTO.
- [ ] **TLS to MongoDB**, with certificate and hostname verification on ([TLS settings](connections.md#tls-settings)). Why: the dump carries all your data and the connection carries its credentials.
- [ ] **Per-connection access** for every user and API key that is not an administrator, and few administrators ([per-connection access](security.md#per-connection-access), [roles](design/roles.md#roles-and-scopes)). Why: one team's credential should not read or restore another team's databases.
- [ ] **A key rotation runbook** you have walked through once: `secret.key`, the backup encryption key and the storage credentials ([key rotation](#key-rotation), [runbook](encryption.md#key-rotation-runbook)). Why: after a leak or a departure you rotate under pressure, and every rotation needs a new recovery kit.
- [ ] **MongoRescue in another failure domain than the databases**: another cloud account, region or cluster, with its metadata backups on a target outside the instance's own ([metadata backups](#metadata-backups), [Kubernetes](kubernetes.md#what-the-chart-runs)). Why: an incident that takes the databases must not take the tool that restores them.

## TLS and the reverse proxy

MongoRescue serves plain HTTP. Put it behind a proxy that terminates TLS, and make the MongoRescue port reachable only from the proxy: publish it on loopback (`-p 127.0.0.1:8080:8080`), or keep it on a private Docker network that only the proxy joins.

Two optional settings (Settings → Security) make the proxy setup complete. With *Trust proxy headers* on, MongoRescue takes the client address for login throttling from the last `X-Forwarded-For` entry and marks the session cookie `Secure` when `X-Forwarded-Proto` is `https`. Enable it only when a proxy sets these headers: otherwise clients could spoof them. If your proxy does not send `X-Forwarded-Proto`, set *Secure cookies* to `always` instead.

Caddy example:

```
backup.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Restores and large backup listings can run for a long time; raise the proxy's upstream read timeout accordingly.

## Authentication

Every API call needs a session or an API key; there is no unauthenticated mode. On a fresh data directory MongoRescue starts in **setup mode** and logs, at `WARN`, a one-time setup code (`Setup required: open http://.../ and enter setup code XXXX-...`). The code lives only in memory, changes on every restart and stops working once the first user exists. Complete setup immediately after deployment.

- **Users** sign in to the dashboard. Passwords are hashed with bcrypt and must be 12 to 72 bytes long. Every user has a dashboard role, viewer, operator or admin ([design/roles.md](design/roles.md)).
- **Sessions** use an `HttpOnly`, `SameSite=Strict` cookie and a CSRF token that the dashboard sends with every change. They expire after 12 hours of inactivity or 7 days after login, and are revoked on logout, from the session list (user menu → *Active sessions*, or Settings → Sessions; *All users* shows everyone's), when the user's password changes (all other sessions) or when the user is deleted.
- **Single sign-on** through an OpenID Connect provider (Entra ID, Google, Okta, Keycloak) is set up in Settings → Single sign-on; see [sso.md](sso.md). Keep at least one local administrator with a strong password as the break-glass way in (MongoRescue refuses to remove the last one while single sign-on is on), consider *Password sign-in: local administrators only*, and shorten `security.session_absolute_timeout` (for example to `12h`): sessions do not follow the provider's, so a user disabled at the provider keeps a running session until it ends, and API keys a single sign-on user created work until the user is deleted in MongoRescue.
- **Login throttling**: 5 failures for the same client address and username lock that pair out for 30 seconds, doubling up to 15 minutes. After 20 failures from one address, each further username from it is locked after one failure; a correct password for an unlocked user is never refused, so users behind one proxy address cannot be locked out by someone else. Attempts are reserved before the password check (one at a time per address and username), so parallel requests cannot exceed the budget. The lockouts live in memory.
- **API keys** are created in Settings → API keys, shown once, and stored only as SHA-256 hashes; each records when it was last used. Send them as `Authorization: Bearer <key>` or `X-API-Key: <key>`. Each key has a scope: `read` (the default), `operator` (also backups, job runs and safe-clone restores) or `admin` ([api.md](api.md#api-key-scopes)). A `MONGORESCUE_API_KEY` from an earlier build is imported once as an admin key named *Imported from MONGORESCUE_API_KEY*; it is stored as an HMAC-SHA256 under a subkey of `secret.key` rather than a plain hash, since it was chosen by hand. Remove the variable afterwards, and replace admin keys with narrower ones where you can.
- **AI assistants** use the [MCP endpoint](mcp.md) `/mcp` with an API key; it never accepts sessions, cannot delete anything or restore in place, and records every call in the audit log (Settings → Security). Behind a reverse proxy on the same host, turn on *Trust proxy headers*; otherwise `/mcp` refuses requests whose `Host` is not local (DNS-rebinding protection).

The channel **test** endpoint and the connection test make outbound requests to whatever destination they name, so only give accounts and keys to people and systems you trust with network access from the MongoRescue host.

## Data directory

`MONGORESCUE_DATA_DIR` holds:

| File | Contents |
| :--- | :--- |
| `mongorescue.db` (+ `-wal`, `-shm`) | SQLite metadata database: jobs, backup and restore history, users, sessions, API key hashes, connections and notification settings. Mode `0600`. |
| `secret.key` | The key encrypting connection strings and notification channel secrets inside the database (unless `MONGORESCUE_SECRET_KEY` is set). Mode `0600`. |
| `mongorescue.lock` | Advisory lock that stops a second instance from using the same directory. The operating system holds the lock for the process, not the file: after a crash it is released with the process, so a leftover `mongorescue.lock` never has to be deleted. |

Connection strings and channel secrets are encrypted with AES-256-GCM. **Losing the key loses them**: MongoRescue refuses to start when the key does not match the database (`the secret key does not match the key this database was encrypted with`). Store a copy of `secret.key`, or set `MONGORESCUE_SECRET_KEY` from your secret manager, and keep it apart from database backups so one leaked backup does not reveal both. A refused start changes nothing in the database: put the original key back (file or variable) and the stored credentials open again. `MONGORESCUE_SECRET_KEY` takes precedence over `secret.key` when both exist, and a damaged `secret.key` is reported, never replaced. The age identity used for backup encryption is one of these stored secrets; escrow it separately as well (see [encryption.md](encryption.md#key-management-and-loss)).

Losing the database does not lose the backup archives, but it loses the records that point to them, your schedules, users and connections. Back it up with [metadata backups](#metadata-backups) and keep `secret.key` in a [recovery kit](#recovery-kit).

### Disk space

Keep the data directory on a filesystem with room to spare: every run writes its record to `mongorescue.db` and its log under `logs/`, and a full disk stops both.

- **Before every run** (backups, job runs, restores, PITR base backups) MongoRescue checks the free space of the data directory. Below `MONGORESCUE_MIN_FREE_SPACE_MB` (`-min-free-space-mb`, 100 MiB by default, `0` turns the check off) the run does not start: the API answers `503` with the free space and the minimum (and `Retry-After: 30`), and a scheduled run is recorded as failed, so `backup.failed` and the job's heartbeat `/fail` ping fire.
- **When a metadata write fails because the disk is full** (SQLite `database or disk is full`, `ENOSPC`), MongoRescue publishes the critical event `system.disk_full` (sent to every enabled notification channel, whatever the rules), shows the `data_dir_full` warning (a dashboard banner, `GET /api/v1/settings`, counted by `mongorescue_settings_warnings`), and refuses new backups and restores until there is enough free space again; it checks every 30 seconds and resumes on its own. Where the free space cannot be read, the episode ends as soon as a write succeeds (a small probe file in the data directory, at most every 5 seconds, or the save of a record that waited). `system.disk_full` is sent at most once an hour: an episode that starts sooner after the last alert is only logged and shown as the warning.
- **A backup whose final record cannot be saved never reports success.** The save is retried for a few seconds; if it still fails, the backup is reported as failed (`backup.failed`, a failed job run, the heartbeat's `/fail` ping, no copies, no retention), the failure is logged and written to the run's log, and the failed record is saved as soon as writes work again (at the latest by the next start, which fails every run left in progress). Its archive is complete, so it is kept (`the backup record could not be saved (data disk full: …); the archive was kept`, never `archive_cleanup_pending`, and copies already made stay): the [storage scan](verification.md#storage-scans) lists it as an orphan and *Import* revives the record as a completed backup. Only the artifact of a run that failed by itself (an upload that broke off) is cleaned up, as before.
- **At the next start**, a backup still recorded as in progress is failed. If its archive exists in storage it may be complete, so it is kept (`interrupted before the record was saved; the archive was kept`, no `archive_cleanup_pending`): the [storage scan](verification.md#storage-scans) lists it as an orphan and *Import* revives the record as a completed backup. Only an archive smaller than the size recorded for it goes to the purge; when the target cannot be read nothing is deleted.
- **Only a full disk fails a run.** Any other failure of the final save (a busy or locked database, a timeout) is transient: the record is kept as it is and saved in the background, retried with a backoff from 1 second to 1 minute until it succeeds, and the run reports its real outcome. A good archive or clone is never deleted over a lock. A restart before that save succeeds leaves the record to the startup recovery.
- **The same holds for restores** (also point-in-time restores and chain tests): the record is stored before `restore.succeeded` is published. A restore whose record cannot be saved is reported as failed (`restore.failed`), and what it restored is handled like the clones of any failed restore: a safe clone, or the clones of a point-in-time restore or chain test, are dropped; an in-place target is never touched.
- The writes at the start of a run (the in-progress records, the chunks of the PITR collector) report a full disk too, so the episode starts before a run gets far.

The rest of the server copes with a full data directory: requests that must write fail with an error, the database stays consistent (SQLite rolls back the failed transaction), and nothing needs repairing once space is freed. Alert on the filesystem itself as well (node exporter's `node_filesystem_avail_bytes`), well before the minimum.

### Metadata backups

Settings → Recovery → *Metadata backups* (the `metadata_backup` settings, off by default) takes a snapshot of `mongorescue.db` on a schedule (every 24 hours by default) and on demand (*Back up now*, `POST /api/v1/metadata-backup/run`):

1. SQLite's `VACUUM INTO` writes a consistent copy of the live database to a temporary directory inside the data directory (mode `0700`). It is the only temporary copy MongoRescue writes, and it holds metadata, never a dump.
2. The copy is streamed through age encryption, with the backup encryption settings (Settings → Encryption), to the chosen storage target as `_mongorescue/metadata/<install_id>/mongorescue-<UTC time>.db.age`, and the temporary copy is removed. Storage scans ignore these objects. The install ID (16 hex characters; part of the snapshot key shown under Settings → Recovery, `prefix` in `GET /api/v1/metadata-backup` and `metadata_prefix` in the recovery kit) is derived from `secret.key`: it stays the same on a host restored with the same key, and installations sharing a bucket and prefix write, and prune, only their own snapshots. Clones that share one `secret.key` share the install ID too; give each installation its own key or its own target prefix.
3. All but the newest `retention_count` snapshots (14 by default) under the installation's own prefix are deleted from the target.

With encryption off, snapshots are still uploaded (as `.db`), and the dashboard warns until encryption is on. Credentials inside a snapshot (connection strings, S3 keys, notification secrets, the age identity and passphrase) stay sealed with `secret.key` either way, so a snapshot alone opens nothing; keep `secret.key` apart from the snapshots. Write the snapshots to a target that does not live on the MongoRescue host, or they share its fate. A failed snapshot is retried after an hour, shown under Settings → Recovery, counted in `mongorescue_metadata_backups_total{result="error"}` and published as `metadata_backup.failed` for notification rules. Alert on `time() - mongorescue_last_successful_metadata_backup_timestamp_seconds` as well.

Other consistent copies still work: stop MongoRescue and copy the data directory, run `sqlite3 /data/mongorescue.db ".backup '/backup/mongorescue.db'"` against the live database, or snapshot the whole volume atomically. Never copy `mongorescue.db` alone while the server runs (WAL mode: the copy may miss changes or be torn).

### Restore MongoRescue from a snapshot

On a new host (or after losing the data directory), with the [recovery kit](#recovery-kit) at hand:

1. Install the same or a newer MongoRescue release. Do not start it yet (a start creates a new `secret.key` and an empty database).
2. Download the newest snapshot from the target named in the kit (`recovery.json` → `metadata_snapshot`; the installation's prefix is `metadata_prefix`, `_mongorescue/metadata/<install_id>/`), with the target's credentials from the kit, e.g. `aws s3 cp s3://<bucket>/<prefix>_mongorescue/metadata/<install_id>/mongorescue-<time>.db.age .`. Pick a newer snapshot under the same prefix than the kit names if there is one.
3. Decrypt it with age: `age -d -i identities.txt -o mongorescue.db mongorescue-<time>.db.age` when the server holds the private key (`identities.txt` is in the kit). In recipient-only mode the server has no private key and the kit no `identities.txt`: use the key file you keep for the recipients, `age -d -i <your key file> -o mongorescue.db mongorescue-<time>.db.age`. In passphrase mode run `age -d -o mongorescue.db mongorescue-<time>.db.age` and enter the backup passphrase (the kit never contains passphrases). The kit's `README.txt` names the step that fits. A `.db` snapshot is not encrypted: rename it.
4. Put `mongorescue.db` and the kit's `secret.key` into the data directory, owned by the MongoRescue user, with `chmod 600`. If the old instance used `MONGORESCUE_SECRET_KEY`, set it to the content of `secret.key` instead. The key must be the one the snapshot was written with: with another key MongoRescue refuses to start and changes nothing.
5. Start MongoRescue. Jobs, backup records, users, settings, storage targets and notification channels are back; sign in with your usual account. Backups taken after the snapshot have no record yet: run a storage scan (Settings → Storage → *Scan now*) and import them.

Without any snapshot, start a fresh instance with the kit's `secret.key`, recreate the storage targets from `recovery.json`, put the private keys from `identities.txt` (or your passphrase) under Settings → Encryption and import the archives a storage scan finds.

### Recovery kit

Settings → Recovery → *Download recovery kit* (or the dashboard banner) asks for a kit passphrase (at least 12 characters, typed twice) and your current password, and downloads `mongorescue-recovery-kit-<date>.tar.age`: a tar archive sealed with age scrypt under that passphrase. MongoRescue does not keep the passphrase. Open it with `age -d -o kit.tar mongorescue-recovery-kit-<date>.tar.age` and `tar -xf kit.tar`. It contains:

| File | Contents |
| :--- | :--- |
| `README.txt` | The recovery steps above |
| `secret.key` | The key sealing the credentials in `mongorescue.db` and its snapshots |
| `secret.key.previous` | Only when asked for (*Include the previous key*): the key the last `secret.key` rotation replaced, which opens the snapshots taken before it |
| `identities.txt` | The age X25519 private keys, current and retired (only when one is configured) |
| `recovery.json` | Encryption settings (passphrases are only flagged as configured, never included), every storage target with its credentials, and the location of the latest metadata snapshot |

The kit holds every secret in plain form once opened: store it offline (a password manager, an encrypted USB stick, a safe), apart from the backups and from the kit passphrase. Only a signed-in administrator can download it, after confirming their password; API keys are refused whatever their scope. Every download is written to the audit log (who and when, never the passphrase or the content).

Until a kit has been downloaded, and again after `secret.key`, the encryption keys or a storage target change, the dashboard shows a reminder. *Remind me later* hides it until the next such change.

Since v0.14.0 the database records a checksum of every schema migration applied to it, and MongoRescue refuses to start (`ErrMigrationChanged`, naming the migration, nothing written) when a binary embeds an edited version of one; starting an older release on a database migrated by a newer one is refused as before (`ErrSchemaTooNew`).

A single damaged row (a record whose stored JSON no longer fits, or whose credentials cannot be decrypted) does not take a list down: it is skipped, logged with its table and ID, shown to administrators in a dashboard banner and left unchanged on disk. [troubleshooting.md](troubleshooting.md#unreadable-records) shows how to back up the database, inspect the row with `sqlite3`, and repair, export or remove it.

Releases before the SQLite store kept metadata in `state.json`; it is imported automatically on the first start and renamed to `state.json.migrated-<timestamp>` (see [configuration.md](configuration.md#upgrading-deprecated-environment-variables-and-configjson)). Job connection strings from those releases become managed connections.

## RPO and RTO

The recovery point objective (RPO) is how much data you can afford to lose: how old the newest good backup may be when disaster strikes. Every job has one: its *Recovery point objective* in the job form (`rpo_minutes`, 15 minutes to 90 days), or by default twice the longest gap between its runs plus an hour, at least six hours (an hourly job gets 6 hours, a daily one 49, a weekday job 145, since Friday to Monday is its longest gap). MongoRescue checks every database of every enabled job every 5 minutes and whenever a job is saved, paused, resumed or backed up, publishes `job.rpo_missed` once when its newest successful backup is older than that and `job.rpo_recovered` when it is fresh again, and exports `mongorescue_job_rpo_seconds` and `mongorescue_job_rpo_met` ([metrics.md](metrics.md#recovery-point-objectives)). A database without a successful backup counts from when it joined the job, so adding one to an old job does not alert at once. Set the RPO to what you promised, not to the schedule: a daily job whose data must never be more than a day old needs `24` hours, not the default 49.

Pausing a job forgets its breaches without an event. Resuming a job whose newest backup is still older than its objective therefore alerts right away (`job.rpo_missed`), before its next run had a chance to catch up; run it once by hand after resuming if you expect that.

The recovery time objective (RTO) is how long a restore may take. MongoRescue estimates it per database from the newest passed [restore test](verification.md#automated-restore-tests) (restoring into a temporary database, so the figure includes the copy and the index builds), else from the newest completed real restore of the whole database; without either it is *Unknown*. Turn on restore tests to keep the estimate current, and compare it with your objective: the estimate grows with the data.

Overview → *Recovery readiness* (and `GET /api/v1/readiness`, [api.md](api.md#recovery-readiness)) brings this together per database: the last good, verified and restore-tested backups, the RPO with its current age, the estimated RTO, and whether the keys are in a recovery kit. A row is *Not ready* when an RPO is missed or the newest restore test or verification failed, and shows a *Warning* when it was never verified or restore-tested, its jobs are paused, or its encrypted backups have no escrowed key.

**Filtered backups and the RPO.** A backup with a [collection filter](api.md#collection-filters-per-database) (only some collections, or all but some) counts towards its database's RPO like any successful backup: the database is "fresh" as soon as the filtered backup completes. It does not cover the collections it left out: their newest backup may be much older, or there may be none, and neither the RPO check nor the restore tests notice. Such backups carry `filtered: true` with the collections they kept (`collections`) or skipped (`exclude_collections`), show a *Filtered* badge in the Backups list and details, and the readiness row says *Covered partially: N collections excluded* (or *only N collections*). If the excluded collections matter, back them up with a job of their own and give it its own RPO.

## 3-2-1 backups

The 3-2-1 rule asks for **3** copies of your data (the live database and two backups), on **2** different kinds of storage, with **1** of them off site. MongoRescue gets there with [copy targets](configuration.md#copies-on-a-second-target-3-2-1):

1. Add a second storage target in another place than the primary. If the primary is a local disk or a NAS, use an S3 bucket in another region or with another provider. If the primary is S3, use a bucket at another provider, or a local disk you take off site.
2. In the job form, check that target under *Copy targets* (up to three). Keep *Copy mode* on **async** unless a run must not count as done until its copy exists (**sync**).
3. Every backup is then copied byte for byte, checked against its SHA-256, under the same key. Encryption stays as it is: an encrypted archive stays encrypted on every target, so the copy target never sees plaintext and needs no key.
4. Give the off-site copy its own immutability: an S3 target with [Object Lock](configuration.md#immutable-backups-s3-object-lock) locks each copy with its own retention, so even credentials stolen from the primary cannot delete it.

What you get:

- **Restore from either target.** A restore reads the primary, and falls back to a healthy copy by itself when the primary archive is missing or has the wrong size; the restore record says so (`source_fallback`). The dashboard's restore dialog (*Read the archive from*), `--from-target` on the CLI and `source_target_id` in the API choose a copy explicitly.
- **Verification of every copy.** The integrity sweep re-reads every copy like the primary and copies a damaged or missing one again.
- **Alerts.** `backup.copy_failed` and `backup.copy_recovered` events (subscribe to them in notification rules), the `mongorescue_backup_copy_queue_depth` metric, and the readiness warning `copy_missing` when a backup's copies are not done six hours after it finished. The readiness row shows "N copies" for the newest backup.
- **One lifecycle.** Retention and deletion apply to the backup with all its copies: a deleted backup can be undeleted with its copies during the grace period, and the purge then removes every copy, each after its own target's lock.

Copies read storage, not MongoDB, so they run outside backup windows too; they upload at the job's (or the general) `max_upload_mbps`. Size the copy target like the primary: it holds the same archives for as long as the primary does.

## Cross-region disaster recovery

"We use S3" is not disaster recovery: a region outage, a lost account or one leaked key can take the primary bucket and every copy in the same failure domain with it. Real DR keeps an **immutable copy in another region, behind other credentials**, and proves regularly that a restore works **from that copy alone**. MongoRescue checks each part and sums it up per database under Overview → *Recovery readiness* (*DR: cross-region ✓* or *DR: same region ✗*).

### Locked copies

A job with **Require locked copies** (`require_locked_copies` in the API) is refused (`400`) unless every one of its copy targets has [S3 Object Lock](configuration.md#immutable-backups-s3-object-lock), so a copy is never left deletable. **Settings → Security → Require locked copies** (`security.require_locked_copies`) applies to every job while it is on: no job can opt out (a new job without the policy, or a job turning it off, is refused with `400`), and the copy targets of every job and PITR stream must have Object Lock. Turning the setting off, or a job's policy while the setting is off, is a lowered protection: it takes effect after the [delete grace period](security.md#lowering-a-protection-takes-the-grace-period-too) and, under the [two-person rule](security.md#the-two-person-rule), only once a second administrator approved it (pending change and approval kinds `disable_locked_copies` and `job_locked_copies`).

### Region awareness

Every storage target has a **region**: an AWS S3 target takes its `s3.region`, or, when that is empty or `auto`, the bucket's location (`GetBucketLocation`, needs `s3:GetBucketLocation`); any other target (MinIO, another S3 provider, a local disk or NAS) only has the label you give it (for example `dc-frankfurt`), since its `s3.region` says nothing about where the data is. Set the label on every such target. For a database whose jobs have copy targets the readiness report then warns:

- `dr_same_region` when no copy target is in a known region other than the primary's (an unknown region never counts as another one);
- `dr_same_credentials` when every copy target is reachable with the primary's credentials or account, where this is detectable (see [copy targets in another account](#copy-targets-in-another-account));
- `dr_unlocked_copy` when a job that requires locked copies has a copy target without Object Lock (a job saved before the setting was turned on). The check of a job's copy targets runs when they or its policy change, so pausing or renaming such a job still works; removing the Object Lock of a copy target that a requiring job or stream uses is refused (`400`).

### DR drills

A **DR drill** is a [restore test](verification.md#automated-restore-tests) that reads the archive from a copy target instead of the primary: in the job form, *Restore test* → *Read from* a copy target (`restore_test.source_target_id`, one of the job's `copy_targets`). Pick the copy in the other region. A drill tests the newest backup whose copy on that target is complete (with async copies that may be the previous backup); a scheduled drill is skipped while no backup has a complete copy there yet, and a manual one is refused. The result names the target it read (`source_target_id`), and the readiness row shows the last drill and its result. `dr_drill_stale` warns when no drill passed in the last 30 days.

**DR: cross-region.** A database's readiness row shows *DR: cross-region* once one of its jobs has a copy target in another region with Object Lock, and a drill from such a copy passed within the last 30 days. Otherwise it shows what is missing (*same region*, *not locked*, *no recent drill*).

### Region failure

The runbook for "region A is gone" (the primary target, maybe the databases and MongoRescue with it). Prepare it before you need it:

- The **recovery kit** (and its passphrase) and the backup **encryption key** are stored **outside region A**: in another region, another account, or offline. Without `secret.key` the stored credentials are unreadable, and without the private key encrypted archives are.
- [Metadata backups](#metadata-backups) go to a target in region B, so the instance's own database survives too.
- A drill from the region B copy passed recently (no `dr_drill_stale`).

When region A fails:

1. **Start MongoRescue in region B** (or anywhere with access to region B). If the instance in region A is gone, restore it from the metadata backup in region B with the recovery kit ([restore MongoRescue from a snapshot](#restore-mongorescue-from-a-snapshot)), or start a fresh instance and add the region B copy target with its own credentials.
2. **Check what region B holds.** Every backup lists its copies; the region B target must show them *done*. Copies that were still pending when region A went are lost with it: the newest restorable backup is the newest one with a done copy in region B.
3. **Restore from region B.** A restore falls back to a healthy copy by itself when the primary archive cannot be read; to be explicit, choose the region B target under *Read the archive from* (`source_target_id`, `--from-target` on the CLI). Restore into a new cluster in region B, as a safe clone first.
4. **Point the jobs at region B.** Until region A is back, make the region B target the primary of the jobs (and add a new copy target in a third region), so new backups do not fail against the lost one.
5. **After region A returns**, run the integrity sweep: it verifies every copy and copies missing or damaged ones again.

## Large databases

MongoRescue streams `mongodump --archive`: a logical copy that reads every document through the query layer of the member it connects to. That is simple, portable across versions and restorable into any database, but its cost grows with the data:

- **Load.** A dump scans every collection, pulls the whole data set through the member's cache (evicting the working set your application relies on) and competes for disk and network. On a busy primary that shows as latency.
- **Time.** Expect roughly the read throughput of the member's disks, often 50 to 200 MB/s before compression; a restore is slower, since `mongorestore` rewrites every document and rebuilds every index. Measure both with a [restore test](verification.md#automated-restore-tests): its duration is the RTO MongoRescue reports.
- **Consistency.** A dump without the oplog is consistent per document, not across collections: writes during a long dump land in some collections and not in others. Use [point-in-time recovery](design/pitr.md) when that matters.

As a rule of thumb, `mongodump` is comfortable up to a few hundred GB per database and a dump window of a few hours. Beyond about 1 TB, when the restore must finish in less time than a logical restore takes, or when the dump cannot fit into a quiet window even from a secondary, use a physical backup instead: filesystem or volume snapshots (LVM, ZFS, EBS, persistent disk snapshots) of a secondary with journaling on the same volume, your cloud provider's backups (MongoDB Atlas cloud backups, Ops Manager or Cloud Manager), or Percona Backup for MongoDB. Keep MongoRescue for the databases that fit, for portable copies, and for restore tests of what you can restore logically.

### Reading from a secondary

Each connection may set a **read preference** (`primary`, `primaryPreferred`, `secondary`, `secondaryPreferred` or `nearest`, with optional tag sets such as `dc=east,use=backup`), and each job may override it. Empty keeps whatever the connection string says, which is the primary unless it sets `readPreference` itself. MongoRescue adds the preference to the connection string it hands `mongodump` (through the private tools config file, never on the command line) and to the driver that captures the [manifest](verification.md), so both read from the same kind of member.

- `secondaryPreferred` reads from a secondary and falls back to the primary when none is available; it is the usual choice.
- `secondary` never reads from the primary: a backup fails before `mongodump` starts when no secondary is reachable, also on a standalone server or a single-member replica set.
- Tag sets pick dedicated members, such as a hidden or delayed member tagged for backups; they are tried in order.
- A `directConnection=true` URI talks to one member only and ignores read preferences: list the replica set members (or use `mongodb+srv://`) with `replicaSet=` instead.

Before `mongodump` starts, MongoRescue asks the member the preference selects (`hello`) and records it on the backup (`source_member`: host, `primary` or `secondary`, and the replica set); the run log names it too, and *Test connection* reports which member a backup would read from. With several eligible secondaries `mongodump` may choose another one of them; it still satisfies the same preference. Remember that a secondary may lag: a backup from it is as old as its replication lag.

### Throttling

- **Upload cap.** `max_upload_mbps` (megabits per second) caps the stream to storage with a token bucket: per job, or for every job without one under Settings → General (`general.max_upload_mbps`). The dump slows down with it, since `mongodump` writes into the same pipe, so it also lowers the read rate on the member.
- **Parallel collections.** `num_parallel_collections` (1 to 16) is `mongodump --numParallelCollections`; `mongodump` dumps 4 collections at once by default, and 1 is the gentlest.
- **Concurrent backups per connection.** `max_concurrent_backups` on a connection limits how many backups (from all of its jobs, scheduled, on demand or manual) read from it at once; `0` is unlimited. Further backups wait in arrival order, shown as *Waiting for a slot* (`phase: "waiting"` in the active runs), and start as soon as one ends. Waiting does not count against the backup timeout, and a waiting backup can be cancelled.

### Backup windows

A job's **backup window** (`backup_window`: a time zone, the days it opens on and a start and end time) lets its scheduled runs start only within those hours; an end before the start closes the window the next day, so `22:00` to `02:00` on `sat` covers Saturday night into Sunday. Times follow the zone's daylight saving changes: a window opening at a time that the clock skips opens when the clock jumps.

- A scheduled run outside the window does not start. Consecutive skips share one *skipped* job run (`status: "skipped"`, `skip_reason: "outside window"`, `skipped_runs`), shown as "N scheduled runs skipped outside the window"; the first skip of each gap emits `backup.skipped`, which only rules that name it receive. A skip is neither a success nor a failure: it does not count in the failure counters, the heartbeat, the history or the job's last status.
- With `cancel_at_window_end` a scheduled run still going when the window closes is cancelled (recorded as cancelled by `system`, "the backup window ended"). Without it, the run finishes.
- Manual and on-demand runs ignore the window; the dashboard warns before starting one.
- The default RPO counts only the runs the window allows: an hourly job with a window of 01:00 to 05:00 runs at 01, 02, 03 and 04 o'clock, so its longest gap is 21 hours and its default RPO 43 hours.

### S3 multipart limits

Archives are uploaded to S3-compatible storage with multipart uploads, two parts at a time, so the archive is never buffered. S3 allows at most 10,000 parts per object, so the part size caps one archive (after compression and encryption). Each S3 target sets its part size (`s3.part_size_mb`, Settings → Storage → *Upload part size*), from 5 to 512 MiB:

| Part size | Largest archive | Upload memory (part size × 2) |
| :--- | :--- | :--- |
| 5 MiB | about 48.8 GiB | 10 MiB |
| 16 MiB (default) | about 156 GiB | 32 MiB |
| 64 MiB | about 625 GiB | 128 MiB |
| 128 MiB | about 1.2 TiB | 256 MiB |
| 512 MiB | about 4.9 TiB | 1 GiB |

The memory is per running upload, so multiply it by the number of backups that can run at once (jobs, and each connection's `max_concurrent_backups`) and leave room for it in the container's memory limit. The dashboard shows the largest archive next to the field. Raise the part size before a database grows into the limit:

- Before each backup to an S3 target, MongoRescue estimates the archive from the size of the database's last completed backup on the same connection, or from the server's `dbStats` data size (uncompressed) when there is none. Above **80%** of the target's largest archive the backup is recorded with a warning (`warnings` on the backup, shown in the backups list and the run log) and logged; it still runs. The job form's database preview shows the same warning for the selected target.
- An upload that still runs out of parts fails with an error that names the limit and the `part_size_mb` setting, not a raw SDK error.

Single objects are limited to 5 TiB by S3 anyway, and some S3-compatible services have lower limits on parts or objects. Compress (`gzip`), back up large databases per collection with several jobs, or use a local or file-system target (for example a mounted volume) for archives near that size, and see [Large databases](#large-databases) for when a logical dump is no longer the right tool.

## Encryption

Use X25519 recipients (Settings → Encryption) rather than a passphrase: the instance taking backups then needs only the public key, and a compromise of that host or its storage credentials does not expose backup contents. Keep the private key on the host (or in the secret store) used for restores. See [encryption.md](encryption.md).

For restores into existing databases, keep the *Verify before restore* policy (Settings → General) at `auto` or `always` so a corrupted or undecryptable artifact is rejected before `mongorestore` touches the target.

## Key rotation

After a suspected leak, when someone who held a key leaves, or on a schedule, rotate from **Settings → Security → Key rotation**. The full runbook, with what each rotation changes, is in [encryption.md](encryption.md#key-rotation-runbook).

| What | How | Afterwards |
| :--- | :--- | :--- |
| `secret.key` | *Rotate secret.key* (your password; a second administrator under the two-person rule), or `POST /api/v1/security/rotate-secret-key` | Everyone signs in again. Download a new recovery kit; keep the previous one (or include `secret.key.previous`) for snapshots taken before the rotation. Those snapshots are deleted once the delete grace period has passed, then `secret.key.previous` too (after a new kit; otherwise a warning asks for one) |
| `MONGORESCUE_SECRET_KEY` | Not rotated by MongoRescue: move the key into `secret.key`, rotate, then set the variable to the new key ([procedure](encryption.md#keys-from-mongorescue_secret_key)) | As above |
| Backup encryption key | *Rotate encryption key*, optionally re-encrypting existing backups, or `POST /api/v1/encryption/rotate` | Escrow the new identity (new recovery kit); keep the old ones until no backup needs them |
| S3 credentials | *Test and rotate* with the new key pair, or `POST /api/v1/storage-targets/{id}/rotate-credentials` | Revoke the old key at the provider |

A rotation of `secret.key` is crash-safe: a start after an interruption completes or rolls it back (logged), and never accepts a key the rotation did not name.

### Least-privilege storage credentials

Give MongoRescue a key limited to its bucket and prefix, with only the actions it uses: `s3:PutObject` (backups, snapshots, probes), `s3:GetObject` (restores, verification), `s3:ListBucket` (scans, retention, probes; restrict it with an `s3:prefix` condition) and `s3:DeleteObject` (retention, purges, re-encryption, probes), plus `s3:AbortMultipartUpload` and `s3:ListMultipartUploadParts` for large uploads. A credential rotation refuses a key that cannot write, read, list or delete. With object lock or versioning, deletions keep a version: size the bucket's lifecycle rules accordingly. Add a lifecycle rule that aborts incomplete multipart uploads after a day (`AbortIncompleteMultipartUpload`): a failed upload is aborted when the storage answers, but a backup cut off by a crash, a SIGKILL or a storage outage longer than the 30-second abort window leaves its uploaded parts behind; they are not an object and no backup refers to them, but they are billed until removed ([fault-injection tests](testing.md#fault-injection-suite)). Example AWS policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": "arn:aws:s3:::my-backups",
     "Condition": {"StringLike": {"s3:prefix": ["mongorescue/*"]}}},
    {"Effect": "Allow", "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject",
      "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"],
     "Resource": "arn:aws:s3:::my-backups/mongorescue/*"}
  ]
}
```

Use a separate key per installation, so revoking one never stops another.

#### Copy targets in another account

A copy only protects against a leaked key or a compromised account if that key or account cannot reach it. Put every copy target in **another account** (AWS account, Cloudflare account, MinIO tenant) than the primary, with **its own key** that MongoRescue alone holds:

1. Create the copy bucket in the second account, in another region than the primary, with **Object Lock (compliance mode) and versioning** enabled at creation.
2. Create a user (or role) in that account whose policy allows only what copies need on that bucket and prefix: `s3:PutObject`, `s3:GetObject` and `s3:ListBucket` (copies, verification, restores and drills), `s3:DeleteObject` (the purge, which Object Lock holds back until the retention ends), `s3:AbortMultipartUpload` and `s3:ListMultipartUploadParts` (large copies), plus `s3:GetBucketObjectLockConfiguration`, `s3:GetBucketVersioning`, `s3:PutObjectRetention` (and `s3:PutObjectLegalHold` with *legal hold on pin*) for Object Lock, and `s3:GetBucketLocation` so the target's region is detected. Never give it `s3:BypassGovernanceRetention`, `s3:PutBucketObjectLockConfiguration` or `s3:DeleteBucket`.
3. Do not reuse the primary's access key, and do not rely on the default credentials (instance profile, environment) for both: two targets without keys on one endpoint share the process's credentials.
4. Keep the second account's root and administrator credentials out of the place that runs MongoRescue, and out of region A.

The readiness report checks the part it can see: `dr_same_credentials` warns when every copy of a job uses the primary's access key ID, both use the default credentials on the same endpoint, or both are on the same Cloudflare R2 account (the account is part of the endpoint). It cannot see AWS account IDs or role trust, so a different key in the same account passes the check: the account split is yours to keep.

## Container images

Images are published to `ghcr.io/yigitcittan/mongorescue` for `linux/amd64` and `linux/arm64`. Tags:

| Tag | Meaning |
| :--- | :--- |
| `0.1.0` | Exact release (recommended) |
| `0.1` | Latest patch of a minor release |
| `latest` | Current `main` branch; changes without notice |
| `sha-<commit>` | A specific commit |

Mount `/data` and `/backups` as volumes; the image already uses them as the data directory and the local backup directory, so no environment variable is needed.

The container runs as the unprivileged user `mongorescue` (UID and GID `10001`). Named Docker volumes inherit the ownership of the image directories, so they work as-is. Bind mounts keep the ownership of the host directory and must be writable by UID 10001, for example:

```bash
sudo install -d -o 10001 -g 10001 -m 750 /srv/mongorescue/data /srv/mongorescue/backups
docker run -v /srv/mongorescue/data:/data -v /srv/mongorescue/backups:/backups ghcr.io/yigitcittan/mongorescue:0.1.0
```

The image defines a `HEALTHCHECK` that calls `GET /api/v1/health` inside the container. Change the listening port with `MONGORESCUE_SERVER_PORT` rather than the `-port` flag, so the server and the health check use the same port.

## Docker Compose

The repository's `docker-compose.yml` is hardened by default. Keep these properties when you adapt it:

- **No required configuration.** The file needs no `.env`; every variable in `.env.example` is optional. Keep an `.env` you do create (for example with `MONGORESCUE_SECRET_KEY`) out of version control with mode `600`.
- **Port.** The dashboard is published as `${MONGORESCUE_PORT:-8080}:8080` on all interfaces so it works out of the box. Before exposing the host, either change the mapping to `127.0.0.1:8080:8080` and put a TLS-terminating proxy on the host (the Caddy example above works unchanged), or put the proxy in the same Compose project, let it reach `mongorescue:8080` over the project network and remove the `ports` entry entirely.
- **Pinned image.** `MONGORESCUE_VERSION` selects the image tag. Set it to an exact release such as `0.1.0` so a `docker compose pull` does not change the version unexpectedly.
- **Bind mounts.** The file uses named volumes, which get the right ownership automatically. If you replace them with host directories, create them owned by UID/GID `10001` first (see [Container images](#container-images)).
- **Locked-down container.** The service runs with `read_only: true`, a `tmpfs` on `/tmp` (where `mongodump` and `mongorestore` receive their temporary `--config` file), `cap_drop: [ALL]` and `no-new-privileges`. The only writable paths are `/data`, `/backups` and `/tmp`.
- **Back up the data volume.** The data volume (`/data`) holds the metadata database and `secret.key`. Back it up as described in [Data directory](#data-directory), for example with `docker compose stop mongorescue` before copying the volume.

- **Stopping during a backup.** `docker compose stop` gives the container 10 seconds by default, after which running backups are cancelled (recorded as cancelled, their partial archives removed). To let them finish, set `MONGORESCUE_SHUTDOWN_GRACE` (for example `9m`) and a longer `stop_grace_period` (for example `10m`); see [Graceful shutdown](kubernetes.md#graceful-shutdown).

To attach MongoRescue to a Compose project that already runs MongoDB, start from [examples/compose-existing-stack.yml](../examples/compose-existing-stack.yml).

## systemd

`scripts/mongorescue.service` runs the binary as a daemon with `-data-dir=/var/lib/mongorescue/data`; the default "Local disk" storage target is then `/var/lib/mongorescue/backups`. Everything else is configured in the dashboard. To supply `MONGORESCUE_SECRET_KEY` from a file, use an `EnvironmentFile=` readable only by the service user. The shipped unit runs as `root`; consider a dedicated user that owns the data and backup directories. To let running backups finish on `systemctl stop`, set `Environment=MONGORESCUE_SHUTDOWN_GRACE=9m` and `TimeoutStopSec=10min` ([graceful shutdown](kubernetes.md#graceful-shutdown)).
