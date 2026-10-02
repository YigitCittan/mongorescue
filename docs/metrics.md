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
| `mongorescue_job_runs_total` | counter | `job`, `status` | Finished job runs over all their databases (`status`: `ok`, `partial`, `failed`, `cancelled`) |
| `mongorescue_job_run_duration_seconds` | histogram | `job` | Duration of job runs (all databases) |
| `mongorescue_restores_total` | counter | `status` | Finished restores (`succeeded`, `failed`) |
| `mongorescue_notifications_total` | counter | `channel_type`, `status` | Notification deliveries (`status`: `success`, `failure`, `dropped`) |
| `mongorescue_events_dropped_total` | counter | | Events dropped because the event queue was full or stopped |
| `mongorescue_scheduled_jobs` | gauge | | Jobs registered with the scheduler |
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

The `job` label is the scheduled job ID; on-demand backups use `job="manual"`. The standard Go runtime and process collectors (`go_*`, `process_*`) are exported as well.

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

The first rule does not fire for a job that has never succeeded, because the series does not exist yet; pair it with notifications on `backup.failed` (see [notifications.md](notifications.md)).
