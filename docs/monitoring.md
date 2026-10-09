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

`GET /api/v1/health` (no authentication) answers `503` with `"scheduler": "stale"` when the scheduler's last liveness tick is older than 90 seconds (three ticks), and `200` with `"scheduler": "ok"` and `scheduler_last_tick` otherwise; `status` keeps its value, so checks must read the HTTP status or `scheduler` (see [api.md](api.md#health)). The Docker image's `HEALTHCHECK` uses it, so a container with a hung scheduler is reported unhealthy; point load balancer and uptime checks at it too.

### Clock steps

The liveness tick runs every 30 seconds on a monotonic timer of its own, not on the cron that starts jobs, and its age is measured on the monotonic clock. A wall clock step (an NTP correction, a restored VM snapshot, a manual change) therefore never delays the tick nor makes the scheduler stale: the health check stays `200`, the heartbeat keeps pinging and `MongoRescueSchedulerStale` does not fire. Only a scheduler that cannot take its own lock (a deadlock) or a stopped one stops ticking.

Scheduled jobs follow the wall clock, since cron expressions name wall-clock times:

- **Forward step.** Every job whose next run the clock jumped over runs once, late, when the scheduler next wakes up (within about a minute: its own checks run every minute), and then on its schedule from the new time. Missed runs are not caught up one by one.
- **Backward step.** Jobs wait until the clock reaches their next run again, so their runs are delayed by the size of the step. A step never repeats a run by itself. A job whose schedule is rebuilt after the step (it is edited or re-enabled, or MongoRescue restarts) could be due again at a time that already ran; MongoRescue skips such a trigger when the job's previous scheduled run started less than half its interval earlier (measured on the monotonic clock, or after a restart on the start time of the newest scheduled run on record), and logs `skipping a repeated scheduled run`. After a restart at most one trigger is skipped this way.

## Prometheus alert rules

[`deploy/prometheus/alerts.yml`](../deploy/prometheus/alerts.yml) holds alerting rules over the [metrics](metrics.md), unit-tested with `promtool test rules` in CI ([`alerts_test.yml`](../deploy/prometheus/alerts_test.yml); run them with `make test-prometheus-rules`):

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

Load the file in `prometheus.yml`. The rules work with a default [scrape configuration](metrics.md#scrape-configuration) under any scrape job name: they select no job and do not need `honor_labels`. By default Prometheus renames the `job` label of per-job series (the MongoRescue job ID) to `exported_job`; the rules group by `job` and `exported_job` and name the job from whichever holds it, so `honor_labels: true` works too.

```yaml
rule_files:
  - /etc/prometheus/rules/mongorescue-alerts.yml
```

`MongoRescueDown` recognises MongoRescue targets by `mongorescue_build_info` instead of a job name, so it needs nothing to edit; an instance that has been down for more than 7 days drops out of it (the external heartbeat keeps alerting), and a target that was never scraped successfully is not covered. Prometheus only notices that MongoRescue is down while Prometheus itself runs; the external heartbeat covers the case where both are on the same failed host.

## Recommended external services

Any service that alerts when an HTTP `GET` ping stops arriving works for the global heartbeat. The job heartbeats need one that understands the `/start` and `/fail` paths:

- [healthchecks.io](https://healthchecks.io) supports both, hosted or self-hosted (it is open source). It is the reference for the protocol above.
- Other heartbeat services with a plain ping URL, such as Better Stack heartbeats, Cronitor heartbeat monitors, Uptime Kuma push monitors (self-hosted) or Dead Man's Snitch, work as the global heartbeat. Check their documentation before using them for job heartbeats: a service without the `/start` and `/fail` paths answers them with an error, which MongoRescue logs and does not retry.

Run the monitor somewhere other than MongoRescue's host (a hosted service, or another machine), or it fails together with MongoRescue.
