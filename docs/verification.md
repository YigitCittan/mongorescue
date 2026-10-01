# Verification, restore tests and retention safety

A backup is only worth something if it restores. MongoRescue collects evidence of that on four levels and makes retention safe and visible:

| Check | When | What it proves | Cost |
| :--- | :--- | :--- | :--- |
| [Post-backup verification](#post-backup-verification) | after every upload (on by default) | the stored object is exactly what was written | one read of the archive |
| [Integrity sweep](#integrity-sweep) and *Verify now* | on a schedule (off by default) or on demand | archives at rest have not changed since | one read of every archive |
| [Restore test](#automated-restore-tests) | per job, daily, weekly, monthly or every N backups | the archive restores, with the expected collections, document counts and indexes | one restore into a temporary database |
| [Storage scan](#storage-scans) | weekly (on by default) or on demand | storage and backup records agree | one listing per target |

Nothing in this page ever deletes an archive or touches a backup's source database.

## Post-backup verification

While a backup streams, MongoRescue computes the SHA-256 and the size of the bytes it hands to the storage driver (the age ciphertext for encrypted backups). After the upload it reads the stored object back, streaming it through SHA-256 into nothing, and compares both:

- **ok**: the record gets `verification: "ok"` and `verified_at`.
- **mismatch**: the object differs from what was written (a broken gateway, a storage bug, an object replaced meanwhile). The backup **fails** with *stored archive does not match its recorded checksum* (`backup.ErrChecksumMismatch`), the damaged object is deleted like any failed artifact, and the usual `backup.failed` notification goes out. The record keeps `verification: "mismatch"`.
- **error**: the object could not be read to the end (storage unreachable, cancellation). The backup stays completed, with `verification: "error"` and the reason in `verification_error`; nothing is known about the archive yet, and the next sweep or a *Verify now* tries again.

The setting `integrity.verify_after_backup` (Settings → Integrity, default on) applies to every backup; a job can override it with `verify_after_backup: "on"` or `"off"` (`""` follows the setting). With `integrity.verify_decrypt` an encrypted archive is also decrypted to its end, which authenticates every age chunk and proves that the keys under Settings → Encryption still open it; it needs the identity or passphrase there, otherwise only the checksum is compared.

Verification streams: memory stays constant whatever the archive size, and no temporary copy is written.

## Integrity sweep

The sweep re-verifies every completed backup, one at a time, starting with backups that were never verified (oldest first) and then those verified longest ago. A sweep that is interrupted (shutdown) records where it stopped; the next one starts again with the least recently verified backups.

| Setting | Default | Meaning |
| :--- | :--- | :--- |
| `integrity.sweep_schedule` | `off` | `off`, `daily`, `weekly` or `monthly` (30 days) after the previous sweep started |
| `integrity.sweep_bandwidth_limit` | `0` | Read limit in MiB/s, `0` is unlimited |

The sweep is off by default because it reads every archive back: on cloud storage that is egress, which providers bill. Use the bandwidth limit to keep it from saturating a link. A mismatch found by a sweep does not fail or delete the backup: its record keeps `status: "completed"` with `verification: "mismatch"`, the dashboard shows the red badge, a `verification.failed` event is published and the backup no longer counts as the job's last verified backup (see [retention](#never-the-last-verified-backup)). Restores of it fail on the checksum, as before.

*Verify now* (the backup's ⋯ menu, `POST /api/v1/backups/{id}/verify`, the MCP tool `verify_backup`) verifies one backup in the background; poll the backup for a new `verified_at`. Settings → Integrity shows the sweep status (running, last result, next run) and starts a sweep on demand (`POST /api/v1/integrity/sweep`, admin).

## Automated restore tests

A job's `restore_test` policy restores its latest backup into a temporary database after a successful **scheduled** backup, whenever a test is due:

```json
"restore_test": {"enabled": true, "frequency": "weekly", "connection_id": ""}
```

| Field | Meaning |
| :--- | :--- |
| `frequency` | `daily`, `weekly`, `monthly` (at most once per period, with an hour of slack for cron jitter) or `every_n` |
| `every_n` | With `every_n`: test after every N successful backups of the job (1 to 1000) |
| `connection_id` | A dedicated test server. Empty restores into the server the backup came from, into the temporary database only |

A test runs *Run restore test now* in the job details (`POST /api/v1/jobs/{id}/restore-test`, operator) as well, whatever the policy says. It

1. checks that the connection's user may create collections and indexes, insert, read and drop the temporary database `<db>_rescue_verify_<YYYYMMDD_HHMMSS>`. A user without those privileges fails the test with *the connection's user lacks the privileges a restore test needs*, naming the missing actions; grant `readWriteAnyDatabase` (or `restore` plus `dbAdminAnyDatabase`) or choose a test connection with such a user;
2. refuses to start when the temporary database already exists, and leaves it alone;
3. restores the backup into the temporary database (the streamed bytes are checked against the recorded checksum, as in every restore);
4. compares the restored copy with the **manifest** captured at backup time: every collection must exist, its document count must lie within the range captured before and after the dump, and every index must exist with the same keys, uniqueness, sparseness and TTL. Collections the restored copy has beyond the manifest (created during the dump) are noted, not counted as differences;
5. **drops the temporary database in every case**: success, failure, cancellation (shutdown) and even an internal panic. A drop that fails is reported on the result (`drop_error`) so the database can be dropped by hand.

The result (`ok`, `mismatch` with the differences, or `error` with the reason) and its duration are stored (`GET /api/v1/jobs/{id}/restore-tests`, the newest 200 per job) and copied onto the job and the backup (`last_restore_test`), which the dashboard shows as *Son restore testi: başarılı · 3 gün önce*. `restore_test.succeeded` and `restore_test.failed` events feed notifications and metrics.

The test never writes to the source database: the temporary name always differs from it, the restore is a safe clone into that name, and only that name is ever dropped. On a busy production server prefer a dedicated test connection, since the restore adds load.

### Manifest

Every backup captures a manifest through the MongoDB driver: per collection (views and `system.*` collections excluded) the `estimatedDocumentCount` and the index specifications, once before and once after the dump, so the count is a range when the collection changed while it was dumped. A manifest that cannot be captured (missing privileges, a timeout of two minutes) only logs a warning; restore tests of such a backup restore it and note that counts and indexes were not compared. Manifests are stored apart from the backup record (the record says `has_manifest`) and deleted with it.

## Retention

### Preview

`GET /api/v1/jobs/{id}/retention/preview` lists the backups the job's policy would delete if it ran now, oldest first, each with its rule (`max_age`: older than `retention_days`; `max_count`: beyond the `retention_count` newest), and the backups a rule selects but that are kept (`protected`: pinned or the last verified one). `?retention_days=` and `?retention_count=` preview other values without saving them; the job form uses them to show *Bu ayarla N yedek silinecek* while you edit. The preview and the actual pruning share one function, so the preview is exactly what the next prune deletes (that prune runs after the next scheduled backup, which adds one more backup to count). The MCP tool `retention_preview` returns the same.

### Pins (legal hold)

`POST /api/v1/backups/{id}/pin` with an optional `{"note": "..."}` (operator) puts a backup on legal hold: retention never deletes it and `DELETE /api/v1/backups/{id}` answers `409` until `POST /api/v1/backups/{id}/unpin`. The record keeps who pinned it, when and why; the dashboard shows a pin icon. Retention re-checks the pin in the same transaction that marks a backup pruned, so a pin set while retention runs is never overridden. The MCP tool `pin_backup` pins; unpinning is only offered in the dashboard and the REST API.

### Never the last verified backup

Retention keeps its existing floors (the `max(retention_count, 1)` newest scheduled backups, and no count-based pruning of backups younger than a day) and never deletes the job's newest backup whose archive passed verification, whatever the policy says. Until a job has a verified backup, the floors alone apply.

### Retention history

Every deletion by retention is recorded three times: in the retention log (`GET /api/v1/jobs/{id}/retention/log`, shown as *Saklama geçmişi* in the job details), in the audit log (tool `retention.delete`, transport `system`) and as a `retention.deleted` event. An archive that could not be deleted from storage is recorded with the error; the record is pruned anyway and the next storage scan lists the leftover object as an orphan.

## Storage scans

A scan lists every object of a storage target and compares the archives (keys ending in `.archive`, `.archive.gz`, optionally `.age`) with the backup records of that target:

- **Orphans** are archives without a live record (none at all, or a failed or pruned one that still names the key). *Import* (`POST /api/v1/storage-targets/{id}/import` with `{"key": "..."}`, admin) creates a record from the object: the database, backup ID and start time are read from the default key layout `<db>/<YYYY>/<MM>/<backup id>.archive[.gz][.age]` (other layouts keep the first path element as the database), the object is hashed in the background (the record is `pending` until then), and the record is marked `imported` and unverified. An imported backup has no source connection: restoring it needs an admin to choose the target connection. A failed import marks the record failed and detaches it from the key, so the archive stays an orphan and is never deleted through that record.
- **Missing** are completed records whose archive is gone. They are marked `status: "missing"` (with `missing_since`); the dashboard no longer offers to restore them and retention no longer counts them. A later scan that finds the archive again marks them completed.

Records of backups that completed after the listing started are never reported missing, and archives of running backups are never orphans. Nothing is deleted, ever.

Scans run weekly for every target (`integrity.storage_scan`, default on) and on demand (`POST /api/v1/storage-targets/{id}/scan`, admin; `GET` returns the latest result). Settings → Storage shows the latest scan per target with the orphans and missing archives, and the dashboard shows a warning while any target has drift. A scan with drift publishes `storage.drift_detected`.

## Notifications and metrics

| Event | Published when | Notes |
| :--- | :--- | :--- |
| `verification.failed` | a verification found a mismatch or could not run | after upload, in a sweep or on demand (`source`) |
| `restore_test.succeeded`, `restore_test.failed` | a restore test finished | `failed` covers differences and errors |
| `storage.drift_detected` | a scan found orphans or missing archives | `orphans`, `missing`, `target_id` |
| `retention.deleted` | retention deleted a backup | `detail` names the rule |

All of them can be selected in notification rules (see [notifications.md](notifications.md)). `verification.succeeded` is published for metrics only. The Prometheus series are listed in [metrics.md](metrics.md#metrics): `mongorescue_verifications_total`, `mongorescue_restore_tests_total`, `mongorescue_last_successful_restore_test_timestamp_seconds`, `mongorescue_storage_orphan_archives`, `mongorescue_storage_missing_archives`, `mongorescue_last_storage_scan_timestamp_seconds` and `mongorescue_retention_deletions_total`.
