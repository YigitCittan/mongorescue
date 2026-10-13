# Prometheus metrics

MongoRescue exposes metrics in the Prometheus text format on `GET /metrics`.

## Access

`/metrics` requires an API key (`Authorization: Bearer <key>` or `X-API-Key`): one created in Settings → API keys. Session cookies are not accepted there. Turn on **Settings → Security → Public metrics** (`security.metrics_public`) to serve it without authentication, for example when the port is only reachable from your monitoring network.

## Metrics

| Metric | Type | Labels | Description |
| :--- | :--- | :--- | :--- |
| `mongorescue_backups_total` | counter | `job`, `status` | Finished backups (`status`: `succeeded`, `failed`) |
| `mongorescue_backup_duration_seconds` | histogram | `job` | Backup duration |
| `mongorescue_backup_size_bytes` | gauge | `job` | Size of the last successful backup artifact |
| `mongorescue_last_successful_backup_timestamp_seconds` | gauge | `job` | Unix time of the last successful backup |
| `mongorescue_database_backups_total` | counter | `job`, `database`, `status` | Finished backups per database (`status`: `succeeded`, `failed`, `cancelled`; `job="manual"` for manual backups) |
| `mongorescue_database_backup_size_bytes` | gauge | `job`, `database` | Size of the last successful backup of each database |
| `mongorescue_database_last_successful_backup_timestamp_seconds` | gauge | `job`, `database` | Unix time of the last successful backup of each database |
| `mongorescue_job_runs_total` | counter | `job`, `status` | Finished job runs over all their databases (`status`: `ok`, `partial`, `failed`, `cancelled`, or `skipped` for a scheduled run outside its backup window, which backs nothing up) |
| `mongorescue_job_run_duration_seconds` | histogram | `job` | Duration of job runs (all databases) |
| `mongorescue_restores_total` | counter | `status` | Finished restores (`succeeded`, `failed`) |
| `mongorescue_notifications_total` | counter | `channel_type`, `status` | Notification deliveries (`status`: `success`, `failure` (given up), `dropped` (the queue was full), `expired` (waited longer than `MONGORESCUE_NOTIFICATION_MAX_AGE`)) |
| `mongorescue_events_dropped_total` | counter | | Events dropped because the event queue was full or stopped |
| `mongorescue_audit_write_failures_total` | counter | | Audit log entries that could not be stored (see [audit.md](audit.md)) |
| `mongorescue_audit_sync_writes_total` | counter | | Audit log entries written on the request path because the write queue was full |
| `mongorescue_audit_queue_depth` | gauge | | Audit log entries waiting to be written |
| `mongorescue_audit_forward_total` | counter | `outcome` | Audit log entries forwarded to the audit webhook: `sent`, `failed` (refused or unreachable) or `dropped` (queue full or stopped); see [audit.md](audit.md#forwarding) |
| `mongorescue_scheduled_jobs` | gauge | | Jobs registered with the scheduler |
| `mongorescue_api_rate_limited_total` | counter | `caller`, `budget` | REST API requests refused with `429` by the [rate limit](security.md#rest-api-rate-limits) (`caller`: `api_key`, `session`, `anonymous`; `budget`: `general`, `work`, or `auth` for the sign-in routes of unauthenticated callers) |
| `mongorescue_mcp_calls_total` | counter | `tool`, `result` | [MCP](mcp.md) tool calls (`result`: `ok`, `error`, `denied`, `rate_limited`; unknown tools as `tool="unknown"`) |
| `mongorescue_bulk_operations_total` | counter | `resource`, `action` | [Bulk operations](api.md#bulk-actions) run (not dry runs; `resource`: `backups`, `restores`, `jobs`) |
| `mongorescue_bulk_items_total` | counter | `resource`, `action`, `outcome` | Items of bulk operations (`outcome`: `succeeded`, `skipped`, `failed`) |
| `mongorescue_build_info` | gauge | `version`, `commit`, `go_version` | Always 1 |
| `mongorescue_verifications_total` | counter | `source`, `result` | Archive [verifications](verification.md) (`source`: `after_upload`, `sweep`, `on_demand`; `result`: `ok`, `mismatch`, `error`) |
| `mongorescue_restore_tests_total` | counter | `job`, `result` | Automated restore tests (`ok`, `mismatch`, `error`) |
| `mongorescue_last_successful_restore_test_timestamp_seconds` | gauge | `job` | Unix time of the last passed restore test |
| `mongorescue_storage_orphan_archives` | gauge | `target` | Archives without a backup record found by the last storage scan |
| `mongorescue_storage_missing_archives` | gauge | `target` | Backup records whose archive the last storage scan did not find |
| `mongorescue_last_storage_scan_timestamp_seconds` | gauge | `target` | Unix time of the last storage scan |
| `mongorescue_retention_deletions_total` | counter | `job` | Backups deleted by retention |
| `mongorescue_metadata_backups_total` | counter | `result` | [Metadata snapshots](production.md#metadata-backups) (`ok`, `error`) |
| `mongorescue_api_keys_expiring` | gauge | | API keys that expire within `security.api_key_expiry_warning_days` and have not expired yet ([API key expiry](security.md#api-key-expiry)); updated hourly and when the settings change, on the active instance |
| `mongorescue_api_keys_expired` | gauge | | Stored API keys past their expiry (refused; revoke them) |
| `mongorescue_key_wrapping_active` | gauge | `provider` | `1` while `secret.key` is [wrapped by a KMS](security.md#wrapping-secretkey-with-a-kms); absent without key wrapping |
| `mongorescue_key_wrap_duration_seconds` | histogram | `provider`, `operation` | Duration of the KMS calls that unwrap (at startup) and wrap (key rotation) `secret.key`; `operation` is `wrap` or `unwrap` |
| `mongorescue_key_wrap_errors_total` | counter | `provider`, `operation` | Failed KMS calls, each retry counted. A start that never reaches the KMS fails before it serves metrics: watch the instance's health too |
| `mongorescue_backup_copies_total` | counter | `result` | Attempts to copy a backup to a [copy target](configuration.md#copies-on-a-second-target-3-2-1) (`ok`, `mismatch`, `error`) |
| `mongorescue_backup_copy_queue_depth` | gauge | | Backup copies waiting in the copy queue (pending, or failed with a next attempt) |
| `mongorescue_last_successful_metadata_backup_timestamp_seconds` | gauge | | Unix time of the last stored metadata snapshot |
| `mongorescue_metadata_backup_size_bytes` | gauge | | Size of the last stored metadata snapshot |
| `mongorescue_job_rpo_seconds` | gauge | `job`, `database` | Age of the newest successful backup of each database of an enabled job (since the job's creation when it has none); see [recovery point objectives](#recovery-point-objectives) |
| `mongorescue_job_rpo_met` | gauge | `job`, `database` | 1 while that age is within the job's recovery point objective, 0 when it is missed |
| `mongorescue_job_rpo_target_seconds` | gauge | `job`, `database` | The job's recovery point objective (`rpo_minutes`, or the default from its schedule) |
| `mongorescue_scheduler_last_tick_timestamp_seconds` | gauge | | Unix time of the scheduler's last liveness tick (every 30s while it runs; `0` before it starts). Older than 90 seconds means the scheduler is hung; see [health](api.md#health) |
| `mongorescue_heartbeat_dropped_total` | counter | | Job heartbeat pings dropped because 32 were already queued or in flight (see [monitoring.md](monitoring.md#how-pings-are-sent)) |
| `mongorescue_ha_role` | gauge | `cluster`, `role` | [Active-passive HA](production.md#high-availability) only: 1 for the instance's current role (`active` or `standby`), 0 for the other. A standby serves `/metrics` without credentials, with only the process, Go runtime, build and HA series |
| `mongorescue_ha_epoch` | gauge | `cluster` | HA only: the lease epoch (fencing token) the instance holds, or the highest it saw as standby |
| `mongorescue_ha_lease_healthy` | gauge | `cluster` | HA only: 1 when the last read or write of the lease succeeded |
| `mongorescue_ha_lease_errors_total` | counter | `cluster` | HA only: failed reads and writes of the lease, renewals included |
| `mongorescue_ha_takeovers_total` | counter | `cluster` | HA only: times this instance acquired the lease |
| `mongorescue_ha_takeover_duration_seconds` | gauge | `cluster` | HA only: duration of the last takeover, from acquiring the lease to serving as active |
| `mongorescue_ha_step_downs_total` | counter | `cluster` | HA only: times this instance lost the lease while active and stepped down |
| `mongorescue_ha_takeover_failures_total` | counter | `cluster` | HA only: takeovers given up because no usable metadata snapshot was found (the instance stayed standby) |
| `mongorescue_ha_snapshots_rejected_total` | counter | `cluster` | HA only: metadata snapshots refused because their authenticated header did not match their name or cluster, or they did not decrypt (copied, renamed or altered on the HA target); never restored nor deleted. Counted once per stored object version and process |
| `mongorescue_ha_empty_starts_total` | counter | `cluster` | HA only: times this instance became active from empty state (no lease, no metadata snapshot on the HA target) and kept its own database; expected once for a new cluster |
| `mongorescue_ha_rollback_suspected_total` | counter | `cluster`, `reason` | HA only: takeovers refused because they could roll the metadata back: `lease_untrusted`, `lease_missing`, `snapshot_behind_lease`, `snapshot_behind_local`, `legacy_after_upgrade`, `lease_replayed`, `lease_stale` ([rollback protection](design/ha.md#rollback-protection)) |
| `mongorescue_ha_refused_snapshots` | gauge | `cluster` | HA only: how many such refused snapshots the HA target holds, as of this instance's last check; they stay until someone deletes them ([how](design/ha.md#security)) |
| `mongorescue_pitr_collector_up` | gauge | `stream` | 1 while the [PITR](pitr.md) collector of the stream stores chunks, 0 while it fails or is stopped |
| `mongorescue_pitr_lag_seconds` | gauge | `stream` | Seconds between the replica set's newest write and the end of the last stored chunk |
| `mongorescue_pitr_last_chunk_timestamp_seconds` | gauge | `stream` | Oplog time (seconds) of the end of the last stored chunk |
| `mongorescue_pitr_chunks_total` | counter | `stream`, `result` | Oplog chunks (`ok`, `error`) |
| `mongorescue_pitr_chunk_bytes_total` | counter | `stream` | Stored bytes of oplog chunks (compressed and encrypted) |
| `mongorescue_pitr_oplog_headroom_seconds` | gauge | `stream` | Seconds between the oldest oplog entry and the collector's position: how long it may stop before entries are lost |
| `mongorescue_pitr_window_start_timestamp_seconds`, `mongorescue_pitr_window_end_timestamp_seconds` | gauge | `stream` | Bounds of the newest point-in-time window (absent without an eligible base) |
| `mongorescue_pitr_chain_breaks_total` | counter | `stream`, `reason` | Chains ended by `gap`, `replica_set_changed` or `diverged` |
| `mongorescue_pitr_chain_test_unverified_collections` | gauge | `stream` | Collections whose document count the newest chain test could not verify (no exact count at the newer base's consistent point); 0 when every count was checked. Take a new base backup when it stays above 0 ([pitr.md](pitr.md#chain-tests)) |
| `mongorescue_settings_warnings` | gauge | | Active settings warnings shown in the dashboard banner (encryption off, metadata backups unencrypted, recovery kit missing or outdated, a kept administrator role) |

The `job` label is the scheduled job ID; on-demand backups use `job="manual"`. With a default scrape configuration Prometheus keeps its own `job` label (the scrape job) and renames MongoRescue's to `exported_job`; with `honor_labels: true` the job ID stays in `job`. The [shipped alert rules](monitoring.md#prometheus-alert-rules) work either way. `mongorescue_backups_total` and `mongorescue_job_runs_total` exist at `0` for every enabled job (and `job="manual"`) from the first RPO check after the start, so `increase()` also sees a job's first failure. The standard Go runtime and process collectors (`go_*`, `process_*`) are exported as well.

## Scrape configuration

```yaml
scrape_configs:
  - job_name: mongorescue
    scheme: https
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/secrets/mongorescue-api-key
    static_configs:
      - targets: ["backup.example.com"]
```

## Alerting

Ready-made rules, unit-tested with `promtool test rules`, ship in [`deploy/prometheus/alerts.yml`](../deploy/prometheus/alerts.yml): instance down, scheduler stale, backup failures, RPO missed, archive verification failures, missing archives, restore test failures, metadata backup failures and active settings warnings. See [monitoring.md](monitoring.md#prometheus-alert-rules). The examples below show the pattern.

Alert when a job has had no successful backup for 26 hours (a daily schedule plus two hours of slack):

```yaml
groups:
  - name: mongorescue
    rules:
      - alert: MongoRescueBackupStale
        expr: time() - mongorescue_last_successful_backup_timestamp_seconds > 26 * 3600
        for: 10m
        labels:
          severity: critical
        annotations:
          summary: "No successful MongoDB backup for job {{ $labels.job }} in 26h"

      - alert: MongoRescueBackupFailing
        expr: increase(mongorescue_backups_total{status="failed"}[1h]) > 0
        labels:
          severity: warning
        annotations:
          summary: "Backup job {{ $labels.job }} failed in the last hour"

      - alert: MongoRescueArchiveDamaged
        expr: increase(mongorescue_verifications_total{result="mismatch"}[1h]) > 0
        labels:
          severity: critical
        annotations:
          summary: "A stored backup archive no longer matches its checksum ({{ $labels.source }})"

      - alert: MongoRescueRestoreTestStale
        expr: time() - mongorescue_last_successful_restore_test_timestamp_seconds > 8 * 24 * 3600
        labels:
          severity: warning
        annotations:
          summary: "No passed restore test for job {{ $labels.job }} in 8 days"
```

The first rule does not fire for a job that has never succeeded, because the series does not exist yet; pair it with notifications on `backup.failed` (see [notifications.md](notifications.md)), or alert on the RPO gauges below, which exist from the job's creation.

## Recovery point objectives

The `mongorescue_job_rpo_*` gauges follow each job's [recovery point objective](api.md#recovery-point-objectives) per database. The RPO checker refreshes their set of series every 5 minutes and right after a job's backup finishes or the job is saved, paused, resumed or deleted (paused and deleted jobs drop out); the age is computed when Prometheus scrapes, so it grows between checks, and `mongorescue_job_rpo_met` follows it. Unlike the fixed 26 hours of the rule above, the objective follows each job's schedule or its own `rpo_minutes`:

```yaml
      - alert: MongoRescueRPOMissed
        expr: mongorescue_job_rpo_met == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Job {{ $labels.job }}: {{ $labels.database }} is past its recovery point objective"
```

The same breach is published once as `job.rpo_missed` (and `job.rpo_recovered` when it heals) for [notification rules](notifications.md).
