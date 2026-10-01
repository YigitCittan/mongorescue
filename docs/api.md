# REST API

Every response uses the envelope `{"success": bool, "data": ..., "error": "..."}`.

## Authentication

There is no unauthenticated mode. Apart from the public routes below, every request needs either

- a **session**: the `mr_session` cookie set by `POST /api/v1/setup` or `POST /api/v1/auth/login` (HttpOnly, `SameSite=Strict`, `Secure` over HTTPS). Unsafe methods (`POST`, `PUT`, `PATCH`, `DELETE`) must also send the session's CSRF token as `X-CSRF-Token`, or they are rejected with `403`; or
- an **API key**: `Authorization: Bearer <key>` or `X-API-Key: <key>`. Keys are created under Settings → API keys (`mr_<prefix>_<secret>`, shown once); a `MONGORESCUE_API_KEY` of an earlier build is imported once as a key. API-key requests need no CSRF token.

Public routes: `/` and the dashboard assets, `GET /api/v1/health`, `GET /api/v1/setup/status`, `POST /api/v1/setup` and `POST /api/v1/auth/login`. `GET /api/v1/auth/me` answers signed-out visitors with `200` and an empty session (`{"user": null, "csrf_token": "", "auth": ""}`); every other protected route answers `401`. `/metrics` takes an API key (not a session) unless the `security.metrics_public` setting is on. `/mcp`, the [MCP endpoint](mcp.md), takes an API key only, never a session.

### API key scopes

Every API key has a scope, chosen when it is created (`read` when omitted); sessions are always admin. The auth middleware checks the scope of every request against one route table (`internal/server/scopes.go`) and answers `403` with the key's and the required scope when it is too small:

| Scope | Allowed |
| :--- | :--- |
| `read` | Every `GET` route except `GET /api/v1/audit` and `GET /api/v1/users`, plus `/metrics` and the MCP endpoint (read tools only) |
| `operator` | `read` plus `POST /api/v1/backups`, `POST /api/v1/backups/{id}/retry`, `POST /api/v1/jobs/{id}/run`, `POST /api/v1/restore` into a safe clone on the backup's own connection, and cancelling backups and restores that are not in place (`POST /api/v1/backups/{id}/cancel`, `POST /api/v1/restores/{id}/cancel`), plus the MCP action tools |
| `admin` | Everything: deletions, in-place and cross-connection restores, jobs, connections, storage targets, notifications, settings, users (including the user list), API keys and the audit log |

An in-place restore (`"safe_clone": false` or a `target_database`) and a restore into another connection than the backup's (`target_connection_id`) need `admin` even though the route itself needs `operator`, and so does cancelling a running in-place restore (it may leave the target partially restored). Keys created before scopes existed (and a key imported from `MONGORESCUE_API_KEY`) are `admin` keys.

Failed logins return a generic `401`. After 5 failures for the same (client IP, username), further attempts for that pair get `429 Too Many Requests` with `Retry-After`; the lockout starts at 30 seconds and doubles up to 15 minutes. Once an IP has 20 recent failures, each further username from it is locked after a single failure, but the correct password of a user that is not locked always works, so clients sharing one address (NAT, a proxy) cannot lock each other out. Only one attempt per (IP, username) is checked at a time, and at most four per IP for usernames that already failed; concurrent extras get `429` with `Retry-After: 2`. Password comparisons share a global pool (twice the number of CPUs): a login waits up to 5 seconds for a free slot rather than being refused. Wrong setup codes are throttled only after 100 per IP within 15 minutes, and the correct code is always accepted. Passwords longer than 72 bytes are rejected without a password check.

`POST /api/v1/setup` and `POST /api/v1/auth/login` require `Content-Type: application/json` (`415` otherwise) and, when the request has an `Origin` header, it must be this server's host or a configured CORS origin (`403` otherwise). This blocks cross-site form posts (login CSRF).

Sessions end after the `security.session_idle_timeout` without requests (default 12 hours) or the `security.session_absolute_timeout` after login (default 7 days), on logout, and when the user's password changes (other sessions) or the user is deleted. Deleting a user also revokes the API keys they created.

## Endpoints

