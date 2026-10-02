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
| `operator` | `read` plus `POST /api/v1/backups`, `POST /api/v1/backups/{id}/retry`, `POST /api/v1/jobs/{id}/run`, `POST /api/v1/jobs/{id}/cancel`, `POST /api/v1/restore` into a safe clone on the backup's own connection, and cancelling backups and restores that are not in place (`POST /api/v1/backups/{id}/cancel`, `POST /api/v1/restores/{id}/cancel`), `POST /api/v1/backups/{id}/verify`, `.../pin` and `POST /api/v1/jobs/{id}/restore-test`, plus the MCP action tools |
| `admin` | Everything: deletions, in-place and cross-connection restores, jobs, connections, storage targets, notifications, settings, users (including the user list), API keys and the audit log |

The recovery kit (`POST /api/v1/recovery-kit`) is refused to every API key, `admin` included: it needs a signed-in user who confirms their password. An in-place restore (`"safe_clone": false` or a `target_database`) and a restore into another connection than the backup's (`target_connection_id`) need `admin` even though the route itself needs `operator`, and so does cancelling a running in-place restore (it may leave the target partially restored). Keys created before scopes existed (and a key imported from `MONGORESCUE_API_KEY`) are `admin` keys.

Failed logins return a generic `401`. After 5 failures for the same (client IP, username), further attempts for that pair get `429 Too Many Requests` with `Retry-After`; the lockout starts at 30 seconds and doubles up to 15 minutes. Once an IP has 20 recent failures, each further username from it is locked after a single failure, but the correct password of a user that is not locked always works, so clients sharing one address (NAT, a proxy) cannot lock each other out. Only one attempt per (IP, username) is checked at a time, and at most four per IP for usernames that already failed; concurrent extras get `429` with `Retry-After: 2`. Password comparisons share a global pool (twice the number of CPUs): a login waits up to 5 seconds for a free slot rather than being refused. Wrong setup codes are throttled only after 100 per IP within 15 minutes, and the correct code is always accepted. Passwords longer than 72 bytes are rejected without a password check.

`POST /api/v1/setup` and `POST /api/v1/auth/login` require `Content-Type: application/json` (`415` otherwise) and, when the request has an `Origin` header, it must be this server's host or a configured CORS origin (`403` otherwise). This blocks cross-site form posts (login CSRF).

