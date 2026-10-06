# Privacy

MongoRescue is self-hosted software. It has no telemetry, analytics or crash reporting, and the project runs no service that receives data from it.

This page covers two things: what MongoRescue itself sends and stores ([policy](#what-mongorescue-connects-to)), and how the personal data in your databases lives on in backups, and what to do about erasure requests such as GDPR Article 17 ([backups and erasure requests](#backups-and-erasure-requests)).

## What MongoRescue connects to

MongoRescue only makes network connections that you configure or that are listed here:

- **MongoDB servers** you add as connections, to run backups and restores.
- **Storage targets** you configure (local disk or an S3-compatible service), to store and read backup archives.
- **Notification channels** you configure (for example webhooks or email), to send backup and restore events.
- **GitHub (desktop app only).** At startup and every 6 hours the desktop app asks `api.github.com` for the latest release of `YigitCittan/mongorescue`, and when you choose to update it downloads the release files from `github.com`. These requests carry the app version in the `User-Agent` header and nothing about your data. GitHub's handling of these requests is covered by the [GitHub Privacy Statement](https://docs.github.com/site-policy/privacy-policies/github-general-privacy-statement).

## Data stored on your machine

Settings, connection details, users and backup metadata are stored in the data directory on your machine (`mongorescue.db`). Credentials in it are encrypted with a key that is also kept in the data directory (`secret.key`). Nothing in the data directory leaves your machine unless you copy it, or you turn on [metadata backups](production.md#metadata-backups), which write encrypted snapshots of it to a storage target.

## Backups and erasure requests

A backup is a copy of your data at one moment. When a person asks you to erase their data, deleting it from the live database does not delete it from the backups taken before: it lives on in every archive, oplog chunk and restored clone until they expire or are removed. MongoRescue cannot find or edit one person's documents inside an archive (archives are compressed, usually encrypted, and checksummed: editing one would break its verification), so the controls are retention and re-applying erasures after a restore.

### What each artefact contains

| Artefact | Contains personal data from your databases? | What it holds |
| :--- | :--- | :--- |
| **Backup archives** | Yes, all of it | A `mongodump` archive of one database (or the selected collections): every document, index definitions, optionally users and roles. Encrypted with age when backup encryption is on. |
| **PITR oplog chunks** | Yes, including deleted data | The oplog of a replica set: every insert, update and delete, with the documents they wrote. A document erased in production is still in the chunks that recorded its insert and updates. Always encrypted. |
| **PITR base backups** | Yes, all of it | A `mongodump --oplog` archive of the whole instance (every database). Always encrypted. |
| **Restored clones** | Yes, all of it | The databases a restore creates on a MongoDB server (`<db>_rescue_<timestamp>`, and `<db>_rescue_<timestamp>_<id>` for point-in-time restores). MongoRescue never drops a successful clone. The temporary databases of restore tests and chain tests are dropped when the test ends. |
| **Run logs** | Rarely | The redacted output of `mongodump` and `mongorestore` for one run: namespaces, document counts, progress, errors. Tool errors can quote a document key, for example the value of a duplicate key. |
| **Backup manifests** | No | Per collection the document count and the index definitions (keys, never values). |
| **The audit log** | No data from your databases | Who did what, when and from where: user names, API key names, client IP addresses and user agents of MongoRescue's own users, the action and its target IDs. Never request bodies, setting values or document contents. |
| **The API key activity log** | Rarely | The redacted arguments of API key and MCP calls: IDs, database and collection names, filters a client sent. |
| **Restore and backup records** | Rarely | Database and collection names, sizes, counts, outcomes and redacted error messages (which can quote a tool error, see run logs). |
| **Metadata snapshots** | No data from your databases | Encrypted copies of `mongorescue.db`: users (password hashes), connections (credentials sealed with `secret.key`), jobs, settings, the history and both logs above. |
| **The recovery kit** | No data from your databases | `secret.key`, the age identities, storage target credentials and the location of the latest metadata snapshot, sealed under the kit passphrase. Stored wherever you put it. |
| **Notification payloads** | No | Event type, job, backup and restore IDs, the database name, sizes, redacted errors and a rendered summary ([notifications.md](notifications.md#webhook-payload)). They leave MongoRescue for the channels you configure, which keep them by their own rules. |

### Where each artefact lives and how long it is kept

| Artefact | Where | Kept by default | Configured with |
| :--- | :--- | :--- | :--- |
| Scheduled backup archives | The job's storage target | 30 days, at least the newest 10 (`default_retention_days`, `default_retention_count` for new jobs), plus the delete grace period | The job's `retention_days` and `retention_count` |
| Manual, on-demand and MCP backups | The chosen storage target | Until someone deletes them, plus the delete grace period | Delete them, one by one or in bulk |
| Pinned backups (legal hold) | The storage target | Forever, until unpinned | Unpin (admin), then retention or a delete applies |
| PITR oplog chunks | The stream's storage target, `_mongorescue/oplog/…` | As long as the oldest kept base needs them (7 bases or 14 days), plus the delete grace period | `oplog_max_days` and the base retention of the stream |
| PITR base backups | The stream's storage target, `_mongorescue/base/…` | 7 bases or 14 days, plus the delete grace period | The stream's base retention |
| Deleted backups, chunks and bases | Unchanged on the storage target | `security.delete_grace_days`, 7 days | 1 to 90 days |
| Restored clones | The target MongoDB server | Until you drop them | Drop them when you are done |
| Run logs | `<data_dir>/logs/<id>.log` | 30 days, at most until the backup is purged | `general.log_retention_days` |
| The audit log | `mongorescue.db` | 365 days | `audit.retention_days`, 30 to 36500 |
| The API key activity log | `mongorescue.db` | The newest 10,000 entries | Not configurable |
| Restore and backup records | `mongorescue.db` | Backup records until their backup is purged; restore records indefinitely | Not configurable |
| Metadata snapshots | A storage target, `_mongorescue/metadata/<install_id>/` | The newest 14 | `metadata_backup.retention_count` |
| The recovery kit | Wherever you store the download | Your choice | Delete old kits when you download a new one |
| Notification payloads | The receiving system | Its own rules | The receiving system |

Retention only deletes after a successful scheduled run of a job, never deletes the newest `retention_count` scheduled backups or the job's newest verified backup, and never deletes faster than the delete grace period: see [configuration.md](configuration.md#general) and [security.md](security.md#delete-protection). A shorter retention, a shorter grace period and a lower `metadata_backup.retention_count` take effect only after the current grace period.

### Configuring retention

- **Job retention.** `retention_days` and `retention_count` on each job (job form or `PUT /api/v1/jobs/{id}`); `0` turns a rule off, and both at `0` keep backups forever. The retention preview (`GET /api/v1/jobs/{id}/retention/preview`) shows what the next run deletes. Manual and on-demand backups are never pruned: delete them when they are no longer needed.
- **`oplog_max_days`** on a [PITR stream](pitr.md#enabling-a-stream) is the main control for oplog data: chunks older than that many days are deleted even when that shortens the point-in-time window. Without it, chunks stay as long as the base retention needs them. Bases follow the stream's base retention.
- **The audit log.** `audit.retention_days` (Settings → Audit log), 30 to 36500 days. Pruning keeps the hash chain verifiable ([audit.md](audit.md#retention)).
- **Run logs.** `general.log_retention_days` (Settings → General).
- **The delete grace period.** `security.delete_grace_days`, 1 to 90 days: every deletion (by a user, by retention, of a chunk or a base) keeps the object this long so it can be undone, and the purge removes it afterwards. A longer grace period protects against ransomware and mistakes but lengthens the time erased data survives, so choose it with both in mind.
- **Metadata snapshots.** `metadata_backup.retention_count` (Settings → Recovery).
- **Object Lock (coming in [#59](https://github.com/YigitCittan/mongorescue/issues/59)).** S3 Object Lock will make archives undeletable until their retention date, even for whoever holds the bucket's credentials. Erased data then survives until that date by design: set the lock period no longer than your retention needs.

Restored clones are not covered by any retention: drop them once they have served their purpose.

### Point-in-time recovery and deleted data

A [point-in-time recovery stream](pitr.md) (experimental) stores the oplog of a replica set: every write, including the documents of writes that were later deleted. Deleted data therefore survives in the stored oplog chunks until retention removes them:

- PITR streams require backup encryption: chunks and base backups are always age-encrypted.
- `oplog_max_days` deletes chunks older than that many days, even when that shortens the point-in-time window. Deleted chunks keep their object until the delete grace period ends; the purge then removes it.
- The API and the dashboard show chunk metadata only (positions, counts, sizes, checksums), never oplog contents.
- Configuring streams is admin-only.

A point-in-time restore replays the oplog up to the chosen moment, so it brings back everything that existed then, erased documents included: re-apply the erasure log afterwards as after any other restore.

### The common legal position

Supervisory authorities and most legal advice accept that personal data is not erased from backups one record at a time, as long as:

1. **Backups expire on a schedule.** Every copy is deleted when its retention ends, and the retention is documented and no longer than needed. Keep the grace period and `oplog_max_days` in that calculation.
2. **Backups are not used for anything else.** They are kept for recovery only, are access-controlled and encrypted.
3. **Erasures are re-applied after every restore.** Restored data must not bring an erased person back: before restored data goes into use, every erasure made since the backup was taken is applied again.

Pinned backups (legal hold) are kept for a legal obligation and are exempt from erasure for as long as that obligation lasts; record why each one is pinned (the pin note). This is general information, not legal advice: confirm the position that applies to you with your data protection officer.

### Keep an erasure log

Record every erasure in an **erasure log**, kept apart from the data it erases, so it can be re-applied after a restore:

- what to erase, by stable identifiers (the `_id` or a customer number, not the personal data itself), the database and collection, and the date of the request;
- the commands that erase it, for example `{"delete": "users", "deletes": [{"q": {"_id": {"$in": [...]}}, "limit": 0}]}` and the matching deletes or updates in other collections;
- when it was applied to production.

After every restore, apply the whole log to the restored data before it is used, and keep the log for as long as the oldest backup it may need to be re-applied to: entries older than every backup, chunk and clone can be removed. Do the same after restoring a metadata snapshot when the erasure log lives in MongoRescue.

## Contact

Questions about this policy: open an [issue](https://github.com/YigitCittan/mongorescue/issues). For security issues, see [SECURITY.md](../SECURITY.md).