| Method | Endpoint | Description | Success | Errors |
| :--- | :--- | :--- | :--- | :--- |
| `GET` | `/api/v1/health` | Health check (`status`, `version`, `time`) | 200 | |
| `GET` | `/api/v1/setup/status` | `{"setup_required": bool}` | 200 | |
| `POST` | `/api/v1/setup` | `{setup_code, username, password}` → first user and session: `{user, csrf_token}` | 201 | 400, 403 wrong code, 409 already set up, 429 |
| `POST` | `/api/v1/auth/login` | `{username, password}` → `{user, csrf_token}` | 200 | 401, 403 foreign origin, 415, 429 |
| `POST` | `/api/v1/auth/logout` | Revoke the current session | 200 | |
| `GET` | `/api/v1/auth/me` | `{user, csrf_token, auth: "session"\|"api_key"}`; signed out: `{user: null, csrf_token: "", auth: ""}` | 200 | |
| `GET` / `POST` | `/api/v1/users` | List / create users `{username, password}`; admin only | 200 / 201 | 400, 403, 409 username taken |
| `DELETE` | `/api/v1/users/{id}` | Delete a user and revoke their sessions and API keys | 200 | 400 yourself, 404, 409 last user |
| `PUT` | `/api/v1/users/{id}/password` | `{current_password, new_password}` (current required for your own account) | 200 | 400, 403 wrong current password, 404 |
| `GET` / `POST` | `/api/v1/api-keys` | List keys / create `{name, scope}` (`scope`: `read` (default), `operator` or `admin`) → `{api_key, key}` (plaintext only here) | 200 / 201 | 400 |
| `DELETE` | `/api/v1/api-keys/{id}` | Revoke a key | 200 | 404 |
| `GET` | `/api/v1/audit` | Recent API key activity (MCP calls and REST requests), newest first (`?limit=` 1-1000, default 200); admin only | 200 | 400, 403 |
| `GET` / `POST` | `/api/v1/connections` | List / create `{name, uri, description}` | 200 / 201 | 400 |
| `GET` / `PUT` | `/api/v1/connections/{id}` | Get / update a connection | 200 | 400, 404 |
| `DELETE` | `/api/v1/connections/{id}` | Delete a connection | 200 | 404, 409 used by jobs |
| `POST` | `/api/v1/connections/{id}/test` | Test a saved connection → `{ok, server_version, latency_ms, error}` | 200 | 404 |
| `POST` | `/api/v1/connections/test` | Test an unsaved `{uri}` | 200 | 400 |
| `GET` | `/api/v1/connections/{id}/databases` | `[{name, size_bytes, empty}]`; `admin`, `config`, `local` only with `?system=true` | 200 | 404, 502 unreachable |
| `GET` | `/api/v1/connections/{id}/databases/{db}/collections` | `[{name, type}]` | 200 | 404, 502 |
| `GET` | `/api/v1/stats` | Dashboard KPIs over every record: counts, `failed_backups_24h`, `total_restores`, `last_backup`, `job_last_backups` ([details](#listing-backups-and-restores)) | 200 | |
| `GET` | `/api/v1/settings` | All settings, secrets masked: `{general, security, encryption, restart_required, warnings}` | 200 | |
| `PUT` | `/api/v1/settings` | Partial update, e.g. `{"general": {...}}`; returns the full settings | 200 | 400 |
| `POST` | `/api/v1/settings/encryption/generate-key` | New X25519 key pair `{identity, recipient}` (not stored) | 200 | |
| `POST` | `/api/v1/settings/warnings/{id}/dismiss` | Dismiss a persistent warning for good; returns the remaining `{warnings}` | 200, 404 | |
| `GET` / `POST` | `/api/v1/storage-targets` | List / create `{name, type, local \| s3}` (the first target becomes the default) | 200 / 201 | 400 |
| `GET` / `PUT` | `/api/v1/storage-targets/{id}` | Get / update a target | 200 | 400, 404, 409 changed meanwhile or location locked |
| `DELETE` | `/api/v1/storage-targets/{id}` | Delete a target | 200 | 404, 409 default or in use |
| `POST` | `/api/v1/storage-targets/{id}/test` | Test a saved target → `{ok, latency_ms, error}` | 200 | 404 |
| `POST` | `/api/v1/storage-targets/test` | Test an unsaved target (plus `id` when editing, for its masked secret) | 200 | 400 |
| `POST` | `/api/v1/storage-targets/{id}/default` | Make the target the default | 200 | 404 |
| `GET` | `/api/v1/jobs` | List scheduled jobs | 200 | |
| `POST` | `/api/v1/jobs` | Create or update a job (`connection_id` and `database` required; `storage_target_id` optional; the cron expression is validated) | 201 | 400 |
| `GET` | `/api/v1/jobs/{id}` | Get a job with its next three activations (`next_runs`, UTC) | 200 | 404 |
| `PUT` | `/api/v1/jobs/{id}` | Update a job and reschedule it at once ([details](#updating-a-job)) | 200 | 400, 404, 409 changed meanwhile |
| `DELETE` | `/api/v1/jobs/{id}` | Delete a job | 200 | 404 |
| `POST` | `/api/v1/jobs/{id}/run` | Run a job now | 202 | 400, 404, 409 |
| `GET` | `/api/v1/backups` | List backups, newest first; filters, sorting and pagination ([details](#listing-backups-and-restores)) | 200 | 400 |
| `GET` | `/api/v1/backups/databases` | Distinct database names of all backups, sorted (for filters) | 200 | |
| `POST` | `/api/v1/backups` | Start a backup `{connection_id, database, collections \| exclude_collections, storage_target_id, gzip}` | 202 | 400, 409 |
| `DELETE` | `/api/v1/backups/{id}` | Delete a backup and its artifact on the backup's storage target | 200 | 404 |
| `POST` | `/api/v1/backups/{id}/retry` | Retry a failed backup with its parameters; the new record's `retry_of` is `{id}` ([details](#retrying-a-failed-backup)) | 202 | 404, 409 not failed or already running, 422 connection or target gone |
| `GET` | `/api/v1/backups/{id}/collections` | Collections stored in the backup, read from its archive header ([details](#selective-restores)) | 200 | 404, 422 key missing |
| `POST` | `/api/v1/backups/{id}/cancel` | Cancel a running backup ([details](#cancelling-a-run)) | 202 | 404, 409 not running or finishing |
| `GET` | `/api/v1/backups/{id}/log` | The backup's run log as `text/plain`: the last N lines with `?tail=N` (1-10000), otherwise the whole file as a download ([details](#run-logs)) | 200 | 400, 404 |
| `POST` | `/api/v1/restore` | Restore (safe clone by default; optional `selected_collections`, `target_connection_id` (admin), `verify`) | 202 | 400, 403, 404, 409, 422 |
| `GET` | `/api/v1/restores` | Restore audit history, newest first; filters, sorting and pagination ([details](#listing-backups-and-restores)) | 200 | 400 |
| `GET` | `/api/v1/restores/databases` | Distinct target databases of all restores, sorted | 200 | |
| `POST` | `/api/v1/restores/{id}/cancel` | Cancel a running restore; an in-place one needs admin ([details](#cancelling-a-run)) | 202 | 403, 404, 409 not running or finishing |
| `GET` | `/api/v1/restores/{id}/log` | The restore's run log (like the backup log) | 200 | 400, 404 |
| `GET` | `/api/v1/runs/active` | Live progress of every running backup and restore ([details](#live-progress)) | 200 | |
| `GET` / `POST` | `/api/v1/notifications/channels` | List / create notification channels | 200 / 201 | 400 |
| `PUT` / `DELETE` | `/api/v1/notifications/channels/{id}` | Update / delete a channel | 200 | 400, 404 |
| `POST` | `/api/v1/notifications/channels/{id}/test` | Send a test notification | 200 | 404 |
| `GET` / `POST` | `/api/v1/notifications/rules` | List / create notification rules | 200 / 201 | 400 |
| `PUT` / `DELETE` | `/api/v1/notifications/rules/{id}` | Update / delete a rule | 200 | 400, 404 |
| `GET` | `/metrics` | Prometheus metrics | 200 | 401 |
| `POST` | `/mcp` | [MCP](mcp.md) Streamable HTTP endpoint (JSON-RPC; stateless, so `GET` and `DELETE` answer 405); API keys only | 200 | 401, 403 disabled or foreign origin |

Every protected endpoint also answers `401` without valid credentials, `403` for a cookie request with a missing or wrong `X-CSRF-Token` and `403` for an API key whose scope is too small.

Database and collection names sent to create or update a job, start a backup, or name an in-place restore target and selected collections must follow MongoDB's cross-platform rules: a database name has 1 to 63 bytes, none of `/ \ . " $`, a space or a control character, and does not start with `-`; a collection name is not empty and has no `$` or control character. Other names are refused with `400` (*invalid namespace*). Scheduled runs of existing jobs, retries and restores of existing backups are never refused for their names. Backups are always stored under a key the server derives from the database name; clients cannot choose it.

## Connections

Connection strings are validated, encrypted at rest and only ever returned redacted (`mongodb://user:******@host/...`). To keep the stored password when editing, send the URI back exactly as it was returned; any other value containing `******` is rejected. A connection test succeeds or fails with HTTP `200` (`ok: false` plus a redacted `error`) and times out after 10 seconds.

Jobs and manual backups name a `connection_id`. Backup records keep `connection_id` and a `connection_name` snapshot. A restore goes to the backup's connection unless `target_connection_id` names another one, which restores across servers and needs `admin`; restore records always carry `source_connection_id`, `source_connection_name`, `target_connection_id` and `target_connection_name`, the names as they were when the restore started.

## Settings

`GET /api/v1/settings` returns every setting grouped as `general`, `security` and `encryption` ([descriptions](configuration.md#settings)), plus `restart_required` (always empty: every change applies to the next operation or request) and `warnings`, the persistent notices the dashboard shows as a banner: `[{id, message, setting}]`. The only warning so far is `encryption_off_after_upgrade` (see [encryption.md](encryption.md#encryption-turned-off-by-an-upgrade)). Durations are Go duration strings (`"6h0m0s"`), secrets (`encryption.identity`, `encryption.passphrase`) are `"******"` when set and `""` when not, and `encryption.retired_keys` lists `{kind, recipient, retired_at}` without key material.

`PUT /api/v1/settings` takes one or more groups with only the fields to change and answers with the full settings. Unknown fields, invalid values and malformed durations are rejected with `400` and nothing is changed. Sending `"******"` for a secret keeps the stored value; `""` removes it. A replaced or removed identity or passphrase is moved to `retired_keys`, so backups encrypted with it stay restorable.

```bash
curl -X PUT http://localhost:8080/api/v1/settings -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' -d '{"general": {"backup_timeout": "3h", "default_retention_days": 14}}'
```

To encrypt new backups with a fresh key pair, call `POST /api/v1/settings/encryption/generate-key`, store the returned `identity` safely, and send `{"encryption": {"enabled": true, "mode": "x25519", "recipients": ["<recipient>"], "identity": "<identity>"}}`. The passphrase of `passphrase` mode needs at least 16 characters.

## Storage targets

A target is `{id, name, type: "local"|"s3", is_default, local: {path}, s3: {endpoint, region, bucket, prefix, access_key_id, secret_access_key, use_path_style}, created_at, updated_at, last_test_at, last_test_ok, last_test_error}`. Create and update bodies contain only the sub-object of their type; the default is changed only with `POST /api/v1/storage-targets/{id}/default`. The secret access key is returned as `"******"`; sending it back keeps the stored key only while `endpoint`, `bucket` and `access_key_id` are unchanged. Tests answer `200` with `ok: false` and an `error` when the probe fails. `local.path` must be absolute and must not be `/`, the data directory or inside it. Updates are refused with `409` when the target changed meanwhile, and when they would move a target that holds completed or running backups (type, path, endpoint, bucket or prefix); create a new target instead.

Every signed-in user, and every API key with the `admin` scope, has full rights, including settings, storage targets and the connection and storage test endpoints, which connect to hosts named in the request. Roles for users are on the roadmap.

Jobs and manual backups take an optional `storage_target_id` (the default target when omitted). Backup records carry `storage_target_id` and a `storage_target_name` snapshot; restores, deletions and retention use the record's target. Deleting the default target, or a target still used by a job or holding a completed or running backup, answers `409` with the reason.

## Restores

Restore into a safe clone (the default, so `safe_clone` may be omitted):

```bash
curl -s -X POST http://localhost:8080/api/v1/restore \
  -H "Authorization: Bearer $MONGORESCUE_KEY" -H 'Content-Type: application/json' \
  -d '{"backup_id": "bkp_shop_20260924_030000_3f9a1c2e"}'
```

Restoring in place (into the source database, or into `target_database`) must be confirmed explicitly with `{"safe_clone": false, "confirm_in_place": true}`; any other in-place request is rejected with `400 Bad Request` before `mongorestore` starts. In-place restores are always verified first (`verify` and the policy apply to safe clones only). A missing decryption key is rejected up front with `422 Unprocessable Entity`; a checksum mismatch or failed decryption found during verification marks the restore record as failed, and `mongorestore` is never started.

### Selective restores

`selected_collections` restores only the named collections of the backup (one `--nsInclude` each, with `*` and `\` escaped, so a name always means exactly that collection); omitted or empty, the whole database is restored. The restore record repeats the selection in `selected_collections`. With `drop_target`, `mongorestore` drops each collection right before restoring it, so only the selected collections are dropped in the target; its other collections are kept. A view is restored from its definition and reads from its source collection (`view_on`), which is not restored with it unless it is selected too.

```bash
curl -s -X POST http://localhost:8080/api/v1/restore \
  -H "Authorization: Bearer $MONGORESCUE_KEY" -H 'Content-Type: application/json' \
  -d '{"backup_id": "bkp_shop_20260924_030000_3f9a1c2e", "selected_collections": ["orders", "customers"]}'
```

`GET /api/v1/backups/{id}/collections` (read scope) lists what the backup holds, to choose from:

```json
{
  "backup_id": "bkp_shop_20260924_030000_3f9a1c2e",
  "database": "shop",
  "source": "archive",
  "collections": [
    {"name": "big_orders", "type": "view", "view_on": "orders"},
    {"name": "metrics", "type": "timeseries"},
    {"name": "orders", "type": "collection"}
  ]
}
```

The list is read from the prelude of the mongodump archive only: the artifact is streamed from its storage target, decrypted and decompressed on the fly, and the download stops where the prelude ends, so the data of the backup is never transferred. At most 64 MiB of decompressed prelude are read, within 30 seconds, and at most four archives at a time; lists of completed backups are cached in memory per backup. `type` is `collection`, `view` or `timeseries` (system collections are listed too; the dashboard hides them). `size_bytes` appears when the archive records a size (current mongodump versions do not); document counts are not part of the archive header.

When the archive cannot be read (an artifact that is missing, damaged or not a mongodump archive, a timeout) or the backup failed or was pruned (its archive is not read then), `source` is `record`, `warning` gives the reason and `collections` is the backup's own collection filter (empty for a whole-database backup). An encrypted backup without its key is answered the same way when the record has a filter, with the key hint in `warning`; without one, the response is `422` with the same hint as a restore.

## Updating a job

`PUT /api/v1/jobs/{id}` (admin scope, like creating a job) replaces a job's `name`, `cron_expression`, `database`, `collections`, `exclude_collections`, `connection_id` and `storage_target_id`. `retention_days`, `retention_count`, `gzip` and `enabled` are optional: an omitted field keeps the job's current value, so `{"enabled": false, ...}` pauses a job without touching its retention. Pausing with `"paused_until": "<RFC 3339 time>"` resumes the job on its own at that time (the scheduler checks every minute; the time must be in the future, else `400`); pausing without it pauses until the job is resumed, an edit that keeps the job paused keeps its `paused_until`, and resuming (`"enabled": true`) clears it. A paused job's run in progress continues; stop it with [cancel](#cancelling-a-run). The job is validated exactly like a new one: the cron expression must parse (five fields or a descriptor such as `@daily` or `@every 6h`; an empty one means `@daily`), `database` and a known `connection_id` are required, retention must not be negative, and `storage_target_id` must name a target (empty means the default target).

The new schedule takes effect immediately, without a restart: the job's cron entry is replaced, or removed for a disabled job, and `next_run` is recomputed. The id, `created_at`, `last_run` and the job's backups (`GET /api/v1/backups?job_id={id}`) are kept. The response is the updated job. Concurrent updates are stored and scheduled in the same order, and a backup that finishes while the job is being edited only records its run times, so it never reverts the edit.

To avoid overwriting someone else's change, send the job's current `updated_at` (from `GET /api/v1/jobs/{id}`) in the body: the update is refused with `409 Conflict` if the job was changed since. Without `updated_at` the last write wins.

| Status | When |
| --- | --- |
| `400 Bad Request` | Invalid JSON, cron expression, database, retention, connection or storage target |
| `404 Not Found` | No job `{id}` |
| `409 Conflict` | `updated_at` was sent and the job was changed since |

```bash
curl -X PUT http://localhost:8080/api/v1/jobs/job_shop_1727146800_3f9a1c2e \
  -H "X-API-Key: $MONGORESCUE_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name":"shop hourly","cron_expression":"@hourly","database":"shop","connection_id":"conn_prod","retention_count":24}'
```

In the dashboard, clicking a job row (or **Details** in its **⋯** menu) opens the job's details: its schedule in words with the next three runs, retention, compression, encryption, state, and the last 20 runs with their success rate. **Edit** opens the job form prefilled, and **Pause** / **Resume** (also in the row's **⋯** menu) pauses the schedule, until resumed or until a date and time, or resumes it; a paused job shows a *Paused* badge. **Stop current run** cancels the job's running backup. Edits and pauses send `updated_at`, so a job changed elsewhere in the meantime is reported instead of overwritten.

## Retrying a failed backup

`POST /api/v1/backups/{id}/retry` (operator scope, like starting a backup) starts a new manual backup with the parameters of the failed backup `{id}`: the same connection, database, collections and storage target (the default target for records written without one). When the backup belongs to a job that still exists, the job's `exclude_collections` and `gzip` are used; otherwise compression follows the failed backup's storage key. Encryption follows the current settings. The retry is started exactly like `POST /api/v1/backups` (same concurrency limit per database, events and notifications) and the response is the new in-progress record with `"trigger": "manual"` and `"retry_of": "{id}"`.

The failed record is never changed or deleted by a retry: its status, `error_message`, `started_at` and `completed_at` stay in the list, and every attempt links to the one it retries through `retry_of` (omitted for backups that are not retries). A failed retry can be retried again, forming a chain.

| Status | When |
| --- | --- |
| `404 Not Found` | No backup `{id}` |
| `409 Conflict` | The backup has not failed (only `failed` backups can be retried), or a backup of the same database is already running |
| `422 Unprocessable Entity` | The backup's connection or storage target no longer exists, or the backup has no connection recorded |

```bash
curl -X POST http://localhost:8080/api/v1/backups/bkp_shop_20260924_030000_3f9a1c2e/retry \
  -H "X-API-Key: $MONGORESCUE_OPERATOR_KEY"
```

In the dashboard, a failed backup that has not been retried yet shows a **Retry** button, and **Details** opens the full error message (including `mongodump`'s stderr, which the server stores in the redacted error message), the absolute start and end times, the duration, the trigger, connection, database, storage target and the retry chain.

## Listing backups and restores

`GET /api/v1/backups` and `GET /api/v1/restores` take optional query parameters. All given filters must match; empty values are ignored.

| Parameter | Backups | Restores | Meaning |
| --- | --- | --- | --- |
| `id` | yes | | Backups with exactly these IDs, comma-separated (at most 200) |
| `status` | `pending`, `in_progress`, `completed`, `failed`, `cancelled`, `pruned` | `pending`, `in_progress`, `completed`, `failed`, `cancelled` | Records in this state |
| `database` | Backed-up database | Target database | Exact, case-sensitive match |
| `connection_id` | yes | | Backups taken from this connection |
| `job_id` | yes | | Backups of this scheduled job |
| `trigger` | `scheduled`, `on_demand`, `manual`, `mcp` | | How the backup was started (records older than triggers count as `scheduled` when they belong to a job, else `manual`) |
| `retry_of` | yes | | Retries of this backup |
| `backup_id` | | yes | Restores of this backup |
| `from`, `to` | yes | yes | `started_at` range in RFC 3339 (`2026-10-01T00:00:00Z`); `from` is inclusive, `to` exclusive |
| `q` | ID or database | ID, source or target database | Substring, ASCII case-insensitive; `%` and `_` are literal (at most 256 characters) |
| `sort` | yes | yes | `desc` (newest first, default) or `asc` |
| `limit` | yes | yes | Page size, 1 to 200 |
| `offset` | yes | yes | Matches to skip (default 0) |

Without `limit` the response is unchanged from earlier releases: `data` is the array of every matching record. With `limit`, `meta` describes the page:

```json
{
  "success": true,
  "data": [{"id": "bkp_shop_20260924_030000_3f9a1c2e", "status": "failed", "...": "...",
            "retried_by": {"id": "bkp_shop_20260924_031000_9b1d2c3e", "started_at": "2026-09-24T03:10:00Z"}}],
  "meta": {"total": 312, "limit": 25, "offset": 0}
}
```

> **Breaking (API) in 0.9.0:** these parameters are now validated. Earlier releases ignored every query parameter of `GET /api/v1/restores` and every one of `GET /api/v1/backups` except `database` and `job_id`; a malformed `status`, `trigger`, `from`, `to`, `sort`, `limit`, `offset`, `id` (more than 200 IDs) or an over-long `q`, `database`, `connection_id`, `job_id`, `retry_of`, `backup_id` or ID now answers `400` instead of being ignored. Unknown parameter names are still ignored.

`total` counts every match, so a client can show "1–25 of 312"; an `offset` past the end returns an empty `data` with the same `total`. A malformed or out-of-range value (`limit=0`, `limit=500`, `sort=random`, `from=yesterday`, `to` before `from`, an unknown `status` or `trigger`) answers `400` with a message naming the parameter. Backup items carry `retried_by`, the newest backup whose `retry_of` is this one, when there is one; it is computed for each listed row, so retry links work on any page.

```bash
curl "http://localhost:8080/api/v1/backups?status=failed&database=shop&from=2026-09-01T00:00:00Z&limit=25&offset=25" \
  -H "X-API-Key: $MONGORESCUE_READ_KEY"
```

`GET /api/v1/stats` covers every record regardless of any list filter and is computed with SQL aggregates (it does not read every record): `total_backups`, `completed_backups`, `failed_backups`, `failed_backups_24h`, `total_bytes`, `active_backups` and `active_restores` (pending or in progress), `total_restores`, `active_jobs`, `last_backup` (`{id, database, job_id, status, started_at, error_message}` of the newest backup) and `job_last_backups` (each job ID mapped to its newest backup), plus the default storage target. When some figures cannot be read (they then count as zero), the response adds `degraded: true` and a `degraded_reason`. For administrators (dashboard sessions and `admin` keys) it also lists `corrupt_records`: stored rows that every list skips because they cannot be read, as `{table, id, error}` (the error never quotes stored data). See [troubleshooting.md](troubleshooting.md#unreadable-records).

## Cancelling a run

`POST /api/v1/backups/{id}/cancel` and `POST /api/v1/restores/{id}/cancel` (operator scope) stop a running backup or restore, whether it was started through the API, the dashboard, MCP or the scheduler. They answer `202 Accepted` with the record while the run stops (its `progress.cancelling` is `true`); poll it until the status is `cancelled`. `mongodump` or `mongorestore` is terminated with its process group (SIGTERM, then SIGKILL), so no tool process outlives the run.

- A cancelled **backup** deletes its partial artifact (an S3 multipart upload is aborted, a local temporary file removed) and records neither size nor checksum.
- A cancelled **safe-clone restore** drops the partially restored `<db>_rescue_<timestamp>` database; the message says so (or asks to drop it by hand if that failed).
- A cancelled **in-place restore** cannot be undone: the target may be left **partially restored**. The record's `warning` and `error_message` say so loudly. Cancelling one needs `admin`, like starting it.
- A run cancelled before its tool started leaves the target untouched.

The record gets `"status": "cancelled"`, `cancelled_by` (the username, `API key <name>`, `... via MCP`, or `system` for the desktop app's force quit) and `cancelled_at`. Cancelled runs are not failures: they are not counted in `failed_backups` or the last 24 hours' failures, emit `backup.cancelled` / `restore.cancelled` instead of the failure events (notification rules may subscribe to them) and are counted with `status="cancelled"` in the metrics.

| Status | When |
| --- | --- |
| `202 Accepted` | The cancellation was requested |
| `403 Forbidden` | The key may not cancel this run (an in-place restore needs admin) |
| `404 Not Found` | No backup or restore `{id}` |
| `409 Conflict` | The run is not running (it finished, or it is not active in this process), or it is already finishing: its tool completed and only the outcome is being recorded |

Once `mongodump` or `mongorestore` has completed successfully, the run is past the point of no return: a cancellation that arrives then is refused with `409`, and one that raced with the tool's exit does not undo the finished backup or restore. If a check after the tool fails (checksum mismatch, documents that failed to insert) while a cancellation was requested, the run is recorded as cancelled, and a safe clone is dropped.

## Run logs

Every backup and restore writes a log file, `<datadir>/logs/<id>.log`: the complete output of `mongodump` / `mongorestore` and MongoRescue's own timestamped `[mongorescue]` phase lines (start, tool arguments without the connection, phases, outcome, cancellation). Each line is redacted before it is written: connection strings and secrets are masked, document values in duplicate-key errors are replaced, and tool lines are cut to 300 bytes. A log never exceeds 5 MiB: the first 1 MiB and the newest output are kept around a `… N bytes truncated …` marker, and the run streams its log to disk without holding it in memory.

`GET /api/v1/backups/{id}/log` and `GET /api/v1/restores/{id}/log` (read scope) return the log as `text/plain`. With `?tail=N` they return the last `N` lines (1-10000), also while the run is still writing them; without it, the whole file with `Content-Disposition: attachment`. Runs from older releases have no log (`404`). Logs are deleted with their backup record, when retention prunes the backup, and after `general.log_retention_days` (default 30; 0 keeps them).

## Live progress

`GET /api/v1/runs/active` (read scope) returns the progress of every running backup and restore, oldest first; running records in `GET /api/v1/backups` and `GET /api/v1/restores` carry the same object as `progress`:

```json
{"id": "bkp_shop_20261001_100000_3f9a1c2e", "kind": "backup", "job_id": "job_shop", "database": "shop",
 "phase": "dumping", "percent": 41.2, "bytes": 52428800, "documents": 41200,
 "current_collection": "shop.orders", "collections_done": 3, "collections_total": 5,
 "bytes_per_second": 10485760, "started_at": "...", "updated_at": "...",
 "phases": {"queued": "...", "started": "..."}}
```

Bytes count what was streamed to storage (backups) or read from it (restores, with `total_bytes` the archive size). Documents, collections and the backup percentage come from the tools' progress lines (the percentage covers the collections the tool has started so far); a restore's percentage is the share of the archive read. `phase` is `queued`, `dumping`, `verifying`, `restoring`, `finishing` or `cancelling`. Every record also stores the timestamps of its `phases` (`queued`, `started`, `dump_done`, `upload_done`, `verify_done`, `restore_done`, `finished`); records from older releases have `queued` and `finished` only. The dashboard polls every 2 seconds while something runs, shows a progress bar on running rows and, in the details dialog, the progress, a phase timeline and a **Log** tab that follows the log of a running run.

## Asynchronous operations

Every backup record has a `trigger`: `scheduled` (a cron run of `job_id`), `on_demand` (`POST /api/v1/jobs/{id}/run`), `manual` (`POST /api/v1/backups`) or `mcp` (an assistant's `start_backup` or `run_job`). It is set by the server, never taken from the request, and retention only prunes a job's `scheduled` backups ([configuration.md](configuration.md#general)).

`POST /api/v1/backups`, `POST /api/v1/backups/{id}/retry`, `POST /api/v1/jobs/{id}/run` and `POST /api/v1/restore` return `202 Accepted` as soon as the operation has started. The response body is the new backup or restore record with `"status": "in_progress"`; poll `GET /api/v1/backups` or `GET /api/v1/restores` until it becomes `completed`, `failed` or `cancelled`. Operations keep running if the client disconnects; [cancel](#cancelling-a-run) them to stop them.

| Status | Meaning |
| --- | --- |
| `202 Accepted` | The operation started |
| `400 Bad Request` | Invalid request, for example a missing `connection_id`, an unknown `storage_target_id` or an unconfirmed in-place restore |
| `401 Unauthorized` | Not signed in and no valid API key |
| `403 Forbidden` | The API key's scope does not allow the operation (for example a read key starting a backup, or an operator key restoring in place) |
| `409 Conflict` | A backup of the same database on the same connection, or a restore into the same target, is already running |
| `422 Unprocessable Entity` | The backup is encrypted and no decryption key is configured |
| `503 Service Unavailable` | The server is shutting down, or the desktop app is quitting once its running backups and restores finish (*MongoRescue is shutting down*) |

## Audit log

`GET /api/v1/audit` (admin) returns the most recent activity of API keys, newest first: [MCP](mcp.md) tool calls, resource reads (`tool` is `resources/read`, the URI in `arguments.resource`) and prompt requests (`prompts/get`), and REST requests to `/api/...` authenticated by an API key. Browser sessions and `/metrics` scrapes are not audited.

```json
{"id": 42, "time": "2026-09-25T10:15:03Z", "api_key_id": "key_1a2b3c4d5e6f7a8b", "api_key_name": "claude-desktop",
 "transport": "stdio", "tool": "restore_to_safe_clone", "arguments": {"backup_id": "bkp_shop_20260924_030000_3f9a1c2e", "verify": true},
 "result": "ok", "duration_ms": 18, "count": 1}
```

A REST entry has `transport` `rest`, the route pattern as `tool` (never the raw path, and `(no route)` when none matched), the path parameters as `arguments` (request bodies and queries are not stored) and the response status in `http_status`:

```json
{"id": 43, "time": "2026-09-25T10:16:10Z", "api_key_id": "key_9f8e7d6c5b4a3928", "api_key_name": "ci",
 "transport": "rest", "tool": "DELETE /api/v1/backups/{id}", "arguments": {"id": "bkp_shop_20260901_030000_1a2b3c4d"},
 "result": "denied", "error": "Forbidden", "duration_ms": 0, "http_status": 403, "count": 1}
```

`result` is `ok`, `error` (the call ran and failed; `error` holds the message the assistant saw, or the status text for REST), `denied` (scope too small; `403` for REST) or `rate_limited` (`429`). Argument values of secret-looking keys and credentials inside strings are masked before they are stored. The newest 10,000 entries are kept. Refused and polling clients cannot flush the log: repeated `denied` calls of one key and tool, repeated `rate_limited` calls of one key, and repeated REST reads or failed REST requests of one key, route and status are merged into one entry per 10 seconds whose `count` is the number of calls it stands for (`1` for every other entry). A merged entry keeps the distinct values of each argument, up to 20 (for example `"arguments": {"id": ["bkp_a", "bkp_b"]}`), and sets `"_more_values": true` when it dropped some.