Sessions end after the `security.session_idle_timeout` without requests (default 12 hours) or the `security.session_absolute_timeout` after login (default 7 days), on logout, when they are revoked from the session list (`DELETE /api/v1/auth/sessions/{id}`, the dashboard's user menu → *Sessions*), and when the user's password changes (other sessions) or the user is deleted. Deleting a user also revokes the API keys they created.

## Endpoints

| Method | Endpoint | Description | Success | Errors |
| :--- | :--- | :--- | :--- | :--- |
| `GET` | `/api/v1/health` | Health check (`status`, `version`, `time`) | 200 | |
| `GET` | `/api/v1/setup/status` | `{"setup_required": bool}` | 200 | |
| `POST` | `/api/v1/setup` | `{setup_code, username, password}` → first user and session: `{user, csrf_token}` | 201 | 400, 403 wrong code, 409 already set up, 429 |
| `POST` | `/api/v1/auth/login` | `{username, password}` → `{user, csrf_token}` | 200 | 401, 403 foreign origin, 415, 429 |
| `POST` | `/api/v1/auth/logout` | Revoke the current session | 200 | |
| `GET` | `/api/v1/auth/me` | `{user, csrf_token, auth: "session"\|"api_key"}`; signed out: `{user: null, csrf_token: "", auth: ""}` | 200 | |
| `GET` | `/api/v1/auth/sessions` | Your own live sessions, most recently active first: `[{id, user_id, username, created_at, last_seen_at, expires_at, current}]`; `?all=true` lists every user's (admin only). Never carries tokens or their hashes; an API key sees the sessions of the user who created it | 200 | 400, 403 |
| `DELETE` | `/api/v1/auth/sessions/{id}` | Revoke any user's session → `{revoked_id, current}`; admin only (every signed-in user is an admin; API keys below admin may list their creator's sessions but not revoke them). Revoking your own current session also clears the cookie | 200 | 403, 404 |
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
| `GET` | `/api/v1/stats/history` | Outcomes and stored size per day, each job's recent runs, the next 24 hours' scheduled runs and failed verifications (`?days=` 1-366, default 30; `?tz_offset=` minutes east of UTC) ([details](#overview-history-and-schedule-preview)) | 200 | 400 |
| `GET` | `/api/v1/schedule/preview` | Whether `?cron=` is a valid schedule and its next `?n=` (1-10, default 3) activations, as the scheduler computes them ([details](#overview-history-and-schedule-preview)) | 200 | 400 |
| `GET` | `/api/v1/settings` | All settings, secrets masked: `{general, security, encryption, integrity, metadata_backup, restart_required, warnings}` | 200 | |
| `PUT` | `/api/v1/settings` | Partial update, e.g. `{"general": {...}}`; returns the full settings | 200 | 400 |
| `POST` | `/api/v1/settings/encryption/generate-key` | New X25519 key pair `{identity, recipient}` (not stored) | 200 | |
| `POST` | `/api/v1/settings/warnings/{id}/dismiss` | Dismiss a persistent warning for good; returns the remaining `{warnings}` | 200, 404 | |
| `GET` | `/api/v1/metadata-backup` | Status of the [metadata backups](#metadata-backups-and-the-recovery-kit): last snapshot `{target_id, target_name, key, created_at, size_bytes, encrypted}`, last error, next run | 200 | 503 |
| `POST` | `/api/v1/metadata-backup/run` | Take a metadata snapshot now, in the background (admin) | 202 | 409 already running, 503 |
| `GET` | `/api/v1/recovery-kit` | When the last recovery kit was downloaded and whether it is still current | 200 | 503 |
| `POST` | `/api/v1/recovery-kit` | Download the recovery kit `{passphrase, current_password}` as an age-encrypted tar (admin, signed-in users only) ([details](#metadata-backups-and-the-recovery-kit)) | 200 | 400, 403, 429 |
| `GET` / `POST` | `/api/v1/storage-targets` | List / create `{name, type, local \| s3}` (the first target becomes the default) | 200 / 201 | 400 |
| `GET` / `PUT` | `/api/v1/storage-targets/{id}` | Get / update a target | 200 | 400, 404, 409 changed meanwhile or location locked |
| `DELETE` | `/api/v1/storage-targets/{id}` | Delete a target | 200 | 404, 409 default or in use |
| `POST` | `/api/v1/storage-targets/{id}/test` | Test a saved target → `{ok, latency_ms, error}` | 200 | 404 |
| `POST` | `/api/v1/storage-targets/test` | Test an unsaved target (plus `id` when editing, for its masked secret) | 200 | 400 |
| `POST` | `/api/v1/storage-targets/{id}/default` | Make the target the default | 200 | 404 |
| `GET` | `/api/v1/jobs` | List scheduled jobs, by name; optional filters `q`, `enabled`, `connection_id`, `database`, `schedule`, `last_status` ([details](#listing-jobs)) | 200 | 400 |
| `POST` | `/api/v1/jobs` | Create or update a job (`connection_id` and `database` or `database_selection` required, [several databases](#jobs-with-several-databases); `storage_target_id` and `parallelism` optional; the cron expression is validated) | 201 | 400 |
| `GET` | `/api/v1/jobs/{id}` | Get a job with its next three activations (`next_runs`, UTC) | 200 | 404 |
| `PUT` | `/api/v1/jobs/{id}` | Update a job and reschedule it at once ([details](#updating-a-job)) | 200 | 400, 404, 409 changed meanwhile |
| `DELETE` | `/api/v1/jobs/{id}` | Delete a job | 200 | 404 |
| `POST` | `/api/v1/jobs/{id}/run` | Run a job now: the body is the backup record, or for a job with several databases the run (`status: "running"`), whose databases are resolved and backed up in the background | 202 | 400, 404, 409 |
| `POST` | `/api/v1/jobs/{id}/cancel` | Stop the job's current run: the running database and those still waiting ([details](#jobs-with-several-databases)) | 200, 202 still stopping | 404, 409 not running |
| `GET` | `/api/v1/jobs/{id}/runs` | The job's runs, newest first, each with the outcome of every database (`?limit=` ≤ 200) | 200 | 404 |
| `GET` | `/api/v1/jobs/{id}/databases/preview` | The databases the job's selection backs up now, the excluded ones with reasons and the new ones (`{included, excluded, missing, new_since_last_run, warnings}`; query parameters preview another selection) ([details](#previewing-a-selection)) | 200 | 400, 404, 502 |
| `GET` | `/api/v1/jobs/databases/preview` | The same for an unsaved job (`connection_id`, `mode`, `databases`, `include`, `exclude`, `auto_include_new`) | 200 | 400, 404, 502 |
| `GET` | `/api/v1/backups` | List backups, newest first; filters, sorting and pagination ([details](#listing-backups-and-restores)) | 200 | 400 |
| `GET` | `/api/v1/backups/databases` | Distinct database names of all backups, sorted (for filters) | 200 | |
| `POST` | `/api/v1/backups` | Start a backup `{connection_id, database, collections \| exclude_collections, storage_target_id, gzip, include_users_and_roles}` ([users and roles](#users-and-roles)) | 202 | 400, 409 |
| `DELETE` | `/api/v1/backups/{id}` | Delete a backup and its artifact on the backup's storage target → `{deleted_id, archive_deleted, archive_kept}`. The archive is kept when another record (in any status) names it; `archive_kept` says which, a pinned one included | 200 | 404, 409 pinned |
| `POST` | `/api/v1/backups/bulk` | Run one action on many backups (`delete`, `verify`, `pin`, `unpin`, `cancel`), with a dry run ([details](#bulk-actions)) | 200 | 400, 403, 409 confirm_count, 422 too many |
| `POST` | `/api/v1/restores/bulk` | Delete restore history records or cancel restores in bulk ([details](#bulk-actions)) | 200 | 400, 403, 409, 422 |
| `POST` | `/api/v1/jobs/bulk` | Enable, disable, run or delete jobs in bulk ([details](#bulk-actions)) | 200 | 400, 403, 409, 422 |
| `GET` | `/api/v1/bulk/actions` | The available bulk actions and whether the caller may run each | 200 | |
| `POST` | `/api/v1/backups/{id}/verify` | Re-read the archive and compare it with its checksum, in the background; poll the backup for `verified_at` ([verification](verification.md)) | 202 | 404, 409 not completed or already running |
| `POST` | `/api/v1/backups/{id}/pin` | Pin (legal hold) with an optional `{note}`: retention and deletion skip it | 200 | 400, 404 |
| `POST` | `/api/v1/backups/{id}/unpin` | Lift the pin (admin: it makes the backup deletable again) | 200 | 403, 404 |
| `GET` | `/api/v1/jobs/{id}/retention/preview` | Backups the retention policy would delete now and why (`?retention_days=`, `?retention_count=` preview other values) | 200 | 400, 404 |
| `GET` | `/api/v1/jobs/{id}/retention/log` | Backups the job's retention deleted, newest first (`?limit=` ≤ 200) | 200 | 404 |
| `POST` | `/api/v1/jobs/{id}/restore-test` | Run a restore test of the job's latest backup now, in the background → `{job_id, backup_id}` | 202 | 404, 409 no backup or already running, 503 |
| `GET` | `/api/v1/jobs/{id}/restore-tests` | Restore test results, newest first (`?limit=` ≤ 100) | 200 | 404 |
| `GET` | `/api/v1/integrity` | Integrity sweep status, the latest scan of every storage target, the next scheduled scan and the integrity work running | 200 | |
| `POST` | `/api/v1/integrity/sweep` | Start an integrity sweep now (admin) | 202 | 409 already running |
| `GET` / `POST` | `/api/v1/storage-targets/{id}/scan` | Latest / a new storage scan: orphan and missing archives (`POST` admin) | 200 | 404 |
| `POST` | `/api/v1/storage-targets/{id}/import` | Create (or revive the failed or pruned record of) the orphan archive `{key}`, hashed in the background (admin) | 202 | 400 no valid database in the key, 404, 409 not an orphan or already importing |
| `POST` | `/api/v1/backups/{id}/retry` | Retry a failed backup with its parameters; the new record's `retry_of` is `{id}` ([details](#retrying-a-failed-backup)) | 202 | 404, 409 not failed or already running, 422 connection or target gone |
| `GET` | `/api/v1/backups/{id}/collections` | Collections stored in the backup, read from its archive header ([details](#selective-restores)) | 200 | 404, 422 key missing |
| `POST` | `/api/v1/backups/{id}/cancel` | Cancel a running backup ([details](#cancelling-a-run)) | 200, 202 still stopping | 404, 409 not running or finishing |
| `GET` | `/api/v1/backups/{id}/log` | The backup's run log as `text/plain`: the last N lines with `?tail=N` (1-10000), otherwise the whole file as a download ([details](#run-logs)) | 200 | 400, 404 |
| `POST` | `/api/v1/restore` | Restore (safe clone by default; optional `selected_collections`, `target_connection_id` (admin), `verify`, `restore_users_and_roles` (in place only, [users and roles](#users-and-roles))) | 202 | 400, 403, 404, 409, 422 |
| `GET` | `/api/v1/restores` | Restore audit history, newest first; filters, sorting and pagination ([details](#listing-backups-and-restores)) | 200 | 400 |
| `GET` | `/api/v1/restores/databases` | Distinct target databases of all restores, sorted | 200 | |
| `POST` | `/api/v1/restores/{id}/cancel` | Cancel a running restore; an in-place one needs admin ([details](#cancelling-a-run)) | 200, 202 still stopping | 403, 404, 409 not running or finishing |
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

`GET /api/v1/settings` returns every setting grouped as `general`, `security` and `encryption` ([descriptions](configuration.md#settings)), plus `restart_required` (always empty: every change applies to the next operation or request) and `warnings`, the persistent notices the dashboard shows as a banner: `[{id, message, setting}]`. The warnings are `encryption_off_after_upgrade` (see [encryption.md](encryption.md#encryption-turned-off-by-an-upgrade)), `metadata_backup_unencrypted` (metadata backups are on but encryption is off; it cannot be dismissed) and `recovery_kit_missing` (no recovery kit was downloaded since `secret.key`, the encryption keys or the storage targets last changed; dismissing it hides it until they change again). Durations are Go duration strings (`"6h0m0s"`), secrets (`encryption.identity`, `encryption.passphrase`) are `"******"` when set and `""` when not, and `encryption.retired_keys` lists `{kind, recipient, retired_at}` without key material.

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

## Metadata backups and the recovery kit

The `metadata_backup` settings group (`enabled`, `interval`, `target_id`, `retention_count`; see [configuration.md](configuration.md#metadata-backups)) schedules snapshots of `mongorescue.db`. `POST /api/v1/metadata-backup/run` takes one now (`202`, `409` while one runs); `GET /api/v1/metadata-backup` reports `{enabled, prefix, running, last_run_at, last_trigger, last_error, retention_error, last, next_run_at}`; `prefix` is `_mongorescue/metadata/<install_id>/`, this installation's own prefix (derived from `secret.key`), and `last` carries `install_id`. A failed snapshot publishes `metadata_backup.failed`. The runbook is in [production.md](production.md#restore-mongorescue-from-a-snapshot).

`POST /api/v1/recovery-kit` returns the recovery kit as `application/octet-stream` (`Content-Disposition: attachment; filename=mongorescue-recovery-kit-<date>.tar.age`, `Cache-Control: no-store`). The body is `{"passphrase": "...", "current_password": "..."}`:

- only a signed-in user may call it, with the session cookie and `X-CSRF-Token`; API keys get `403` whatever their scope;
- `current_password` must be the user's password (`403` otherwise; wrong passwords count against the same throttle as password changes, then `429` with `Retry-After`);
- `passphrase` seals the kit with age scrypt and needs at least 12 characters (`400`). It is not stored.

The kit is a tar archive with `README.txt` (the recovery steps), `secret.key`, `recovery.json` (encryption settings, every storage target with its credentials, the snapshot prefix `metadata_prefix` and the latest metadata snapshot) and `identities.txt` (the age private keys, only when the server holds one; in recipient-only mode the README points to your own key file instead). Passphrases are never included. Open it with `age -d -o kit.tar mongorescue-recovery-kit-<date>.tar.age && tar -xf kit.tar`. Every download and refused attempt is written to the audit log (`POST /api/v1/recovery-kit`, the user, the result), never the passphrase, the password or the content. `GET /api/v1/recovery-kit` returns `{downloaded_at, up_to_date, min_passphrase_length}`.

## Restores

Restore into a safe clone (the default, so `safe_clone` may be omitted):

```bash
curl -s -X POST http://localhost:8080/api/v1/restore \
  -H "Authorization: Bearer $MONGORESCUE_KEY" -H 'Content-Type: application/json' \
  -d '{"backup_id": "bkp_shop_20260924_030000_3f9a1c2e"}'
```

Restoring in place (into the source database, or into `target_database`) must be confirmed explicitly with `{"safe_clone": false, "confirm_in_place": true}`; any other in-place request is rejected with `400 Bad Request` before `mongorestore` starts. In-place restores are always verified first (`verify` and the policy apply to safe clones only). A missing decryption key is rejected up front with `422 Unprocessable Entity`; a checksum mismatch or failed decryption found during verification marks the restore record as failed, and `mongorestore` is never started.

### Restore preflight

`POST /api/v1/restores/preflight` (operator scope) takes the body of a restore request and answers the go/no-go summary without starting anything:

```json
{
  "ok": false,
  "checks": [
    {"id": "connection", "status": "pass", "message": "connected to prod (MongoDB 7.0.14)"},
    {"id": "encryption", "status": "pass", "message": "the backup is not encrypted"},
    {"id": "server_version", "status": "warn", "message": "the backup's server version is unknown (backups of earlier releases do not record it); the target runs MongoDB 7.0.14"},
    {"id": "target_database", "status": "pass", "message": "restores into the new database shop_rescue_20261002_120000; existing data is untouched"},
    {"id": "privileges", "status": "pass", "message": "the user of connection prod may restore into shop_rescue_20261002_120000"},
    {"id": "disk_space", "status": "fail", "message": "the target has 120.0 MiB free, less than the 2.1 GiB archive"},
    {"id": "collections", "status": "pass", "message": "a safe clone restores into a new database; no existing collection is replaced"},
    {"id": "users_and_roles", "status": "pass", "message": "users and roles are not restored"}
  ]
}
```

`status` is `pass`, `warn` or `fail`; `ok` is `false` when a check failed. The checks, in this order:

| ID | Checks |
| :--- | :--- |
| `connection` | The target server answers (`fail` otherwise; the server checks below are then `warn` "not checked"). |
| `encryption` | An encrypted backup has a decryption key configured. |
| `server_version` | The target's MongoDB version against the version recorded on the backup (`server_version`, read with `buildInfo` while the manifest is captured; backups of earlier releases have none and warn "unknown"). An older major version fails, a newer major version or an older release of the same major version warns. |
| `target_database` | Whether the target database exists; for a safe clone the clone name must be free (two restores of a database within the same second share one). |
| `privileges` | The connection's user holds `createCollection`, `createIndex` and `insert` on the target database (`connectionStatus`), plus `dropCollection` for an in-place restore with `drop_target`; missing user administration actions for `restore_users_and_roles` only warn. |
| `disk_space` | The archive size against the free space of the target server's data filesystem, from `dbStats` (`fsTotalSize - fsUsedSize`) or, for a server on the same host, from the file system of its `dbPath`. Less free space than the archive fails; less than twice (uncompressed) or four times (compressed or unknown) the archive warns; unknown free space warns. |
| `collections` | For an in-place restore, the existing collections the restore writes into (from the selection, the backup's manifest or its collection filter, against `listCollections`): dropped and replaced with `drop_target`, otherwise documents are added. Listed as a warning. |
| `users_and_roles` | `restore_users_and_roles` against the backup (fails where `POST /api/v1/restore` answers `400`); a valid request warns that the database's users and roles are replaced. |

A preflight applies the scope rules of the restore it checks: an in-place or cross-connection preflight needs admin. `confirm_in_place` is not needed to ask (it is still needed to start the restore). Dry runs need neither privileges nor space.

`POST /api/v1/restore` runs the same checks before it starts and stores them on the restore record (`preflight`). A failed check refuses the restore with `409 Conflict`, the checks in `data` and the failed ones in `error`; send `"force": true` to restore anyway (the record then has `"forced": true`). Warnings never block, and dry runs are never refused. The dashboard runs the preflight while the restore dialog's options change and shows *Restore anyway* when a check failed.

### Restore verification

`"verify_restore": true` compares the restored database with the manifest the backup captured (per collection: the document count range and the indexes) once the restore completed, like an [automated restore test](verification.md#automated-restore-tests) does with a temporary database. It is off when omitted, so API clients of earlier releases are unchanged; the dashboard turns it on by default. Only the restored collections are compared: the selection of a selective restore, and for an in-place restore only the collections in the backup (others in the target are ignored). An in-place restore without `drop_target` adds documents to the ones already there, so a larger count is a note, not a mismatch. The result is stored on the record:

```json
"verification": {
  "status": "failed",
  "mismatches": ["collection orders: 9 documents restored, 10 expected"],
  "notes": [],
  "collections": 2,
  "checked_at": "2026-10-02T12:00:41Z"
}
```

`status` is `passed`, `failed` or `skipped` (a dry run, a backup without a manifest, a target that could not be inspected; `notes` says why). A failed verification keeps the restore `completed`, adds a warning to it and publishes `restore.verification_failed` (selectable in notification rules): the data is applied, but it does not match what the backup recorded. The dashboard shows the result in the restore details.

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

### Users and roles

Backups do not include the users and roles defined on a database unless asked to. A job (`POST /api/v1/jobs`, `PUT /api/v1/jobs/{id}`) or an on-demand backup (`POST /api/v1/backups`) with `"include_users_and_roles": true` (default `false`, returned by `GET /api/v1/jobs/{id}`) runs `mongodump --db=<db> --dumpDbUsersAndRoles`, once per database for a job with several databases. The backup record then says `"users_and_roles": true`; a retry of a failed backup keeps the option. `mongodump` refuses `--collection` with this flag, so a collection filter of such a backup is passed as exclusions of the other collections. Without a database (`database` empty, which mongodump would dump whole), the option is ignored, since mongodump only dumps users and roles together with `--db`. The `admin` database is the exception: it stores every user and role of the server as its own collections (`admin.system.users`, `admin.system.roles`), so its backups contain them anyway, and a single-database job or backup of `admin` with `include_users_and_roles` is refused with `400`. List, all and pattern selections never include `admin`.

A restore with `"restore_users_and_roles": true` runs `mongorestore --db=<db> --restoreDbUsersAndRoles` and records `"users_and_roles": true` on the restore. `mongorestore` restores the users and roles under the name of the database they were dumped from, so the option is allowed only for an in-place restore into the backup's own database (`"safe_clone": false, "confirm_in_place": true`, no `target_database` or the source database's name). Combined with a safe clone or a `target_database` that renames the restore, it is rejected with `400`; it is rejected with `400` as well when the backup was taken without users and roles, or is a backup of `admin`. The users and roles of the target database are replaced by the ones in the backup: users and roles created since the backup are removed, in full even when `selected_collections` restores only some collections. With `target_connection_id`, an in-place restore on another server restores them there, into the database of the same name. Restoring them needs a connection whose user may manage users and roles (for example `userAdminAnyDatabase` or `root`).

```bash
curl -s -X POST http://localhost:8080/api/v1/restore \
  -H "Authorization: Bearer $MONGORESCUE_KEY" -H 'Content-Type: application/json' \
  -d '{"backup_id": "bkp_shop_20260924_030000_3f9a1c2e", "safe_clone": false, "confirm_in_place": true, "restore_users_and_roles": true}'
```

The MCP tools do not take these options: `restore_to_safe_clone` only restores into a safe clone, and jobs are created through the REST API or the dashboard (`get_job` and the job resources show `include_users_and_roles`).

## Updating a job

`PUT /api/v1/jobs/{id}` (admin scope, like creating a job) replaces a job's `name`, `cron_expression`, `database`, `collections`, `exclude_collections`, `connection_id` and `storage_target_id`. `retention_days`, `retention_count`, `gzip`, `include_users_and_roles` ([users and roles](#users-and-roles)), `enabled`, `verify_after_backup` (`""` follows the `integrity.verify_after_backup` setting, `"on"`, `"off"`) and `restore_test` (`{enabled, frequency, every_n, connection_id}`, see [verification.md](verification.md#automated-restore-tests)) are optional: an omitted field keeps the job's current value, so `{"enabled": false, ...}` pauses a job without touching its retention, and `last_restore_test` is managed by the server and never taken from requests. Pausing with `"paused_until": "<RFC 3339 time>"` resumes the job on its own at that time (the scheduler checks every minute; the time must be in the future, else `400`); pausing without it pauses until the job is resumed, an edit that keeps the job paused keeps its `paused_until`, and resuming (`"enabled": true`) clears it. A paused job's run in progress continues; stop it with [cancel](#cancelling-a-run). The job is validated exactly like a new one: the cron expression must parse (five fields or a descriptor such as `@daily` or `@every 6h`; an empty one means `@daily`), `database` and a known `connection_id` are required, retention must not be negative, and `storage_target_id` must name a target (empty means the default target).

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

In the dashboard, clicking a job row (or **Details** in its **⋯** menu) opens the job's details: its schedule in words with the next three runs, retention, compression, encryption, state, and the last 20 runs with their success rate. **Edit** opens the job form prefilled, and **Pause** / **Resume** (also in the row's **⋯** menu) pauses the schedule, until resumed or until a date and time, or resumes it; a paused job shows a *Paused* badge. **Stop current run** stops the job's current run: its running backup and, for a job with several databases, every database still waiting. Edits and pauses send `updated_at`, so a job changed elsewhere in the meantime is reported instead of overwritten.

`database_selection` and `parallelism` are optional in an update: when `database_selection` is omitted, a `database` makes the job a single-database job of it, and an update with neither keeps the job's selection (see [Jobs with several databases](#jobs-with-several-databases)). `restore_test.databases` (`rotate` or `all`) chooses which databases of a multi-database run the restore test covers.

## Jobs with several databases

A job backs up one database or several. Its `database_selection` says which:

```json
{
  "mode": "pattern",
  "databases": ["billing"],
  "include": ["prod_*"],
  "exclude": ["prod_tmp?"],
  "auto_include_new": false
}
```

| `mode` | Backs up | Fields |
| :--- | :--- | :--- |
| `single` | One database, as jobs always did (`database` holds it too) | `databases`: exactly one name |
| `list` | The named databases | `databases`: at least one name |
| `all` | Every database of the connection except those `exclude` matches | `exclude`, `databases` (always added), `auto_include_new` |
| `pattern` | The databases an `include` pattern matches and no `exclude` pattern matches | `include` (at least one), `exclude`, `databases` (always added), `auto_include_new` |

- `admin`, `config` and `local` are never backed up by `list`, `all` or `pattern` jobs, and naming one in a `list` is refused with `400`.
- Patterns use `*` (any run of characters, also none) and `?` (exactly one character); every other character matches itself and matching is case-sensitive, like MongoDB database names. They follow the rules for database names (1 to 63 bytes, none of `/ \ . " $`, a space or a control character, not starting with `-`), with `*` and `?` as the only wildcards. A pattern that matches no database is allowed; the preview and the run report it as a warning.
- Collection filters (`collections`, `exclude_collections`) only apply to `single` jobs; a multi-database job that sends them is refused with `400`.
- `parallelism` (1 to 4, default 1) is how many databases a run backs up at the same time. Each database keeps its own run lock: a database another backup is running for (a manual backup, another job) waits until it finished, up to 30 minutes, and is recorded as failed in this run after that (the others still run).
- Clients that predate selections keep working: a job created or updated with `database` and no `database_selection` is a `single` job of that database, an update that sends neither keeps the job's selection, and responses still carry `database` for single-database jobs (`""` for the others). Every job stored by an earlier release was migrated to a `single` selection (migration 0013).

**New databases.** For `all` and `pattern` jobs the server records the databases the selection matched when the job was saved (and when its selection or connection changes) as `known_databases` (managed by the server, never taken from requests). With `auto_include_new` off (the default), a run backs up only the known databases plus the named ones; databases created later that match are not backed up but listed as `new_databases` in the run, in the run's notification ("3 new databases not included") and in the dashboard, which offers *Add to job* (it adds the name to `databases`). With `auto_include_new` on, a run backs them up too, adds them to `known_databases` and publishes a `job.databases_added` event the first time each one appears.

**Runs.** Every database of a run gets its own backup record and archive, exactly as a single-database backup: the same storage key layout (`<db>/<yyyy>/<mm>/<id>.archive.gz`), verification, manifest, log and cancellation, so restores, retention, pins and verification work unchanged. The records of one run share a `run_id` (`GET /api/v1/backups?run_id=…`), and `GET /api/v1/jobs/{id}/runs` lists the runs: `{id, job_id, trigger, status, started_at, completed_at, duration_seconds, databases: [{database, backup_id, status, error}], new_databases, added_databases, warnings, error}`. A run is `ok` when every database succeeded, `partial` when some failed, `failed` when none succeeded and `cancelled` when it was stopped without failures. A database named in a `list` (or in `databases`) that no longer exists is recorded as failed with `"error": "database not found"`; the others still run. A run whose connection cannot list its databases fails as a whole (`error`), and so does a run that matches no existing database. `POST /api/v1/jobs/{id}/run` of a multi-database job answers `202` at once with the run (`{id, job_id, trigger, status: "running", started_at, databases: []}`); its databases are resolved in the background and planning failures are recorded in the run (poll `GET /api/v1/jobs/{id}/runs`), while a run of the job still going answers `409`. The run is stored after every database, so a crash keeps the outcome of the databases that finished: the next start rebuilds each database's status from its backup record (completed ones stay completed, the others fail as interrupted). Single-database jobs record a run too.

Notifications send one message per run, not one per database: the run's `backup.succeeded`, `backup.failed` (also for a `partial` run, whose message names the failed databases) or `backup.cancelled` event carries `run_id` and a `run` summary (`status`, `databases`, `succeeded`, `failed`, `cancelled`, `failed_databases`, `new_databases`); webhooks receive both fields. The per-database events still feed the metrics (per job and per database), and every run is counted in `mongorescue_job_runs_total` ([metrics](metrics.md)).

**Stopping a run.** `POST /api/v1/jobs/{id}/cancel` (operator) stops the job's current run: the running database and every database still waiting are cancelled (`{job_id, run_ids, cancelled, backups}`; `200` once they stopped, `202` while some still stop, `404` unknown job, `409` no run of it is active). Cancelling any one backup of a run (`POST /api/v1/backups/{id}/cancel`, MCP `cancel_run`, the desktop app's quit) stops the whole run as well. Databases already backed up keep their backups. A scheduled run that is still going when the next one is due is skipped; starting one on demand meanwhile answers `409`. Retention and the restore tests of a scheduled run happen after its run slot is released, so they never delay the next run. The known databases a run records are merged into the job as it is stored when the run ends: when the job's selection or connection was changed meanwhile, only the databases it backed up that the new selection still matches are added, and a newer value is never overwritten.

**Retention** stays per job and database: the job's `retention_days` and `retention_count` apply to each of its databases separately, and so do the protections. The newest completed backup of every database is kept (so a database whose backup failed today keeps its last good one), and so is the newest verified one of every database. A scheduled run prunes only the databases it backed up successfully. `GET /api/v1/jobs/{id}/retention/preview` adds `databases`, the per-database breakdown (`database`, `considered`, `delete`, `protected`, `kept`, `last_good`).

**Restore tests** of a multi-database job test one database per run, taking turns in name order (`restore_test.databases: "rotate"`, the default), or every database of the run (`"all"`).

### Previewing a selection

`GET /api/v1/jobs/{id}/databases/preview` (read scope) resolves the job's selection against the live server exactly as its next run would, without changing anything:

```json
{
  "job_id": "job_pattern_1727146800_3f9a1c2e",
  "selection": {"mode": "pattern", "include": ["prod_*"], "auto_include_new": false},
  "included": ["prod_eu", "prod_us"],
  "excluded": [{"name": "admin", "reason": "system"}, {"name": "prod_new", "reason": "new"}, {"name": "staging", "reason": "not_matched"}],
  "missing": [],
  "new_since_last_run": ["prod_new"],
  "warnings": []
}
```

Reasons are `system`, `excluded` (with the matching `pattern`), `not_matched`, `not_selected`, `new` and `not_found`. Query parameters preview another selection while the job is edited: `mode`, repeated `databases`, `include` and `exclude`, `auto_include_new` and `connection_id`; a changed selection is previewed as saving it would record it (everything it matches counts as known). `GET /api/v1/jobs/databases/preview` previews a selection for a job that does not exist yet (`connection_id` and `mode` required). Answers: `400` invalid selection, `404` unknown job or connection, `502` the databases cannot be listed (a `list` selection is then previewed as named, with a warning).

```bash
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "X-API-Key: $MONGORESCUE_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name":"all prod","cron_expression":"0 2 * * *","connection_id":"conn_prod",
       "database_selection":{"mode":"pattern","include":["prod_*"],"exclude":["prod_tmp*"]},"parallelism":2}'
```

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
| `run_id` | yes | | Backups of this job run (one per database) |
| `backup_id` | | yes | Restores of this backup |
| `from`, `to` | yes | yes | `started_at` range in RFC 3339 (`2026-10-01T00:00:00Z`); `from` is inclusive, `to` exclusive |
| `q` | ID or database | ID, source or target database | Substring, ASCII case-insensitive; `%` and `_` are literal (at most 256 characters) |
| `sort` | `started_at`, `duration`, `size`, `database`, `status` | `started_at`, `duration`, `database` (target), `status` | A column for ascending order, `-` and a column for descending (`sort=-size`); ties keep the newest first. `desc` (newest first, the default) and `asc` still sort by start time |
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

`total` counts every match, so a client can show "1–25 of 312"; an `offset` past the end returns an empty `data` with the same `total`. A malformed or out-of-range value (`limit=0`, `limit=500`, `sort=random`, `sort=id`, `sort=size` on restores, `from=yesterday`, `to` before `from`, an unknown `status` or `trigger`) answers `400` with a message naming the parameter. Backup items carry `retried_by`, the newest backup whose `retry_of` is this one, when there is one; it is computed for each listed row, so retry links work on any page.

```bash
curl "http://localhost:8080/api/v1/backups?status=failed&database=shop&from=2026-09-01T00:00:00Z&limit=25&offset=25" \
  -H "X-API-Key: $MONGORESCUE_READ_KEY"
```

The sort columns are a fixed list mapped to constant SQL; anything else, a different case or a column the resource has not answers `400` listing the accepted columns.

```bash
curl "http://localhost:8080/api/v1/backups?status=completed&sort=-size&limit=25" -H "X-API-Key: $MONGORESCUE_READ_KEY"
```

`GET /api/v1/stats` covers every record regardless of any list filter and is computed with SQL aggregates (it does not read every record): `total_backups`, `completed_backups`, `failed_backups`, `failed_backups_24h`, `total_bytes`, `active_backups` and `active_restores` (pending or in progress), `total_restores`, `active_jobs`, `last_backup` (`{id, database, job_id, status, started_at, error_message}` of the newest backup) and `job_last_backups` (each job ID mapped to its newest backup), plus the default storage target. When some figures cannot be read (they then count as zero), the response adds `degraded: true` and a `degraded_reason`. For administrators (dashboard sessions and `admin` keys) it also lists `corrupt_records`: stored rows that every list skips because they cannot be read, as `{table, id, error}` (the error never quotes stored data). See [troubleshooting.md](troubleshooting.md#unreadable-records).

## Listing jobs

`GET /api/v1/jobs` returns every job sorted by name. Optional filters keep only the matching jobs, with the same matcher as the jobs [bulk filter](#bulk-actions), so selecting "all matching" there selects exactly the listed jobs. All given filters must match; without any the response is unchanged.

| Parameter | Meaning |
| --- | --- |
| `q` | ID, name, database or a multi-database job's selection contains this text (case-insensitive, at most 256 characters) |
| `enabled` | `true` scheduled jobs, `false` paused ones (anything else: `400`) |
| `connection_id` | Jobs of this connection |
| `database` | Jobs covering this database: a single-database job's database, or one a multi-database job lists, knows or matches with a pattern |
| `schedule` | Frequency of the cron expression: `hourly` (more than once a day, also `@every` under 24 h), `daily`, `weekly` (some weekdays), `monthly` (some days of the month) or `other` |
| `last_status` | Outcome of the job's last run: `completed`, `failed`, `partial`, `in_progress`, `cancelled` or `never`. A single-database job's is its newest backup's (a pruned or missing archive still counts as `completed`); a multi-database job's is its newest run's (`ok`, `partial`, `failed`, `cancelled`, `running`), never one database's backup |

An unknown `schedule` or `last_status` value, or an over-long text, answers `400`. Sorting is left to the client (the list is not paged).

## Overview history and schedule preview

`GET /api/v1/stats/history` (read scope) feeds the dashboard's *Overview* tab. Like `/api/v1/stats` it is computed with SQL aggregates and never reads every record:

- `daily`: one entry per day of the window, oldest first, empty days included: `{date, completed, failed, cancelled, bytes, stored_bytes}`. `days` (1-366, default 30) counts back from today. `tz` (an IANA zone such as `Europe/Istanbul`) makes the days start at your midnights, also across daylight saving changes. When `tz` is missing or unknown to the server, `tz_offset` (minutes east of UTC, -840 to 840, e.g. `180` for UTC+3) is used as a fixed offset instead. `time_zone` in the answer names the zone used. `bytes` sums the day's completed backups that are still on record, and `stored_bytes` is the running total since `stored_bytes_before` (completed backups on record that started earlier). Both describe the archives kept today, so backups that retention has deleted no longer count.
- `jobs`: each existing job with backups or an enabled schedule mapped to `{runs, last_success_at, interval_seconds}`: its 12 newest backups, oldest first (`{id, status, started_at, duration_seconds}`), when its newest completed backup started, and the gap between its next two scheduled runs.
- For a job with several databases, `runs` lists its 12 newest runs, each once however many databases it covered (`{id, status, run_status, databases, succeeded, started_at, duration_seconds}`; `status` is `completed` for `ok`, `failed` for `partial` and `failed` runs), and `last_success_at` is its stalest database's newest success (empty while one never succeeded), with `stalest_database` and the per-database `databases`. The Overview's attention list then reads "job X: db Y has no successful backup since …" and flags a partial last run.
- `upcoming`: the enabled jobs' scheduled runs in the next 24 hours, in order (`{job_id, at}`), at most 96 per job and 500 in total (`upcoming_truncated` says the caps cut the list).
- `verification_issues`: the 20 newest completed backups of the window whose verification failed (`{id, job_id, database, started_at, verification}`, `mismatch` or `error`), and `verification_issues_total` for the window.

Every query is answered from an index on `backups`, never by scanning the table, and record JSON is read only for the rows the index selected (a job's newest runs, the window's completed backups).
- `server_time_zone`: `{name, offset_minutes}`, the time zone cron expressions are evaluated in.

`GET /api/v1/schedule/preview?cron=0%202%20*%20*%20*&n=3` (read scope) parses a cron expression with the scheduler's own parser and returns `{valid, error, next_runs, server_time_zone}`: the expression's hours are read in the server's time zone, and `next_runs` are given in UTC. An expression the scheduler rejects is answered with `200` and `"valid": false` plus the reason; a missing or longer than 256 characters `cron`, or `n` outside 1-10, is a `400`. The dashboard's cron builder uses it for its *next runs* line.

## Bulk actions

`POST /api/v1/backups/bulk`, `POST /api/v1/restores/bulk` and `POST /api/v1/jobs/bulk` run one action on many records. They share one body:

```json
{"action": "delete", "filter": {"status": "failed", "database": "shop", "from": "2026-09-01T00:00:00Z"}, "dry_run": true}
{"action": "delete", "ids": ["bkp_a", "bkp_b", "…the dry run's actionable_ids"], "confirm_count": 312}
{"action": "pin", "filter": {"job_id": "job_shop"}, "note": "audit 2026", "confirm_count": 40}
```

- Exactly one of `ids` (duplicates count once; each at most 256 bytes) and `filter` selects the items. **Destructive actions (`delete`) take a filter only with `dry_run: true`**: their real run must name the exact IDs, the dry run's `actionable_ids`, and a filter is refused with `400`, because a filter evaluated later could match other records than the ones that were shown. `filter` takes the names and formats of the list endpoints' query parameters (backups: `status`, `database`, `connection_id`, `job_id`, `trigger`, `retry_of`, `from`, `to`, `q`; restores: `status`, `database`, `backup_id`, `from`, `to`, `q`; jobs: `database`, `connection_id`, `q`, `enabled`, `schedule`, `last_status`, as in [listing jobs](#listing-jobs)) and selects every match; paging does not apply. A selection may hold at most **10,000** items (`422` otherwise); the body may be about 5 MiB, enough for 10,000 IDs of the maximum length. Unknown fields, also inside `filter`, and filter fields that do not apply to the resource are refused with `400`, so a misspelt filter can never widen the selection.
- `dry_run: true` changes nothing and answers `{matched, actionable, skipped: [{id, reason, params, detail}], total_size_bytes, actionable_ids}`. `reason` is a stable code (see the table below) that clients translate; `params` fill it in (`job` for `last_good_backup` and `last_verified`, `status` for `in_progress` and `not_running`) and `detail` says the same in English.
- A real run plans the selection again. When more than 10 items are affected it needs `confirm_count` equal to the dry run's `actionable`; a set `confirm_count` must always match. Otherwise it is refused with `409` and nothing changes, so a stale client can never act on more than it showed. Items are then processed one at a time through the same use cases as the single-item routes (logs, events and the shared-archive rule included). Every deletion of a backup (single, bulk, and retention) holds a lock per job (per connection and database for backups without a job), and bulk deletes decide the protections again inside it on the stored record, right before deleting it; an item that became protected meanwhile (pinned, newly the last good or last verified backup) is added to `skipped` instead. The record and, when no other record names it any more, its archive are deleted under a lock per archive, so records sharing an archive never strand it. The answer adds `succeeded`, `failed` and `results: [{id, ok, error, warning, detail}]` in order. Items are not started once the request ends; an item that started is finished.
- One `bulk.completed` event (not selectable in notification rules) summarises every real run, and `mongorescue_bulk_operations_total{resource,action}` and `mongorescue_bulk_items_total{resource,action,outcome}` count them. Every real run also writes an audit entry (`tool` *bulk backups delete* and so on, with the actor, the counts and a SHA-256 of the processed IDs, sorted and one per line), whether it was started from a session or with an API key.

`GET /api/v1/bulk/actions` (read) lists the available actions as `{resource, name, scope, destructive, allowed}`; `allowed` says whether the caller has the scope. The route needs `operator`; each action needs the scope of its single-item route:

| Resource | Action | Scope | Skipped (reason) |
| :--- | :--- | :--- | :--- |
| backups | `delete` | admin | `in_progress`, `pinned`, `last_good_backup` (the newest completed backup of a job), `last_verified` (the job's newest verified scheduled backup, which retention keeps), `not_found` |
| backups | `verify` | operator | `not_verifiable` (not completed or no checksum) |
| backups | `pin` (optional `note`) | operator | `already_pinned` |
| backups | `unpin` | admin | `not_pinned` |
| backups | `cancel` | operator | `not_running` |
| restores | `delete` (history records only; restored databases are not touched) | admin | `in_progress` |
| restores | `cancel` (in-place restores need admin per item) | operator | `not_running` |
| jobs | `enable`, `disable` | admin | `already_enabled`, `already_disabled` |
| jobs | `run_now` | operator | — (a database that is already being backed up fails the item) |
| jobs | `delete` (their backups are kept) | admin | — |

Every action also skips IDs that name no record (`not_found`). A bulk delete never removes a job's last good backup, a pinned backup or the backup retention keeps as the last verified one; delete those one at a time (after unpinning) if you really mean to. There is no undo.

The dashboard selects rows with checkboxes (shift-click for a range, the header box for the page, then *Select all N matching the filters*, which sends the filter), shows the dry run with the skipped items grouped by reason, asks to type the count for destructive actions on more than 10 items, and runs the confirmed IDs in batches of 25 with a progress bar.

## Cancelling a run

`POST /api/v1/backups/{id}/cancel` and `POST /api/v1/restores/{id}/cancel` (operator scope) stop a running backup or restore, whether it was started through the API, the dashboard, MCP or the scheduler. They wait up to 5 seconds for the run to stop and answer `200 OK` with the final record (`"status": "cancelled"`); a run that takes longer to stop is answered with `202 Accepted` and its in-progress record (`progress.cancelling` is `true`): poll it until the status is `cancelled`. A run cancelled while it is still queued never starts its tool. `mongodump` or `mongorestore` is terminated with its process group (SIGTERM, then SIGKILL), so no tool process outlives the run.

- A cancelled **backup** deletes its partial artifact (an S3 multipart upload is aborted, a local temporary file removed) and records neither size nor checksum.
- A cancelled **safe-clone restore** drops the partially restored `<db>_rescue_<timestamp>` database; the message says so (or asks to drop it by hand if that failed).
- A cancelled **in-place restore** cannot be undone: the target may be left **partially restored**. The record's `warning` and `error_message` say so loudly. Cancelling one needs `admin`, like starting it.
- A run cancelled before its tool started leaves the target untouched.

The record gets `"status": "cancelled"`, `cancelled_by` (the username, `API key <name>`, `... via MCP`, or `system` for the desktop app's force quit) and `cancelled_at`. Cancelled runs are not failures: they are not counted in `failed_backups` or the last 24 hours' failures, emit `backup.cancelled` / `restore.cancelled` instead of the failure events (notification rules may subscribe to them) and are counted with `status="cancelled"` in the metrics.

| Status | When |
| --- | --- |
| `200 OK` | The run stopped; the body is its final record |
| `202 Accepted` | The cancellation was requested and the run is still stopping |
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

`POST /api/v1/backups`, `POST /api/v1/backups/{id}/retry`, `POST /api/v1/jobs/{id}/run` and `POST /api/v1/restore` return `202 Accepted` as soon as the operation has started. The response body is the new backup or restore record with `"status": "in_progress"`; poll `GET /api/v1/backups` or `GET /api/v1/restores` until it becomes `completed`, `failed` or `cancelled`. Operations keep running if the client disconnects; [cancel](#cancelling-a-run) them to stop them. Verifications (`POST /api/v1/backups/{id}/verify`), restore tests, sweeps and orphan imports run in the background the same way; their results land on the backup record (`verified_at`, `verification`, `last_restore_test`) or in `GET /api/v1/jobs/{id}/restore-tests` and `GET /api/v1/integrity`.

Backup records carry the trust fields `verified_at`, `verification` (`ok`, `mismatch`, `error`) and `verification_error`; `pinned`, `pin_note`, `pinned_at`, `pinned_by`; `has_manifest`; `imported` and `imported_at`; `missing_since` for `status: "missing"`; and `last_restore_test`. See [verification.md](verification.md).

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
