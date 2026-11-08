# Monitoring

MongoRescue reports failed backups itself, through [notifications](notifications.md). It cannot report its own failure: when the process crashes, the host is down, the scheduler hangs or the disk is full, the process that would send the alert is the one that stopped, and a silent failure looks like "no news". These mechanisms cover that gap, and they work best together:

| Mechanism | Catches | Needs |
| :--- | :--- | :--- |
| [Global heartbeat](#global-heartbeat) | The process or host is down, the scheduler hangs | An external dead-man's-switch service |
| [Job heartbeats](#job-heartbeats) | A job stops running, runs too long or fails | The same service, one check per job |
| [Health check](#health-check) | A hung scheduler, for Docker, load balancers and uptime monitors | Nothing |
| [Prometheus alert rules](#prometheus-alert-rules) | Down instances, failures, missed RPOs, integrity problems | Prometheus and Alertmanager |

## Global heartbeat

A dead-man's switch alerts when an expected ping does *not* arrive. Set **Settings → Monitoring → Heartbeat URL** (`monitoring.heartbeat_url`) to the ping URL of a check, and **Ping every** (`monitoring.heartbeat_interval`, 1 to 60 minutes, default 5) to how often it is pinged. MongoRescue sends `GET <url>` at start and then every interval, but only while the scheduler is healthy (its last tick is at most 90 seconds old): a hung scheduler stops the pings, so the monitor alerts even though the process still answers.

Configure the check at the external service with the same period and a grace time of a few minutes (for example period 5 minutes, grace 5 minutes). When the process is killed or the host goes down, the monitor alerts within one interval plus the grace time.

**Send test ping** (at most 5 per minute) sends one ping to the URL in the field (or, when the field shows the stored, masked URL, to the stored one) and reports whether the monitor answered with a `2xx` status. The same is `POST /api/v1/settings/monitoring/test` ([API](api.md#endpoints)).

## Job heartbeats

Every job can have a heartbeat URL of its own (the **Heartbeat URL** field of the job form, `heartbeat_url` in the [API](api.md#job-heartbeats)). It follows the [healthchecks.io](https://healthchecks.io/docs/http_api/) protocol:

| Event | Ping |
| :--- | :--- |
| A run starts (scheduled or on demand, all its databases) | `<url>/start` |
| The run succeeded (`ok`) | `<url>` |
| The run failed or was `partial` (also a run that could not start) | `<url>/fail` |
| The run was cancelled by a user, or interrupted by a shutdown | nothing |

A cancellation is a deliberate action, so it pages nobody: the check stays "started" and the monitor reports it as late when no successful run follows within its period and grace time, which is the right signal for a backup that did not happen.

The suffix is appended to the URL's path; a query string is kept (`https://push.example.com/api/push/abc?status=up` becomes `.../abc/fail?status=up`). With a `/start` ping the service also measures how long each run takes and alerts when a run does not finish. Give the check the job's schedule (healthchecks.io accepts cron expressions) and a grace time longer than a run takes.

## How pings are sent

- Pings never block or fail a backup: they are sent in the background, at most 32 at a time (queued or in flight; further ones are dropped, logged and counted in `mongorescue_heartbeat_dropped_total`), every attempt has a 10 second timeout, and a network error, `408`, `429` or `5xx` is retried twice (after 2 and 4 seconds). Other `4xx` answers are not retried. The pings of one job are sent in order, so a `/start` never arrives after the outcome.
- Requests go through the same HTTP client as notifications: redirects are not followed, link-local and cloud metadata addresses are refused (also after DNS resolution), and `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` apply.
- Heartbeat URLs carry the check's token, so they are secrets: they are stored encrypted (`secret.key`), shown only up to their host (`https://hc-ping.com/******`) by the dashboard, the API and MCP, and never logged; failures are logged with the host and a fixed reason only (`connection refused`, `timeout`, `TLS error`, `DNS error`), never the resolved address. A stored URL that can no longer be decrypted is dropped: the job keeps running without its heartbeat and the URL is reported among the unreadable records until it is entered again. Sending the masked value back keeps the stored URL; a masked value for another host is refused, so a URL is never kept for a host it was not entered for.
- At shutdown, pings already queued are still sent, for up to 15 seconds; runs the shutdown interrupts send nothing after their `/start`.

## Health check

`GET /api/v1/health` (no authentication) answers `503` with `"scheduler": "stale"` when the scheduler's last liveness tick is older than 90 seconds (three ticks), and `200` with `"scheduler": "ok"` and `scheduler_last_tick` otherwise; `status` keeps its value, so checks must read the HTTP status or `scheduler` (see [api.md](api.md#health)). The Docker image's `HEALTHCHECK` uses it, so a container with a hung scheduler is reported unhealthy; point load balancer and uptime checks at it too. In [active-passive HA mode](production.md#high-availability) it adds `role` (`active` or `standby`) and `ha` (cluster, epoch, holder, lease health); a standby answers `200` with `"scheduler": "not_started"`. It is the **liveness** check: it stays `200` on a standby and while a shutdown drains, so a restart policy never kills an instance for being passive or busy.

### Readiness check

`GET /api/v1/ready` (no authentication, never rate limited; not to be confused with `GET /api/v1/readiness`, the recovery-readiness report) is the check to route traffic by, since load balancers and Kubernetes read the status code. It answers `200` only when the instance is the active one (always, outside HA mode), is not draining, can write its metadata database and the disk guard accepts new runs. Otherwise it answers `503` with `Retry-After` and a machine-readable reason in the JSON `code` field (and `data.reason`): `not_active`, `draining`, `store_unwritable` or `disk_full`, the first that applies in that order (see [api.md](api.md#readiness-check)). A standby and an instance that is changing role answer it too, as `not_active`. Point load balancer targets at it to follow a failover; keep uptime monitors and the container `HEALTHCHECK` on `/api/v1/health`. On Kubernetes, `ha.readinessActiveOnly` switches the readiness probe to it ([kubernetes.md](kubernetes.md#high-availability)).

### Clock steps

The liveness tick runs every 30 seconds on a monotonic timer of its own, not on the cron that starts jobs, and its age is measured on the monotonic clock. A wall clock step (an NTP correction, a restored VM snapshot, a manual change) therefore never delays the tick nor makes the scheduler stale: the health check stays `200`, the heartbeat keeps pinging and `MongoRescueSchedulerStale` does not fire. Only a scheduler that cannot take its own lock (a deadlock) or a stopped one stops ticking.

Scheduled jobs follow the wall clock, since cron expressions name wall-clock times:

- **Forward step.** Every job whose next run the clock jumped over runs once, late, when the scheduler next wakes up (within about a minute: its own checks run every minute), and then on its schedule from the new time. Missed runs are not caught up one by one.
- **Backward step.** Jobs wait until the clock reaches their next run again, so their runs are delayed by the size of the step. A step never repeats a run by itself. A job whose schedule is rebuilt after the step (it is edited or re-enabled, or MongoRescue restarts) could be due again at a time that already ran; MongoRescue skips such a trigger when the job's previous scheduled run started less than half its interval earlier (measured on the monotonic clock, or after a restart on the start time of the newest scheduled run on record), and logs `skipping a repeated scheduled run`. After a restart at most one trigger is skipped this way.

## Request metrics and correlation IDs

Every HTTP response carries an `X-Request-Id` header. A well-formed incoming value (8 to 128 characters of letters, digits and `._:~-`) is kept only when the request comes from a [trusted proxy](production.md); from anyone else it is ignored and a random ID is generated, so a client cannot choose the ID in your logs. The ID is in every log line written for that request (the `request_id` attribute, text and JSON) and so a user can quote it (the response header) and you can find the line.

`mongorescue_http_requests_total{route,method,code}` and `mongorescue_http_request_duration_seconds{route,method}` ([metrics](metrics.md)) count every request. `route` is the registered route pattern without its method (`/api/v1/jobs/{id}`), never the raw path; paths no route matches, such as scanners probing the server, share `unmatched`, and a non-standard method is `OTHER`, so the series count is bounded by the number of routes. Scrapes of `/metrics` itself are not counted (they would dominate a quiet instance). Example error ratio:

```promql
sum(rate(mongorescue_http_requests_total{code=~"5.."}[5m])) / sum(rate(mongorescue_http_requests_total[5m]))
```

With [`-access-log`](configuration.md#logging) every request also writes one line. Backup, restore, PITR, verification and copy log lines carry `run_id` (the job run, or the record ID of a run that belongs to no job run), `job_id` and `backup_id` or `restore_id`, the ID of the run's log file.

## Prometheus alert rules

[`deploy/prometheus/alerts.yml`](https://github.com/YigitCittan/mongorescue/blob/main/deploy/prometheus/alerts.yml) holds alerting rules over the [metrics](metrics.md), unit-tested with `promtool test rules` in CI ([`alerts_test.yml`](https://github.com/YigitCittan/mongorescue/blob/main/deploy/prometheus/alerts_test.yml); run them with `make test-prometheus-rules`):

| Alert | Severity | Fires when |
| :--- | :--- | :--- |
| `MongoRescueDown` | critical | `up == 0` for 5 minutes on a target that exposed `mongorescue_build_info` in the last 7 days |
| `MongoRescueSchedulerStale` | critical | The last scheduler tick is older than 90 seconds, for 2 minutes |
| `MongoRescueBackupFailed` | warning | A backup of a job (or a manual backup, `job="manual"`) failed in the last hour, or a run failed before any backup started |
| `MongoRescueRPOMissed` | critical | `mongorescue_job_rpo_met == 0` for 5 minutes: a database is past its job's [recovery point objective](api.md#recovery-point-objectives) |
| `MongoRescueVerificationFailed` | critical | An archive verification (after upload, integrity sweep or on demand) found a mismatch or could not read the archive in the last hour |
| `MongoRescueArchivesMissing` | critical | The last storage scan did not find the archives of some backup records |
| `MongoRescueRestoreTestFailed` | warning | An automated restore test failed or found differences in the last 24 hours |
| `MongoRescueMetadataBackupFailed` | warning | A [metadata snapshot](production.md#metadata-backups) failed in the last 24 hours |
| `MongoRescueSettingsWarning` | warning | A settings warning (encryption off, recovery kit missing or outdated, ...) has been active for an hour |
| `MongoRescueHATakeoverFailed` | critical | A standby of an HA cluster acquired the lease in the last 15 minutes but found no usable metadata snapshot to take over from, and stayed standby |
| `MongoRescueHAStartedFromEmptyState` | critical | An instance of an HA cluster found no lease and no metadata snapshot on the HA target in the last 15 minutes and became active with its own database. Expected once for a new cluster; otherwise check the HA target and run a storage scan of every storage target to re-import the archives |
| `MongoRescueHARollbackSuspected` | critical | A standby of an HA cluster refused a takeover in the last 15 minutes because it could roll the metadata back (the lease is not authentic or missing, or the newest snapshot is older than the lease or the instance knows). No instance becomes active until an operator checks the HA target and runs `mongorescue ha force-takeover` ([how](design/ha.md#forcing-a-takeover)) |
| `MongoRescueHASnapshotRejected` | critical | An instance of an HA cluster found a refused metadata snapshot on the HA target (it lasts until the snapshot is deleted) or refused one in the last 15 minutes: its authenticated header does not match its name or cluster (copied or renamed on the HA target), or it does not decrypt (altered). It is never restored nor deleted; delete it after the investigation ([how](design/ha.md#security)) |
| `MongoRescueHAPITRSwapInterrupted` | critical | An instance that took over from one that was swapping an [in-place point-in-time restore](pitr.md#replacing-the-original-databases-in-place) heard nothing from the swap within the handover timeout in the last 15 minutes and marked the restore interrupted: the original databases may be partially replaced; the clones and safety backups are kept ([runbook](runbooks.md#mongorescuehapitrswapinterrupted)) |
| `MongoRescuePITRCollectorDown` | critical | `mongorescue_pitr_collector_up == 0` for 10 minutes: the oplog collector of a [PITR](pitr.md) stream fails or is stopped |
| `MongoRescuePITRLagHigh` | warning | A PITR stream's lag is above 15 minutes (900 s) for 10 minutes |
| `MongoRescuePITROplogHeadroomLow` | critical | The oplog headroom of a PITR stream is below max(6 hours, 3 x the lag) for 10 minutes (the bound of the `pitr.window_low` event): entries are lost, and the recovery window breaks, if the collector stops that long |
| `MongoRescuePITRBaseWindowRisk` | warning | The last successful base backup of a PITR stream took more than half of the oplog window for 10 minutes (the default bound of the `pitr.base_window_risk` event): a base that outlasts the window fails and the recovery window stops moving. See [sizing the oplog](pitr.md#sizing-the-oplog) |
| `MongoRescuePITRChainBroken` | critical | A PITR chain ended in the last hour (`gap`, `replica_set_changed` or `diverged`): restores cannot cross it |
| `MongoRescuePITRChunkErrors` | warning | At least 3 oplog chunks of a stream failed in the last hour |
| `MongoRescuePITRStale` | warning | The newest chunk of a stream ends more than an hour ago while the lag is above 60 s (an idle replica set, lag 0, is not stale), for 10 minutes |
| `MongoRescueBackupCopyFailed` | warning | A [backup copy](configuration.md#copies-on-a-second-target-3-2-1) failed (`result="error"`) in the last hour |
| `MongoRescueBackupCopyMismatch` | critical | A backup copy did not match its primary's checksum in the last hour |
| `MongoRescueBackupCopyQueueGrowing` | warning | The copy queue was never empty during the last 4 hours, for 10 minutes |
| `MongoRescueNotificationFailures` | warning | More than half of a channel type's notifications, and at least 2, were given up (`status="failure"`) in the last hour |
| `MongoRescueNotificationsLost` | warning | Notifications were `dropped` (queue full) or `expired` in the last 15 minutes |
| `MongoRescueEventsDropped` | warning | Events were dropped because the event queue was full or stopped in the last 15 minutes |
| `MongoRescueHeartbeatsDropped` | warning | Job heartbeat pings were dropped in the last 15 minutes |
| `MongoRescueAuditWriteFailed` | critical | An [audit log](audit.md) entry could not be stored in the last 15 minutes |
| `MongoRescueAuditForwardFailed` | warning | Audit entries failed to forward to the audit webhook (`failed` or `dropped`) in the last 15 minutes, for 5 minutes |
| `MongoRescueKeyWrapErrors` | warning | KMS calls that wrap or unwrap `secret.key` failed in the last 15 minutes |
| `MongoRescueAPIKeysExpired` | warning | Stored API keys are past their expiry (`mongorescue_api_keys_expired > 0`) for an hour |
| `MongoRescueAPIKeysExpiring` | warning | API keys expire within the warning days (`mongorescue_api_keys_expiring > 0`) for an hour |
| `MongoRescueConnectionCertExpiringSoon` | warning | A TLS client certificate or CA certificate of a connection expires within 30 days (`mongorescue_connection_tls_cert_expiry_timestamp_seconds`), for an hour |
| `MongoRescueConnectionCertExpired` | critical | A TLS client certificate or CA certificate of a connection has expired, for 5 minutes |
| `MongoRescueDiskFillingUp` | warning | `predict_linear` over the last 6 hours says the data directory or a local storage target (`mongorescue_data_dir_free_bytes`, `mongorescue_storage_free_bytes`) runs out of space within 24 hours, for 30 minutes |
| `MongoRescueHANoActiveInstance` | critical | No instance of an [active-passive HA cluster](production.md#high-availability) has reported `role="active"` for a minute (twice the default lease TTL), while one of them exported `mongorescue_ha_role` in the last 10 minutes |

Load the file in `prometheus.yml`. The rules work with a default [scrape configuration](metrics.md#scrape-configuration) under any scrape job name: they select no job and do not need `honor_labels`. By default Prometheus renames the `job` label of per-job series (the MongoRescue job ID) to `exported_job`; the rules group by `job` and `exported_job` and name the job from whichever holds it, so `honor_labels: true` works too.

```yaml
rule_files:
  - /etc/prometheus/rules/mongorescue-alerts.yml
```

`MongoRescueDown` recognises MongoRescue targets by `mongorescue_build_info` instead of a job name, so it needs nothing to edit; an instance that has been down for more than 7 days drops out of it (the external heartbeat keeps alerting), and a target that was never scraped successfully is not covered. Prometheus only notices that MongoRescue is down while Prometheus itself runs; the external heartbeat covers the case where both are on the same failed host.

Every rule carries a `runbook_url` annotation that points at its section of the [incident runbooks](runbooks.md) (`https://yigitcittan.github.io/mongorescue/runbooks/#<alertname in lower case>`): what the alert means, likely causes, the exact commands, API calls and PromQL to diagnose it, the action and when to escalate. Alertmanager passes the annotation to its receivers, so put `{{ .CommonAnnotations.runbook_url }}` (or `{{ range .Alerts }}{{ .Annotations.runbook_url }}{{ end }}`) in your notification templates. If you host the docs yourself, rewrite the base URL when you load the rules (for example with `sed`); a test keeps the shipped URLs and the runbook anchors in step. Suggested SLOs built on the same metrics are in [production.md](production.md#service-level-objectives), and a hand-sized container limit in [production.md](production.md#sizing).

## Grafana dashboard

[`deploy/grafana/mongorescue.json`](https://github.com/YigitCittan/mongorescue/blob/main/deploy/grafana/mongorescue.json) is a dashboard for Grafana 10 and later over the same metrics. Import it under **Dashboards → New → Import → Upload JSON file** and pick the Prometheus data source that scrapes MongoRescue; it needs nothing else. Variables: **Data source**, **Instance**, **Job** (the MongoRescue job ID), **Database** and **PITR stream**. Its rows:

| Row | Shows |
| :--- | :--- |
| Overview | Instances scraped, active HA instances, scheduler tick age, RPO breaches, failed backups, firing MongoRescue alerts, and the RPO compliance, backup success and restore-test success of the selected range (the [SLIs](production.md#service-level-objectives)) |
| Backups | Backups and job runs by outcome, duration p95, last backup size, throughput, retention deletions, time since the last successful backup per database |
| Recovery point objectives | Age of the newest backup against each job's objective, and whether it is met |
| Restores, verification and restore tests | Restores, verifications, restore tests by outcome, time since the last passed restore test |
| Point-in-time recovery | Lag, oplog headroom, collector state, chunks, chain breaks, recovery window length |
| Copies (3-2-1) | Copies by result, copy queue depth |
| Storage and disk | Free space of the data directory and local targets, days of space left, missing and orphan archives, scan and metadata backup age, process memory |
| High availability | Role, lease epoch and health, lease errors, takeovers, step-downs, refusals and refused snapshots |
| API rate limits, notifications and keys | `429` responses by caller and budget, MCP calls, bulk items, notifications, dropped events and heartbeats, audit log, API key expiry, KMS errors, settings warnings |

The job and database variables read the `exported_job` label, which Prometheus sets when `honor_labels` is `false` (the default); with `honor_labels: true` replace `exported_job` by `job` in the dashboard's queries. The *Firing MongoRescue alerts* tile needs the shipped rules loaded in the same Prometheus, which exposes them as `ALERTS`.

On Kubernetes the chart can ship it as a ConfigMap for the Grafana sidecar (`grafana.dashboard.enabled`, [kubernetes.md](kubernetes.md#monitoring)). A test checks that every metric the dashboard queries exists in `internal/metrics`, and the chart's copy is checked against the original in CI.

## Recommended external services

Any service that alerts when an HTTP `GET` ping stops arriving works for the global heartbeat. The job heartbeats need one that understands the `/start` and `/fail` paths:

- [healthchecks.io](https://healthchecks.io) supports both, hosted or self-hosted (it is open source). It is the reference for the protocol above.
- Other heartbeat services with a plain ping URL, such as Better Stack heartbeats, Cronitor heartbeat monitors, Uptime Kuma push monitors (self-hosted) or Dead Man's Snitch, work as the global heartbeat. Check their documentation before using them for job heartbeats: a service without the `/start` and `/fail` paths answers them with an error, which MongoRescue logs and does not retry.

Run the monitor somewhere other than MongoRescue's host (a hosted service, or another machine), or it fails together with MongoRescue.
