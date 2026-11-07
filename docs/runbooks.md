# Incident runbooks

One section per alert of [`deploy/prometheus/alerts.yml`](../deploy/prometheus/alerts.yml). Every rule carries a `runbook_url` annotation that points at the heading of its alert here (`runbooks/#<alertname in lower case>`), so the page Alertmanager shows is the page that tells you what to do. A test (`go test ./internal/metrics`) fails when a rule has no `runbook_url` or its anchor is missing from this page.

Each runbook has the same parts: **Symptom** (what you see), **Likely causes**, **Diagnosis** (exact commands, API calls and PromQL), **Action** and **Escalation** (when to stop and who or what to involve). The [monitoring](monitoring.md#prometheus-alert-rules) page lists when each rule fires, [metrics](metrics.md) every series, and [production](production.md#service-level-objectives) the suggested SLOs.

## Before you start

The commands use these variables. The API key needs the `read` scope for diagnosis, `operator` to start backups and `admin` for scans, imports and sweeps ([scopes](api.md#api-key-scopes)).

```sh
export MR=https://backup.example.com          # the active instance (see GET /api/v1/health)
export KEY=...                                # an API key
mr() { curl -fsS -H "Authorization: Bearer $KEY" "$@"; }     # mr $MR/api/v1/stats | jq .
```

Responses use the envelope `{"success": ..., "data": ...}` ([api.md](api.md)), so the examples read `jq .data`; the fields named in the comments are the ones to look at.

- **Which instance.** In [HA mode](production.md#high-availability) only the active instance serves the API; `mr $MR/api/v1/health` tells you which one it is (`role`, `ha.holder`). On Kubernetes use `kubectl logs` of that pod.
- **Logs.** MongoRescue logs to standard output with `log/slog` (`-log-level=debug` for more). Every backup and restore also has a run log, redacted, at `<datadir>/logs/<id>.log`, readable with `mr "$MR/api/v1/backups/<id>/log?tail=200"` (`/restores/<id>/log` for restores).
- **The job label.** With a default scrape configuration Prometheus renames the MongoRescue job ID to `exported_job` (it stays `job` with `honor_labels: true`). The PromQL below uses `exported_job`; swap in `job` if your scrape keeps it.
- **Silence, do not delete.** Rules that fire on `increase(...)` clear on their own once the window passes; fix the cause, not the alert.

??? note "Alert index"

    | Group | Alerts |
    | :--- | :--- |
    | Process and scheduling | [MongoRescueDown](#mongorescuedown), [MongoRescueSchedulerStale](#mongorescueschedulerstale), [MongoRescueDiskFillingUp](#mongorescuediskfillingup) |
    | Backups and recovery objectives | [MongoRescueBackupFailed](#mongorescuebackupfailed), [MongoRescueRPOMissed](#mongorescuerpomissed), [MongoRescueVerificationFailed](#mongorescueverificationfailed), [MongoRescueArchivesMissing](#mongorescuearchivesmissing), [MongoRescueRestoreTestFailed](#mongorescuerestoretestfailed), [MongoRescueMetadataBackupFailed](#mongorescuemetadatabackupfailed), [MongoRescueSettingsWarning](#mongorescuesettingswarning) |
    | High availability | [MongoRescueHANoActiveInstance](#mongorescuehanoactiveinstance), [MongoRescueHATakeoverFailed](#mongorescuehatakeoverfailed), [MongoRescueHAStartedFromEmptyState](#mongorescuehastartedfromemptystate), [MongoRescueHARollbackSuspected](#mongorescueharollbacksuspected), [MongoRescueHASnapshotRejected](#mongorescuehasnapshotrejected) |
    | Point-in-time recovery | [MongoRescuePITRCollectorDown](#mongorescuepitrcollectordown), [MongoRescuePITRLagHigh](#mongorescuepitrlaghigh), [MongoRescuePITROplogHeadroomLow](#mongorescuepitroplogheadroomlow), [MongoRescuePITRChainBroken](#mongorescuepitrchainbroken), [MongoRescuePITRChunkErrors](#mongorescuepitrchunkerrors), [MongoRescuePITRStale](#mongorescuepitrstale) |
    | Copies | [MongoRescueBackupCopyFailed](#mongorescuebackupcopyfailed), [MongoRescueBackupCopyMismatch](#mongorescuebackupcopymismatch), [MongoRescueBackupCopyQueueGrowing](#mongorescuebackupcopyqueuegrowing) |
    | Notifications, events, audit, keys | [MongoRescueNotificationFailures](#mongorescuenotificationfailures), [MongoRescueNotificationsLost](#mongorescuenotificationslost), [MongoRescueEventsDropped](#mongorescueeventsdropped), [MongoRescueHeartbeatsDropped](#mongorescueheartbeatsdropped), [MongoRescueAuditWriteFailed](#mongorescueauditwritefailed), [MongoRescueAuditForwardFailed](#mongorescueauditforwardfailed), [MongoRescueKeyWrapErrors](#mongorescuekeywraperrors), [MongoRescueAPIKeysExpired](#mongorescueapikeysexpired), [MongoRescueAPIKeysExpiring](#mongorescueapikeysexpiring) |

## Process and scheduling

### MongoRescueDown { #mongorescuedown }

**Symptom.** `up == 0` for 5 minutes on a target that exposed `mongorescue_build_info` in the last 7 days. No backup runs, and MongoRescue cannot alert about itself.

**Likely causes.**

- The process crashed or was OOM-killed (a large dump with too little memory, see [sizing](production.md#sizing)), or the pod or host is gone.
- The container is restarting in a loop: a wrong `secret.key` (`the secret key does not match the key this database was encrypted with`), a locked data directory (`mongorescue.lock` held by another process) or a database newer than the binary (`ErrSchemaTooNew`).
- Only the scrape fails: an expired or revoked API key (`401`), a network policy, a changed TLS certificate.

**Diagnosis.**

```sh
curl -sS -o /dev/null -w '%{http_code}\n' $MR/api/v1/health          # 200 = process answers; 503 = scheduler stale
kubectl get pod -l app.kubernetes.io/name=mongorescue                 # restarts, last state (OOMKilled?)
kubectl describe pod <pod> | grep -A5 'Last State'
kubectl logs <pod> --previous | tail -50                              # or: docker logs --tail 50 mongorescue
```

```promql
up{job="mongorescue"} == 0
changes(process_start_time_seconds[1h])           # restarts
increase(kube_pod_container_status_restarts_total{container="mongorescue"}[1h])
```

If `/api/v1/health` answers but the scrape fails, test the scrape the way Prometheus does: `curl -H "Authorization: Bearer $KEY" $MR/metrics | head`.

**Action.**

1. `OOMKilled`: raise the memory limit and set `config.goMemLimitRatio` ([sizing](production.md#sizing)); lower job `parallelism`, `num_parallel_collections` or the S3 part size.
2. Startup refusals: restore the original `secret.key` or `MONGORESCUE_SECRET_KEY`; stop the other process holding the data directory; use a release at least as new as the database.
3. Scrape-only failures: create a new `read` API key and update the scrape credentials.
4. After the restart, run the jobs whose run was missed (`mr -X POST $MR/api/v1/jobs/<id>/run`); interrupted runs are failed at startup and `MongoRescueBackupFailed` may fire once.

**Escalation.** The host or node is gone and the data volume with it: [restore MongoRescue from a metadata snapshot](production.md#restore-mongorescue-from-a-snapshot). In HA mode the standby takes over by itself; if it did not, go to [MongoRescueHANoActiveInstance](#mongorescuehanoactiveinstance). The external heartbeat is the second line of defence: page on it as well.

### MongoRescueSchedulerStale { #mongorescueschedulerstale }

**Symptom.** The scheduler's last liveness tick is older than 90 seconds for 2 minutes, `GET /api/v1/health` answers `503` with `"scheduler": "stale"`, the heartbeat pings stop. Scheduled backups do not start. A wall clock step does not cause this ([clock steps](monitoring.md#clock-steps)).

**Likely causes.** A deadlock or a hung database call in the scheduler, or a process starved of CPU or memory (swapping, a CPU limit far below what `mongodump` compression needs).

**Diagnosis.**

```sh
mr $MR/api/v1/health | jq .
kubectl top pod <pod>                       # CPU throttled at its limit? memory near the limit?
kubectl logs <pod> --since=15m | tail -100
```

```promql
time() - mongorescue_scheduler_last_tick_timestamp_seconds
rate(container_cpu_cfs_throttled_periods_total{container="mongorescue"}[5m]) / rate(container_cpu_cfs_periods_total{container="mongorescue"}[5m])
go_goroutines
```

**Action.** Restart MongoRescue (`kubectl rollout restart statefulset/<name>`, `systemctl restart mongorescue`). A graceful stop waits `MONGORESCUE_SHUTDOWN_GRACE` for runs; a hung process is killed after the grace period and its runs are failed at the next start. Then run the jobs that were due. If CPU throttling or memory pressure is the cause, raise the limits ([sizing](production.md#sizing)).

**Escalation.** A restart that does not clear it, or a repeat within a day: collect the goroutine dump (`kill -QUIT <pid>` prints it to the log, which also stops the process) and the last 15 minutes of logs, and open an issue.

### MongoRescueDiskFillingUp { #mongorescuediskfillingup }

**Symptom.** `predict_linear` over the last 6 hours says the data directory (`mongorescue_data_dir_free_bytes`) or a local storage target (`mongorescue_storage_free_bytes{target}`) has no free space within 24 hours. Below `MONGORESCUE_MIN_FREE_SPACE_MB` (100 MiB) runs refuse to start; a full disk also raises `system.disk_full` ([disk space](production.md#disk-space)).

**Likely causes.** Local archives grow faster than retention removes them; run logs and the metadata database on a small data volume; copies or imports landing on a local target; a retention rule that excludes pinned or last-verified backups; an unused volume that was never sized ([sizing](production.md#sizing)).

**Diagnosis.**

```promql
mongorescue_data_dir_free_bytes
mongorescue_storage_free_bytes
predict_linear(mongorescue_storage_free_bytes[6h], 24 * 3600)
sum by (exported_job) (mongorescue_backup_size_bytes)        # size of the last archive per job
increase(mongorescue_retention_deletions_total[7d])          # is retention deleting anything?
```

```sh
du -sh /data/logs /data/mongorescue.db* ; du -sh /backups/*       # in the container: kubectl exec <pod> -- du -sh ...
mr "$MR/api/v1/jobs/<id>/retention/preview"                      # what retention would delete next
mr "$MR/api/v1/backups?status=deleted&limit=50"                  # deleted backups waiting for their purge (grace period)
```

**Action.**

1. Free space where it is cheapest: shorten `retention_days` / `retention_count` of the jobs on that target, unpin backups that no longer need a legal hold, lower `general.log_retention_days`.
2. Move archives to an S3 target (new jobs write there; old archives age out under retention) or grow the volume (`persistence.size`, `backups.persistence.size`, or the PVC).
3. Do not delete archives by hand: the records still point at them and the next scan reports them as missing ([MongoRescueArchivesMissing](#mongorescuearchivesmissing)).

**Escalation.** Less than a day of space left and nothing to free: grow the volume now; a backup that cannot be saved fails ([disk space](production.md#disk-space)) and the RPO suffers within hours.

## Backups and recovery objectives

### MongoRescueBackupFailed { #mongorescuebackupfailed }

**Symptom.** A backup (or a whole job run before any backup started) failed in the last hour. `job="manual"` is an on-demand backup.

**Likely causes.**

- The connection fails: credentials rotated, network policy, TLS, the member chosen by the read preference is unreachable (`secondary` with none available).
- The storage target fails: expired S3 keys, a full or read-only disk, a bucket policy change.
- The process was killed: an OOMKill appears as a failed backup, with the run log ending abruptly or empty ([sizing](production.md#sizing)).
- `mongodump` failed on a large or busy database, the backup timeout was reached, or a [sharded cluster](production.md#sharded-clusters-mongos) is behind the connection.
- The data directory is full ([MongoRescueDiskFillingUp](#mongorescuediskfillingup)).

**Diagnosis.**

```sh
mr "$MR/api/v1/backups?status=failed&sort=-started_at&limit=5" | jq .data
mr "$MR/api/v1/backups/<id>/log?tail=200"                        # the redacted mongodump output and the phase lines
mr $MR/api/v1/jobs/<job id>/runs | jq .data                     # per-database outcome of the last run
kubectl describe pod <pod> | grep -B2 -A6 'Last State'          # OOMKilled, exit code 137
```

```promql
increase(mongorescue_backups_total{status="failed"}[1h]) > 0
increase(mongorescue_job_runs_total{status="failed"}[1h]) > 0
increase(kube_pod_container_status_last_terminated_reason{reason="OOMKilled",container="mongorescue"}[1h])
```

**Action.**

1. Read the error and the log tail; fix the connection or storage (`POST /api/v1/connections/<id>/test`, `POST /api/v1/storage-targets/<id>/test`).
2. OOMKilled: raise the memory limit, set `config.goMemLimitRatio`, lower `parallelism` and `num_parallel_collections` ([sizing](production.md#sizing)).
3. Retry: `mr -X POST $MR/api/v1/backups/<id>/retry` (or the dashboard's *Retry*). Check `MongoRescueRPOMissed` clears.

**Escalation.** Two consecutive failures of one job, or any failure of a job whose RPO is close: treat it as [MongoRescueRPOMissed](#mongorescuerpomissed) in the making and run a manual backup now. A failure that repeats with a healthy connection and storage: attach the run log (already redacted) to an issue.

### MongoRescueRPOMissed { #mongorescuerpomissed }

**Symptom.** `mongorescue_job_rpo_met == 0` for 5 minutes: the newest successful backup of a database is older than its job's recovery point objective. You cannot restore data newer than that.

**Likely causes.** Backups fail ([MongoRescueBackupFailed](#mongorescuebackupfailed)); the scheduler is stale or MongoRescue is down; the job is paused and was just resumed (resuming alerts right away, see [RPO and RTO](production.md#rpo-and-rto)); a backup window or cron schedule that leaves a gap longer than the RPO; a backup that runs longer than the interval and is skipped; the RPO was set tighter than the schedule.

**Diagnosis.**

```promql
mongorescue_job_rpo_met == 0
mongorescue_job_rpo_seconds / mongorescue_job_rpo_target_seconds        # > 1 = missed; how far
time() - mongorescue_database_last_successful_backup_timestamp_seconds
mongorescue_job_runs_total{status="skipped"}                            # skipped outside the backup window
```

```sh
mr $MR/api/v1/readiness | jq .data    # per database: last good backup, RPO age, reasons
mr $MR/api/v1/jobs/<id> | jq .data
mr $MR/api/v1/runs/active                                                # a stuck or long run?
```

**Action.**

1. Take a backup now so the RPO is met again: `mr -X POST $MR/api/v1/jobs/<id>/run`.
2. Find why the last scheduled runs failed or did not start (a paused job, a closed window, a stale scheduler, an overlong run) and fix that.
3. If the objective was unrealistic, change the job's schedule or its `rpo_minutes`, never silence the alert.

**Escalation.** A critical production database more than one RPO behind with no working backup path: take a backup by another means (a snapshot, a manual `mongodump`) while MongoRescue is repaired, and tell the data owners which point in time is currently recoverable.

### MongoRescueVerificationFailed { #mongorescueverificationfailed }

**Symptom.** A verification (`source`: `after_upload`, `sweep` or `on_demand`) found a `mismatch` (the archive no longer matches its recorded checksum) or an `error` (it could not be read).

**Likely causes.** `mismatch`: bit rot or a damaged volume, an object overwritten or truncated on the target, an interrupted upload, a lifecycle rule or another tool touching the bucket. `error`: expired storage credentials, a missing key for an encrypted archive, a network failure during the read.

**Diagnosis.**

```promql
sum by (source, result) (increase(mongorescue_verifications_total{result=~"mismatch|error"}[24h]))
```

```sh
mr $MR/api/v1/integrity | jq .                                    # sweep status and findings
mr "$MR/api/v1/backups?limit=50&sort=-started_at" | jq .data
mr "$MR/api/v1/backups/<id>/log?tail=100"
```

**Action.**

1. For `error`, test the storage target and credentials, then re-verify: `mr -X POST $MR/api/v1/backups/<id>/verify` (or `mongorescue verify <id> --wait`).
2. For `mismatch`, never rely on that archive. If the backup has a copy ([copies](production.md#3-2-1-backups)), restore tests and restores fall back to a healthy copy and the sweep copies it again; otherwise take a new backup now.
3. Find the cause: bucket versioning history, who can write to the bucket, the disk's SMART and filesystem errors.

**Escalation.** A mismatch on an archive with no healthy copy and no newer good backup is a recovery gap: take a new backup immediately and treat it as an [RPO](#mongorescuerpomissed) risk. Repeated mismatches on one storage target mean the target is not trustworthy: move jobs off it.

### MongoRescueArchivesMissing { #mongorescuearchivesmissing }

**Symptom.** The last storage scan did not find the archives of some backup records (`mongorescue_storage_missing_archives{target} > 0`). Those records are marked `missing`; the dashboard no longer offers to restore them and retention stops counting them.

**Likely causes.** Someone or something deleted objects (a lifecycle rule, manual cleanup, a bucket replaced or re-created); a wrong prefix or an unmounted local directory after a change; archives written under the old layout; a purge by retention that raced a scan.

**Diagnosis.**

```promql
mongorescue_storage_missing_archives > 0
time() - mongorescue_last_storage_scan_timestamp_seconds             # when the scan ran
```

```sh
mr $MR/api/v1/storage-targets/<id>/scan | jq .data
mr "$MR/api/v1/backups?status=missing&limit=100" | jq .data
aws s3 ls s3://<bucket>/<prefix>/<database>/ --recursive | head     # is the object really gone?
```

A scan that finds no archive at all while completed backups are recorded fails instead of marking them missing, so a missing count is real drift, not an unmounted directory.

**Action.**

1. Restore the objects if you can (bucket versioning, the provider's undelete, a copy target). Run `mr -X POST $MR/api/v1/storage-targets/<id>/scan`: records whose archive is back are completed again.
2. If the archive exists under another key, import it: `POST /api/v1/storage-targets/<id>/import` with `{"key": "..."}` (admin).
3. Where backups have copies, restores already fall back to the copy; check them under the backup's `copies`.
4. Find and fix what deleted them (lifecycle rules, an over-broad credential) and take a new backup of the affected databases.

**Escalation.** Backups gone with no copy: this is data loss in the backup system. Check the [RPO](#mongorescuerpomissed) of every affected database, take new backups at once, and treat the credential or process that deleted them as a security incident.

### MongoRescueRestoreTestFailed { #mongorescuerestoretestfailed }

**Symptom.** An automated restore test ended with `mismatch` (the restored database differs from the manifest) or `error` in the last 24 hours. The backup was readable, but the proof that it can be restored failed.

**Likely causes.** `error`: the scratch MongoDB is unreachable or out of disk, an encrypted archive's key is missing, the target user lacks privileges, a timeout on a large database (the test needs time of the order of your RTO). `mismatch`: collections or documents differ, usually because documents changed between the dump and the manifest (a busy collection), or a filtered backup, or a damaged archive.

**Diagnosis.**

```promql
sum by (exported_job, result) (increase(mongorescue_restore_tests_total{result=~"mismatch|error"}[24h]))
time() - mongorescue_last_successful_restore_test_timestamp_seconds
```

```sh
mr $MR/api/v1/jobs/<id>/restore-tests | jq .data            # result, differences, duration, values it ran with
mr "$MR/api/v1/restores?limit=5&sort=-started_at" | jq .
mr "$MR/api/v1/restores/<id>/log?tail=200"
```

**Action.** Fix what the result names (target connection and privileges, disk, key), then run the test again: `mr -X POST $MR/api/v1/jobs/<id>/restore-test`. For a mismatch, verify the archive ([MongoRescueVerificationFailed](#mongorescueverificationfailed)) and compare counts of the busy collections; a checksum-clean archive that cannot restore is the case restore tests exist for.

**Escalation.** Two failed tests in a row with a clean checksum: assume the backups of that job are not restorable until proven otherwise, restore the newest one by hand into a scratch cluster, and open an issue with the restore log.

### MongoRescueMetadataBackupFailed { #mongorescuemetadatabackupfailed }

**Symptom.** A scheduled snapshot of MongoRescue's own database (`mongorescue.db`: jobs, backup records, users, settings) failed in the last 24 hours. Without a recent snapshot, losing the data directory loses the records and settings ([metadata backups](production.md#metadata-backups)).

**Likely causes.** The snapshot target is unreachable or its credentials expired; the data directory has no room for the temporary `VACUUM INTO` copy; the encryption settings reject the snapshot (no recipient configured).

**Diagnosis.**

```promql
increase(mongorescue_metadata_backups_total{result="error"}[24h])
time() - mongorescue_last_successful_metadata_backup_timestamp_seconds
```

```sh
mr $MR/api/v1/metadata-backup | jq .                      # last result, error, prefix, next attempt
df -h /data
```

**Action.** Fix the target or free space, then run a snapshot now: `mr -X POST $MR/api/v1/metadata-backup/run`. A failed snapshot is retried after an hour on its own.

**Escalation.** No snapshot for more than a day on an instance whose host is not otherwise protected: also download a fresh [recovery kit](production.md#recovery-kit) and copy `mongorescue.db` with `sqlite3 .backup` until snapshots work again.

### MongoRescueSettingsWarning { #mongorescuesettingswarning }

**Symptom.** A settings warning has been active for an hour (`mongorescue_settings_warnings > 0`): backup encryption off, metadata backups unencrypted, a recovery kit missing or outdated, a kept administrator role, or `data_dir_full`.

**Diagnosis.**

```sh
mr $MR/api/v1/settings | jq .data          # id, message and what resolves each
```

**Action.** Resolve what each warning names: enable encryption (Settings → Encryption), download a [recovery kit](production.md#recovery-kit) (Settings → Recovery), turn metadata-backup encryption on. A warning you accept can be dismissed (`POST /api/v1/settings/warnings/<id>/dismiss`) for the ones that allow it. `data_dir_full` is [MongoRescueDiskFillingUp](#mongorescuediskfillingup) already happening.

**Escalation.** None, unless it is `data_dir_full` or a missing recovery kit on an instance with no metadata backups: both lose data if the host is lost.

## High availability

All HA runbooks assume [active-passive mode](production.md#high-availability). The design and failure matrix are in [design/ha.md](design/ha.md).

### MongoRescueHANoActiveInstance { #mongorescuehanoactiveinstance }

**Symptom.** No instance of an HA cluster has reported `role="active"` for a minute, although one exported `mongorescue_ha_role` in the last 10 minutes. Scheduled backups and PITR collection do not run. With the default 30 second TTL a normal failover takes about 40 seconds, so a firing alert means the takeover is not happening.

**Likely causes.** Both instances cannot reach the HA target (credentials, network, bucket policy); every standby refuses the takeover ([HATakeoverFailed](#mongorescuehatakeoverfailed), [HARollbackSuspected](#mongorescueharollbacksuspected), [HASnapshotRejected](#mongorescuehasnapshotrejected)); the instances use different `MONGORESCUE_SECRET_KEY` values; all instances are down (then [MongoRescueDown](#mongorescuedown) fires too).

**Diagnosis.**

```sh
for h in mongorescue-0.example.com mongorescue-1.example.com; do curl -sS https://$h/api/v1/health | jq .; done
kubectl logs <standby pod> --since=10m | grep -i -e lease -e takeover -e snapshot
aws s3 ls s3://<ha bucket>/<prefix>/_mongorescue/ha/<cluster>/ ; aws s3 ls s3://<ha bucket>/<prefix>/_mongorescue/metadata/ha/<cluster>/
```

```promql
mongorescue_ha_lease_healthy == 0
increase(mongorescue_ha_lease_errors_total[10m])
mongorescue_ha_role{role="active"} == 1
increase(mongorescue_ha_takeover_failures_total[1h]) + increase(mongorescue_ha_rollback_suspected_total[1h])
```

**Action.**

1. Lease unreachable: fix the credentials, network or bucket; the standby takes over by itself once it can read and write the lease (`If-None-Match` conditional writes must work on the HA target).
2. A refusing standby: follow the runbook of the alert that fires with this one.
3. After a takeover, check that the next scheduled run starts on the new active instance and that the old one comes back as standby.

**Escalation.** No active instance for more than 15 minutes with the HA target healthy: after checking the target (who can write to it, which snapshots and lease exist) force the takeover on the instance that should win, `mongorescue ha force-takeover` ([forcing a takeover](design/ha.md#forcing-a-takeover)). Meanwhile start a manual backup on the surviving instance if its RPO is at risk.

### MongoRescueHATakeoverFailed { #mongorescuehatakeoverfailed }

**Symptom.** A standby acquired the lease but found no usable metadata snapshot to restore, and stayed standby (`mongorescue_ha_takeover_failures_total` grew).

**Likely causes.** The snapshots under `_mongorescue/metadata/ha/<cluster>/` were never written (a brand-new cluster that failed over before the first snapshot), were deleted, or cannot be read by this instance: a different `MONGORESCUE_SECRET_KEY`, a snapshot of a newer version (upgrade the standby first), or storage errors.

**Diagnosis.**

```sh
kubectl logs <standby pod> --since=15m | grep -i -e takeover -e snapshot
aws s3 ls s3://<ha bucket>/<prefix>/_mongorescue/metadata/ha/<cluster>/
mr $MR/api/v1/health | jq .          # on the standby
```

**Action.** Check that every instance has the same secret key and a version at least as new as the active one. If the snapshots are gone, check who can delete on the HA target. Once a readable snapshot exists the next attempt succeeds. If the active instance is still running, the next snapshot it writes (at the latest every 5 minutes) fixes a missing one.

**Escalation.** No snapshot exists and the previous active instance is gone: start it again if you can (it writes a snapshot as soon as it is active); otherwise `mongorescue ha force-takeover --keep-local-database` on the standby with the most complete data directory, accepting that metadata written since its last takeover is lost, then run a storage scan of every target to re-import archives.

### MongoRescueHAStartedFromEmptyState { #mongorescuehastartedfromemptystate }

**Symptom.** An instance found no lease and no snapshot on the HA target and became active with its own database. Expected exactly once for a new cluster.

**Likely causes.** A new cluster (nothing to do). Otherwise the HA target was emptied or replaced (a deleted prefix, a re-created bucket, a wrong `MONGORESCUE_HA_TARGET` or `MONGORESCUE_HA_CLUSTER`), so the instance is running with only its local database.

**Diagnosis.**

```sh
kubectl logs <pod> --since=30m | grep -i -e empty -e lease
aws s3 ls s3://<ha bucket>/<prefix>/_mongorescue/ --recursive | head
mr $MR/api/v1/jobs | jq '.data | length'                 # are the jobs you expect there?
```

```promql
increase(mongorescue_ha_empty_starts_total[1d])
```

**Action.** If it is a new cluster, no action. If not: confirm the HA target and cluster name in the configuration of every instance, restore the objects from bucket versioning if they were deleted, and run a storage scan of every storage target (`POST /api/v1/storage-targets/<id>/scan`, then import) to re-import archives the instance does not know. If this instance ran with an old database, jobs and records written after it may be missing: compare them with the other instance's data directory before you remove that.

**Escalation.** Someone deleted the HA prefix: treat it as a security incident (who holds write access to the HA target) and rotate its credentials.

### MongoRescueHARollbackSuspected { #mongorescueharollbacksuspected }

**Symptom.** A standby refused to take over because restoring the snapshots left on the HA target could roll the metadata back (revoked API keys and removed users would return, records would be lost). The `reason` label says which check failed (`lease_untrusted`, `lease_missing`, `snapshot_behind_lease`, `snapshot_behind_local`, `legacy_after_upgrade`, `lease_replayed`, `lease_stale`, see [rollback protection](design/ha.md#rollback-protection)). No instance becomes active until an operator acts, so this usually fires together with [MongoRescueHANoActiveInstance](#mongorescuehanoactiveinstance).

**Likely causes.** Newest snapshots or the lease were deleted or replaced on the HA target; an old lease was put back; a double failure in the middle of an upgrade ([upgrading from 0.27](production.md#high-availability)); a restored bucket that is older than the instances' memory.

**Diagnosis.**

```sh
kubectl logs <standby pod> --since=15m | grep -i rollback
aws s3api list-object-versions --bucket <ha bucket> --prefix <prefix>/_mongorescue/ha/<cluster>/    # who changed the lease and when
aws s3 ls s3://<ha bucket>/<prefix>/_mongorescue/metadata/ha/<cluster>/
```

```promql
increase(mongorescue_ha_rollback_suspected_total[1h])
```

Decide with the `reason`: after a legitimate upgrade or a bucket restore the data you would lose is known; after unexplained deletions it is not.

**Action.** After checking the HA target, force the takeover on the instance that should become active, on its host:

```sh
mongorescue ha force-takeover --from-snapshot _mongorescue/metadata/ha/<cluster>/mongorescue-<time>Z-e<epoch>.db.age
# or keep this instance's own database:
mongorescue ha force-takeover --keep-local-database
```

The next takeover (within the lease TTL) uses the request once. It is logged as an error, audited and sent to every channel as `security.destructive_action` ([details](design/ha.md#forcing-a-takeover)). Afterwards run a storage scan of every target to import archives written after the chosen snapshot.

**Escalation.** Unexplained deletions or a replaced lease mean someone with write access to the HA target did it: investigate and rotate that credential before you force the takeover.

### MongoRescueHASnapshotRejected { #mongorescuehasnapshotrejected }

**Symptom.** A metadata snapshot under `_mongorescue/metadata/ha/<cluster>/` was refused (`mongorescue_ha_refused_snapshots > 0`, or a rejection in the last 15 minutes): its authenticated header does not match its name or cluster, or it does not decrypt. It is never restored and never deleted, so the alert lasts until you delete it.

**Likely causes.** A snapshot was copied or renamed on the HA target (an old one under a higher epoch, one of another cluster sharing the key), altered, or written with another secret key.

**Diagnosis.**

```sh
kubectl logs <pod> --since=1h | grep -i -e rejected -e refused      # the log names the object key
aws s3api list-object-versions --bucket <ha bucket> --prefix <prefix>/_mongorescue/metadata/ha/<cluster>/
```

```promql
mongorescue_ha_refused_snapshots > 0
increase(mongorescue_ha_snapshots_rejected_total[1h])
```

**Action.** Find out who wrote the object (bucket logs or CloudTrail), rotate the HA target's credentials if the writer was not MongoRescue, then delete the refused object ([security](design/ha.md#security)). Check that the cluster still has a valid newest snapshot.

**Escalation.** Treat a snapshot you cannot explain as tampering with the HA target: involve security, and do not force a takeover from it.

## Point-in-time recovery

See [PITR](pitr.md) for what a stream, a base backup and a chunk are. A stream's series carry the label `stream`.

### MongoRescuePITRCollectorDown { #mongorescuepitrcollectordown }

**Symptom.** `mongorescue_pitr_collector_up == 0` for 10 minutes: the oplog collector of a stream stores no chunks (it fails or is stopped). The recovery window stops growing and the oplog headroom shrinks.

**Likely causes.** The MongoDB connection fails (credentials, a primary election in progress that does not settle, a network change); the storage target is unreachable or rejects writes; encryption of the chunks fails; the stream was disabled; the replica set was re-initiated.

**Diagnosis.**

```sh
mr $MR/api/v1/pitr/streams | jq .data
mr $MR/api/v1/pitr/streams/<id>
mr "$MR/api/v1/pitr/streams/<id>/chunks?limit=5"
mr -X POST $MR/api/v1/connections/<connection id>/test
```

```promql
mongorescue_pitr_collector_up == 0
mongorescue_pitr_oplog_headroom_seconds                      # time left before entries are lost
increase(mongorescue_pitr_chunks_total{result="error"}[1h])
```

**Action.** Fix the connection or the storage target named by the collector error; the collector restarts by itself. Check `enabled`. Watch the headroom while you work: if it nears zero, grow the oplog (`replSetResizeOplog`) to buy time.

**Escalation.** When the headroom runs out the chain breaks ([MongoRescuePITRChainBroken](#mongorescuepitrchainbroken)) and only a new base backup restarts the window. Do not wait on a stream whose headroom is under an hour.

### MongoRescuePITRLagHigh { #mongorescuepitrlaghigh }

**Symptom.** A stream's lag (the replica set's newest write minus the end of the last stored chunk) is above 15 minutes for 10 minutes. Point-in-time restores cannot reach the newest writes yet.

**Likely causes.** A slow or throttled storage target, a write burst larger than the collector keeps up with, an undersized MongoRescue (CPU for compression and encryption), a slow network to MongoDB.

**Diagnosis.**

```promql
mongorescue_pitr_lag_seconds
rate(mongorescue_pitr_chunk_bytes_total[10m])               # bytes per second stored
mongorescue_pitr_oplog_headroom_seconds
rate(container_cpu_usage_seconds_total{container="mongorescue"}[5m])
```

```sh
mr $MR/api/v1/pitr/streams/<id> | jq .
```

**Action.** Check the storage target's latency and throughput and the CPU of the pod ([sizing](production.md#sizing)); remove an upload cap that covers the collector. The lag shrinks on its own once the collector catches up. Watch the headroom: it is the real deadline.

**Escalation.** A lag that grows for hours while the headroom falls: treat it as [MongoRescuePITROplogHeadroomLow](#mongorescuepitroplogheadroomlow).

### MongoRescuePITROplogHeadroomLow { #mongorescuepitroplogheadroomlow }

**Symptom.** The oplog headroom (time between the oldest oplog entry and the collector's position) is below max(6 hours, 3 times the lag) for 10 minutes. If the collector stops for that long, entries are lost and the recovery window breaks.

**Likely causes.** A small oplog on a write-heavy replica set, a collector that lags or is down, a bulk write or migration that spins the oplog fast.

**Diagnosis.**

```promql
mongorescue_pitr_oplog_headroom_seconds
mongorescue_pitr_lag_seconds
mongorescue_pitr_collector_up
```

```js
// mongosh, on the replica set
rs.printReplicationInfo()          // oplog size and the time it spans (log length start to end)
```

**Action.** Grow the oplog (`db.adminCommand({replSetResizeOplog: 1, size: <MB>})` on every member) so that it spans well over a day of peak writes; fix the lag or the collector ([CollectorDown](#mongorescuepitrcollectordown), [LagHigh](#mongorescuepitrlaghigh)). The alert clears when the headroom exceeds the bound again.

**Escalation.** Headroom under one hour with the collector down: act now. Resize the oplog first, restore the collector second.

### MongoRescuePITRChainBroken { #mongorescuepitrchainbroken }

**Symptom.** A stream's oplog chain ended in the last hour (`reason`: `gap`, `replica_set_changed`, `diverged`). Restores cannot cross the break; the window before it stays restorable up to the break, and a new base backup starts the next window.

**Likely causes.** `gap`: the collector fell behind the oplog (see headroom). `replica_set_changed`: the replica set was re-initiated or the connection now points at another set. `diverged`: a rollback after a failover removed entries the collector had already stored.

**Diagnosis.**

```promql
sum by (stream, reason) (increase(mongorescue_pitr_chain_breaks_total[1h])) > 0
mongorescue_pitr_window_end_timestamp_seconds - mongorescue_pitr_window_start_timestamp_seconds   # current window length
```

```sh
mr $MR/api/v1/pitr/streams/<id> | jq .
mr $MR/api/v1/readiness | jq .data
```

**Action.** Take a new base backup so a new chain starts: `mr -X POST $MR/api/v1/pitr/streams/<id>/base`. Then fix the reason: for `gap` grow the oplog and fix the collector; for `diverged` check the failover history of the set; for `replica_set_changed` confirm the connection is the intended one.

**Escalation.** Until the new base completes, point-in-time recovery of that stream stops at the break: tell the data owners the exact window you can restore ([PITR runbook](pitr.md#runbook-restore-to-the-minute-before-an-accident)).

### MongoRescuePITRChunkErrors { #mongorescuepitrchunkerrors }

**Symptom.** At least 3 oplog chunks of a stream failed in the last hour (storage target or encryption errors). The chunks are read again, so nothing is lost yet.

**Likely causes.** Storage errors or throttling, expired credentials, an encryption setting that cannot encrypt (missing recipient), a full target.

**Diagnosis.**

```promql
sum by (stream) (increase(mongorescue_pitr_chunks_total{result="error"}[1h]))
mongorescue_pitr_lag_seconds
```

```sh
mr "$MR/api/v1/pitr/streams/<id>/chunks?limit=10" | jq .
mr -X POST $MR/api/v1/storage-targets/<target id>/test
```

**Action.** Fix the target or the credentials; the collector retries. If errors continue the lag grows ([PITRLagHigh](#mongorescuepitrlaghigh)) and the headroom shrinks.

**Escalation.** Errors continuing for more than an hour with a falling headroom: go to [MongoRescuePITROplogHeadroomLow](#mongorescuepitroplogheadroomlow).

### MongoRescuePITRStale { #mongorescuepitrstale }

**Symptom.** The newest chunk of a stream ends more than an hour ago while the replica set keeps writing (the lag is above 60 seconds), for 10 minutes. An idle replica set (lag 0) never triggers this.

**Likely causes.** The collector is stuck without erroring, the stream is paused, or a hung storage call.

**Diagnosis.**

```promql
time() - mongorescue_pitr_last_chunk_timestamp_seconds
mongorescue_pitr_lag_seconds
mongorescue_pitr_collector_up
```

```sh
mr $MR/api/v1/pitr/streams/<id> | jq .data
kubectl logs <pod> --since=1h | grep -i pitr
```

**Action.** Confirm the stream is `enabled`, test the connection and the target, and restart MongoRescue if the collector shows no error but stores nothing (the collector restarts with the process).

**Escalation.** Treat it like [MongoRescuePITRCollectorDown](#mongorescuepitrcollectordown) when the headroom is shrinking.

## Copies

See [3-2-1 backups](production.md#3-2-1-backups). A copy is a byte-for-byte duplicate of an archive on another storage target.

### MongoRescueBackupCopyFailed { #mongorescuebackupcopyfailed }

**Symptom.** Attempts to copy a backup to its copy target failed in the last hour (`mongorescue_backup_copies_total{result="error"}`). Failed copies are retried with a backoff.

**Likely causes.** The copy target is unreachable, its credentials expired, its Object Lock or bucket policy rejects the write, a full local target, or the copy bandwidth cannot keep up.

**Diagnosis.**

```promql
increase(mongorescue_backup_copies_total{result="error"}[1h])
mongorescue_backup_copy_queue_depth
```

```sh
mr "$MR/api/v1/backups?limit=20&sort=-started_at" | jq .data
mr -X POST $MR/api/v1/storage-targets/<copy target id>/test
```

**Action.** Fix the target (credentials, policy, space); copies are retried automatically, and the integrity sweep copies a missing one again.

**Escalation.** A backup without a done copy six hours after it finished raises the readiness warning `copy_missing`; if the primary is also at risk, the 3-2-1 guarantee does not hold: get a copy made before anything else changes.

### MongoRescueBackupCopyMismatch { #mongorescuebackupcopymismatch }

**Symptom.** A copy did not match the checksum of its primary archive in the last hour. The 3-2-1 guarantee does not hold for that backup.

**Likely causes.** A damaged transfer or storage, an object changed on the copy target, or the primary itself is damaged and the copy is correct.

**Diagnosis.**

```promql
increase(mongorescue_backup_copies_total{result="mismatch"}[1h])
```

```sh
mr "$MR/api/v1/backups/<id>" | jq .data
mr -X POST $MR/api/v1/backups/<id>/verify                           # which side is damaged?
```

**Action.** Verify the primary and the copy. If the primary is intact, the sweep copies the damaged one again (or delete the bad copy and let the next attempt run). If the primary is damaged, see [MongoRescueVerificationFailed](#mongorescueverificationfailed).

**Escalation.** Repeated mismatches on one copy target: do not trust it; move the copy to another target and investigate the storage.

### MongoRescueBackupCopyQueueGrowing { #mongorescuebackupcopyqueuegrowing }

**Symptom.** The copy queue was never empty during the last 4 hours, for 10 minutes: copies wait faster than they finish.

**Likely causes.** A slow or unreachable copy target, an upload cap (`max_upload_mbps`) smaller than the backup rate, or backups that outgrew the copy bandwidth.

**Diagnosis.**

```promql
mongorescue_backup_copy_queue_depth
rate(mongorescue_backup_size_bytes[1d])
increase(mongorescue_backup_copies_total[1h])
```

**Action.** Check the copy target's health and throughput, raise `max_upload_mbps`, or reduce the backup frequency or size. Copies run outside backup windows, so a window is not the cause.

**Escalation.** A queue that keeps growing means copies will never catch up: size the copy bandwidth like the primary's ([sizing](production.md#sizing)).

## Notifications, events, audit and keys

### MongoRescueNotificationFailures { #mongorescuenotificationfailures }

**Symptom.** More than half of a channel type's notifications, and at least 2, were given up (`status="failure"`) in the last hour. Alerts may not reach you.

**Likely causes.** The webhook, Slack, Teams, email or other endpoint rejects or times out; a rotated token; an SMTP outage; a proxy or network policy blocks the egress.

**Diagnosis.**

```promql
sum by (channel_type, status) (increase(mongorescue_notifications_total[1h]))
```

```sh
mr -X POST $MR/api/v1/notifications/channels/<id>/test         # the channel's own error
kubectl logs <pod> --since=1h | grep -i notif
```

**Action.** Fix the channel (URL, token, credentials), send a test, and keep a second, independent path (the external heartbeat, Alertmanager) while it is down.

**Escalation.** This alert only reaches you through Prometheus: if Alertmanager is also routed to the failing service, page by another route.

### MongoRescueNotificationsLost { #mongorescuenotificationslost }

**Symptom.** Notifications were `dropped` (the queue was full) or `expired` (waited longer than `MONGORESCUE_NOTIFICATION_MAX_AGE`) in the last 15 minutes and were never delivered.

**Likely causes.** A channel slower than the event rate or unreachable for a long time; an event storm (many jobs failing at once).

**Diagnosis.**

```promql
sum by (channel_type, status) (increase(mongorescue_notifications_total{status=~"dropped|expired"}[15m]))
increase(mongorescue_events_dropped_total[15m])
```

**Action.** Fix the slow channel ([NotificationFailures](#mongorescuenotificationfailures)); reduce the rules that send high-volume events to it; look at what produced the event storm (usually failing backups).

**Escalation.** Events you never received describe real failures: read the failed runs directly (`GET /api/v1/backups?status=failed`).

### MongoRescueEventsDropped { #mongorescueeventsdropped }

**Symptom.** Events were dropped in the last 15 minutes because the event queue was full or stopped. Their notifications and metrics are missing, so other dashboards and alerts built on events may be wrong.

**Likely causes.** A subscriber is slow or blocked, an event burst, or a shutdown in progress.

**Diagnosis.**

```promql
increase(mongorescue_events_dropped_total[15m])
mongorescue_audit_queue_depth
```

```sh
kubectl logs <pod> --since=15m | grep -i -e dropped -e queue
```

**Action.** Find the burst (many failures, a mass bulk action) or the slow component in the log, and let it drain. If it repeats without a burst, restart MongoRescue and report it.

**Escalation.** Open an issue with the log lines and the event rate when it repeats.

### MongoRescueHeartbeatsDropped { #mongorescueheartbeatsdropped }

**Symptom.** Job heartbeat pings were dropped in the last 15 minutes (32 were already queued or in flight). The dead-man's-switch service may report a healthy job as missing.

**Likely causes.** The heartbeat service is slow or unreachable, so pings pile up (each has a 10 second timeout and two retries); many jobs finish at once.

**Diagnosis.**

```promql
increase(mongorescue_heartbeat_dropped_total[15m])
```

```sh
curl -sS -m 10 -o /dev/null -w '%{http_code} %{time_total}s\n' <heartbeat url>
mr -X POST $MR/api/v1/settings/monitoring/test
```

**Action.** Fix reachability or latency to the heartbeat service, or use one in a nearer region. Check that the monitor's period and grace time still make sense ([heartbeats](monitoring.md#job-heartbeats)).

**Escalation.** If the external monitor is paging for jobs that ran fine, silence only those checks until pings flow, and confirm with `mongorescue_job_runs_total`.

### MongoRescueAuditWriteFailed { #mongorescueauditwritefailed }

**Symptom.** Audit log entries could not be stored in the last 15 minutes (the metadata database is full, read-only or locked). Security-relevant actions are not being recorded.

**Likely causes.** The data directory is full or the volume is read-only; SQLite is locked by another process; a damaged disk.

**Diagnosis.**

```promql
increase(mongorescue_audit_write_failures_total[15m])
mongorescue_data_dir_free_bytes
```

```sh
df -h /data ; kubectl logs <pod> --since=15m | grep -i -e audit -e 'disk is full' -e readonly
mr $MR/api/v1/ready | jq .                                    # store_unwritable / disk_full
```

**Action.** Free space or fix the volume ([MongoRescueDiskFillingUp](#mongorescuediskfillingup)); verify the chain afterwards with `mr $MR/api/v1/audit/events/verify`.

**Escalation.** Treat the gap as a compliance event: note when it started and ended, and export the audit log (`GET /api/v1/audit/events/export`) as soon as writes recover.

### MongoRescueAuditForwardFailed { #mongorescueauditforwardfailed }

**Symptom.** Audit entries could not be forwarded to the audit webhook (`outcome`: `failed` or `dropped`) in the last 15 minutes, for 5 minutes. The local log is intact.

**Likely causes.** The webhook endpoint is down or rejects requests, a changed token or TLS certificate, a queue too small for the burst.

**Diagnosis.**

```promql
sum by (outcome) (increase(mongorescue_audit_forward_total{outcome=~"failed|dropped"}[15m]))
mongorescue_audit_queue_depth
```

```sh
kubectl logs <pod> --since=15m | grep -i forward
```

**Action.** Fix the webhook; entries refused while it was down are in the local log and can be exported (`GET /api/v1/audit/events/export`) and replayed into your SIEM.

**Escalation.** A SIEM that must have every entry: export the gap from the local log as soon as the webhook works.

### MongoRescueKeyWrapErrors { #mongorescuekeywraperrors }

**Symptom.** Calls to the KMS that wrap or unwrap `secret.key` failed in the last 15 minutes. MongoRescue cannot restart without unwrapping the key.

**Likely causes.** The instance identity lost its KMS permissions, the KMS key was disabled or scheduled for deletion, a network or endpoint problem, throttling by the KMS.

**Diagnosis.**

```promql
sum by (provider, operation) (increase(mongorescue_key_wrap_errors_total[15m]))
histogram_quantile(0.95, sum by (le, provider, operation) (rate(mongorescue_key_wrap_duration_seconds_bucket[1h])))
mongorescue_key_wrapping_active
```

```sh
kubectl logs <pod> --since=15m | grep -i -e kms -e 'key wrap'
aws kms describe-key --key-id <key>        # state Enabled? pending deletion?
```

**Action.** Restore the identity's `Encrypt`/`Decrypt` permissions and the KMS key's state. A running instance keeps working; the risk is the next restart. Do not restart or reschedule the pod until the calls succeed.

**Escalation.** A KMS key scheduled for deletion loses every stored credential: cancel the deletion at once ([key wrapping](security.md#wrapping-secretkey-with-a-kms)); keep the [recovery kit](production.md#recovery-kit) current.

### MongoRescueAPIKeysExpired { #mongorescueapikeysexpired }

**Symptom.** Stored API keys are past their expiry (`mongorescue_api_keys_expired > 0`) for an hour. Expired keys are refused: scripts, CI jobs, scrapes and MCP clients that still use them fail with `401 api_key_expired`.

**Diagnosis.**

```sh
mr $MR/api/v1/api-keys | jq .data
```

**Action.** For each key still in use (`last_used_at` is recent), create a replacement, update the consumer, then revoke the expired key (`DELETE /api/v1/api-keys/<id>`). If Prometheus used it, fix the scrape credentials first ([MongoRescueDown](#mongorescuedown) hides behind a refused scrape).

**Escalation.** None; a key nobody uses can be revoked.

### MongoRescueAPIKeysExpiring { #mongorescueapikeysexpiring }

**Symptom.** API keys expire within the warning days (`security.api_key_expiry_warning_days`) and the alert has been active for an hour.

**Diagnosis.**

```sh
mr $MR/api/v1/api-keys | jq .data 
```

**Action.** Create replacements with the same scope, update scripts, CI jobs, the Prometheus scrape and MCP clients, then revoke the old keys. A shorter expiry on new keys keeps the habit of rotating.

**Escalation.** None before the expiry; after it, this becomes [MongoRescueAPIKeysExpired](#mongorescueapikeysexpired) and the consumers fail.
