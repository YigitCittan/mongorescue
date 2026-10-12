# Notifications

MongoRescue sends a message when a backup or restore finishes. Channels and rules are managed in the dashboard (Notifications tab) or through the REST API, and stored in the metadata database alongside jobs; channel secrets are encrypted there with the secret key (see [production.md](production.md#data-directory)).

## Channels

<img src="assets/screenshots/notifications.png" alt="Notification channels and rules in the dashboard" width="800">

| Type | `type` | Settings (JSON object) | Secrets |
| :--- | :--- | :--- | :--- |
| Webhook | `webhook` | `webhook: {url, headers, secret}` | `secret`, credential-bearing headers |
| Telegram bot | `telegram` | `telegram: {bot_token, chat_id, parse_mode}` | `bot_token` |
| Email (SMTP) | `email` | `email: {host, port, username, password, from, to, security}` | `password` |
| SMS (Twilio) | `twilio` | `twilio: {account_sid, auth_token, from, to}` | `auth_token` |

- **Webhook** POSTs the JSON payload below to any `http://` or `https://` URL. It covers Slack incoming webhooks (the payload carries a `text` field), Discord (append `/slack` to the Discord webhook URL to use its Slack-compatible endpoint), and SMS gateways or ticketing systems that accept a JSON POST. Extra `headers` can carry an `Authorization` token.
- **Telegram** uses a bot token from @BotFather and a chat ID. `parse_mode` is empty (plain text) or `MarkdownV2`.
- **Email** `security` is `none` (plain TCP, only for local relays), `starttls` (usually port 587) or `tls` (implicit TLS, usually port 465). `to` is a list of addresses.
- **Twilio** sends an SMS to each number in `to` from the Twilio number `from`.

Secrets are masked (`******`) in every API response. When you update a channel, sending a masked value back keeps the stored secret. Changing a channel's type or destination requires re-entering its secrets, so a stored credential can never be redirected to a new endpoint without being supplied again.

Webhook and e-mail destinations may be public hosts, `localhost` or private networks (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`), since self-hosted receivers usually live there. Addresses that are never a receiver are refused: link-local addresses, including the cloud metadata services (`169.254.169.254`, `fd00:ec2::254`, `100.100.100.200`), unspecified, multicast and broadcast addresses, in any form (IPv4-mapped IPv6 and NAT64 `64:ff9b::/96` included). Numeric hosts must be written in dotted decimal (`127.0.0.1`, not `2130706433`, `0x7f000001` or `127.1`). The check runs when a channel is saved and again on every connection after DNS resolution, so a host name that resolves to a refused address fails as well. Webhooks honour `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY`; through a proxy, the target host is resolved and checked before the request is handed over, and a name the local resolver cannot resolve is left to the proxy. Webhook redirects are never followed.

Each channel has a **Send test** button (`POST /api/v1/notifications/channels/{id}/test`) that delivers a `notification.test` event and reports the result immediately. The last delivery outcome is shown on the channel (`last_delivery`).

## Rules

A rule connects events to channels:

```json
{
  "name": "Page on failures",
  "enabled": true,
  "events": ["backup.failed", "restore.failed"],
  "job_ids": ["nightly-shop"],
  "channel_ids": ["ops-webhook", "oncall-sms"]
}
```

- `events`: any of `backup.succeeded`, `backup.failed`, `backup.skipped` (opt-in: only rules that name it receive it; sent once per gap of scheduled runs that fell outside their job's [backup window](production.md#backup-windows), so at most once a day for a daily window; `job_id`, `run_id`, `status: "skipped"`, `detail: "skipped (outside window)"`; not a failure), `restore.succeeded`, `restore.failed`, `verification.failed`, `restore_test.succeeded`, `restore_test.failed`, `storage.drift_detected`, `retention.deleted` (see [verification.md](verification.md#notifications-and-metrics)), `metadata_backup.failed` (a [metadata snapshot](production.md#metadata-backups) could not be stored; `target_id`, `error`), `restore.verification_failed` (a restore with `verify_restore` did not match the backup's manifest; `restore_id`, `database` is the restore target, the first mismatch in `detail`; see [api.md](api.md#restore-verification)), `job.databases_added` (a job that includes new databases automatically backed up new ones; `databases` names them), `job.rpo_missed` (the newest successful backup of one of a job's databases became older than the job's [recovery point objective](api.md#recovery-point-objectives); `job_id`, `database`, the age and the objective in `detail`; sent once per breach, also across restarts), `job.rpo_recovered` (that database has a recent enough backup again), `security.destructive_action` (a backup deleted, purged or unpinned, a storage target deleted, a retention shortening or a lower grace period scheduled or applied, the two-person rule turned off; `action`, `actor` and what happened in `detail`, see [security.md](security.md)) and `security.approval_requested` (a destructive action waits for a second administrator; `approval_id`), `security.key_rotated` (`secret.key`, the backup encryption key or storage credentials were [rotated](encryption.md#key-rotation-runbook); `action` is `secret_key`, `encryption` or `storage_credentials`, `actor`, `approval_id`, the old and new fingerprints in `detail`, never key material), and the [PITR](pitr.md) events `pitr.chain_broken`, `pitr.diverged`, `pitr.lag_high`, `pitr.lag_recovered`, `pitr.window_low`, `pitr.collector_failed` and `pitr.collector_recovered` (`stream`, `connection_id`, what happened in `detail`), and `backup.copy_failed` (copying a backup to one of its [copy targets](configuration.md#copies-on-a-second-target-3-2-1) started failing, or a verification found a copy damaged or gone; `backup_id`, `target_id`, `target_name`, `error`, the next attempt in `detail`) `backup.copy_recovered` (a failing copy succeeded) and `backup.copy_exhausted` (the last automatic attempt of a copy failed; an administrator retries it with `POST /api/v1/backups/{id}/copies/retry`). Drift events belong to no job, so only rules without `job_ids` match them. A job with several databases sends one backup event per run, not one per database: `backup.failed` for a partial run names the failed databases and how many new ones were not included ([details](api.md#jobs-with-several-databases)).
- `job_ids`: optional. Empty means every job, including on-demand backups.
- `channel_ids`: channels that receive matching events.

Security alerts are not tied to rules: *Backup encryption is off* (`security.encryption_off_after_upgrade`, see [encryption.md](encryption.md#encryption-turned-off-by-an-upgrade)) is sent once to every enabled channel, and so is *Data directory full* (`system.disk_full`: a write to the metadata database failed because its disk is full; new backups and restores are refused until space is freed, see [production.md](production.md#disk-space); `error`, `detail`), once per episode and at most once an hour.

## Delivery

Delivery is asynchronous and never blocks or fails a backup. Every delivery (one per event and channel) is stored in the metadata database (`notification_outbox`) before it is sent, and removed once the channel accepted it or it was given up, so a crash, a `SIGKILL`, an OOM kill or a power loss does not lose it: the next start sends it.

- **At least once.** A delivery interrupted after the channel accepted it but before it was removed is sent again after the restart. Every event carries an ID (`event_id` in [webhook payloads](#webhook-payload), the same for every channel and every attempt); receivers that must not act twice drop IDs they have seen.
- **Retries.** Each round makes up to 4 attempts (10 second timeout each, backoff 1s, 2s, 4s). A failed round is retried after 1 minute, doubling up to 1 hour, for 8 rounds (about 2 hours), also across restarts; then the delivery is given up and counted as `failure`. Permanent failures such as an HTTP 4xx other than 408/429 are given up at once. A channel that cannot be loaded (its secrets cannot be decrypted, for example after `secret.key` was replaced, or its stored row is damaged) counts rounds the same way, so it is given up after the same limit, and the settings report the `notification_channel_unreadable` warning naming it until it delivers again; edit and save the channel (re-entering its secrets) or delete it.
- **Order and head-of-line blocking.** The deliveries of a channel are sent one at a time, oldest first: a channel that is down holds its later notifications back until the oldest one is delivered or given up (8 rounds, about 2 hours). Channels do not wait for each other, so give critical alerts a channel of their own rather than sharing one with a noisy or flaky receiver.
- **Staleness.** A delivery that waited longer than `MONGORESCUE_NOTIFICATION_MAX_AGE` (`-notification-max-age`, 24 hours by default) since it was queued, behind its channel's older deliveries or across a long downtime, is dropped as stale, logged as a warning and counted as `expired`.
- **Cap.** At most 10,000 deliveries wait; beyond that the oldest are dropped, logged as a warning and counted as `dropped`.
- **What is stored.** The event (its fields are redacted like every event: no connection credentials) and the channel ID. The message is rendered, and the channel's URL, token or password read, only when it is sent, so the queue holds no channel secret.
- **Disabling or deleting a channel discards its backlog.** Its queued deliveries are dropped when they come up, not kept for when it is enabled again.
- **The remaining window.** An event reaches the queue through the in-process event bus, a few milliseconds after the action it reports was committed; a crash inside that window loses the event (the action itself is in the audit log). Backup and restore runs interrupted by a crash are reported as failed at the next start.
- **Without the database.** If the queue cannot be written (a full data disk, see [production.md](production.md#disk-space)), deliveries are sent from memory, as before this queue existed, and are lost if the process dies; if that in-memory queue is full, the delivery is dropped.

Every delivery outcome is counted in `mongorescue_notifications_total{status}` (`success`, `failure`, `dropped`, `expired`); events the bus itself had to drop are counted in `mongorescue_events_dropped_total` (see [metrics.md](metrics.md)).

Error messages in notifications are redacted: MongoDB credentials never appear in a payload.

## Webhook payload

Every webhook request is a `POST` with `Content-Type: application/json`, an `X-MongoRescue-Event` header carrying the event type, and, when a secret is configured, an `X-MongoRescue-Signature` header.

```json
{
  "version": 1,
  "event_id": "evt_3f9a1c2e5b7d9f01",
  "event": "backup.failed",
  "time": "2026-09-24T03:00:00Z",
  "job_id": "nightly-shop",
  "backup_id": "bkp_shop_20260924_030000_3f9a1c2e",
  "database": "shop",
  "status": "failed",
  "error": "mongodump: exit status 1",
  "duration_seconds": 12.5,
  "subject": "❌ Backup failed: job nightly-shop (db shop)",
  "text": "❌ Backup failed: job nightly-shop (db shop) — mongodump: exit status 1 — 2026-09-24T03:00Z"
}
```

| Field | Notes |
| :--- | :--- |
| `version` | Payload schema version, currently `1`. Breaking changes will bump it. |
| `event_id` | Identifies the event: the same for every channel, every retry and a re-delivery after a restart. Deliveries are at least once, so use it to drop duplicates. Omitted for test messages |
| `event` | `backup.succeeded`, `backup.failed`, `backup.cancelled`, `backup.skipped`, `restore.succeeded`, `restore.failed`, `verification.failed`, `restore_test.succeeded`, `restore_test.failed`, `storage.drift_detected`, `retention.deleted`, `job.databases_added`, `metadata_backup.failed`, `restore.verification_failed`, `job.rpo_missed`, `job.rpo_recovered`, `security.destructive_action`, `security.approval_requested`, `security.key_rotated`, `pitr.chain_broken`, `pitr.diverged`, `pitr.lag_high`, `pitr.lag_recovered`, `pitr.window_low`, `pitr.collector_failed`, `pitr.collector_recovered`, `backup.copy_failed`, `backup.copy_recovered`, `backup.copy_exhausted`, `security.encryption_off_after_upgrade`, `system.disk_full`, or `notification.test` |
| `action`, `actor`, `approval_id` | Security events: the destructive action (such as `delete_backup` or `purge`), who took or requested it, and the approval request |
| `stream`, `connection_id` | `pitr.*` events: the [PITR stream](pitr.md) and its connection |
| `run_id`, `run` | Backup events of a job run: the run and its summary (`status`: `ok`, `partial`, `failed`, `cancelled`, `skipped`; `multi`, `databases`, `succeeded`, `failed`, `cancelled`, `failed_databases`, `new_databases`) |
| `databases` | `job.databases_added`: the databases added |
| `verification`, `source` | Verification and restore test events: the outcome (`ok`, `mismatch`, `error`) and what triggered it |
| `target_id`, `orphans`, `missing` | Drift events: the storage target and what its scan found |
| `detail` | A short redacted explanation (the retention rule, a temporary database that could not be dropped) |
| `time` | RFC 3339, UTC |
| `job_id` | Omitted for on-demand backups |
| `backup_id`, `restore_id` | `restore_id` only on restore events |
| `database` | Backup source or restore target |
| `error` | Redacted; omitted on success |
| `size_bytes` | Backup events only |
| `subject`, `text` | Rendered human-readable summary and body |

## Verifying the signature

The signature is `sha256=` followed by the hex HMAC-SHA256 of the raw request body, keyed with the channel secret. Compute it over the exact bytes received, before any JSON parsing, and compare in constant time.

**Shell (openssl)**

```bash
# body.json holds the raw request body; SIG is the X-MongoRescue-Signature header value.
expected="sha256=$(openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex < body.json | sed 's/^.*= //')"
[ "$expected" = "$SIG" ] && echo "valid" || echo "INVALID"
```

**Go**

```go
func verify(secret string, body []byte, header string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}

func handler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || !verify(os.Getenv("WEBHOOK_SECRET"), body, r.Header.Get("X-MongoRescue-Signature")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	// json.Unmarshal(body, &payload) ...
	w.WriteHeader(http.StatusNoContent)
}
```

## API

| Method | Endpoint | Description |
| :--- | :--- | :--- |
| `GET` | `/api/v1/notifications/channels` | List channels (secrets masked) |
| `POST` | `/api/v1/notifications/channels` | Create a channel |
| `PUT` | `/api/v1/notifications/channels/{id}` | Replace a channel's configuration |
| `DELETE` | `/api/v1/notifications/channels/{id}` | Delete a channel |
| `POST` | `/api/v1/notifications/channels/{id}/test` | Send a test notification |
| `GET` | `/api/v1/notifications/rules` | List rules |
| `POST` | `/api/v1/notifications/rules` | Create a rule |
| `PUT` | `/api/v1/notifications/rules/{id}` | Replace a rule |
| `DELETE` | `/api/v1/notifications/rules/{id}` | Delete a rule |

Example: create a signed webhook channel.

```bash
curl -s -X POST https://backup.example.com/api/v1/notifications/channels \
  -H "Authorization: Bearer $MONGORESCUE_KEY" -H 'Content-Type: application/json' \
  -d '{"name": "Ops webhook", "type": "webhook", "enabled": true,
       "webhook": {"url": "https://hooks.example.com/mongorescue", "secret": "change-me"}}'
```

The test endpoint makes outbound requests to whatever destination a channel names. Only give accounts and API keys to people and systems you trust with outbound network access from the MongoRescue host; see [production.md](production.md).
