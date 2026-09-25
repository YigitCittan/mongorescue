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
| `operator` | `read` plus `POST /api/v1/backups`, `POST /api/v1/jobs/{id}/run` and `POST /api/v1/restore` into a safe clone on the backup's own connection (and the MCP action tools) |
| `admin` | Everything: deletions, in-place and cross-connection restores, jobs, connections, storage targets, notifications, settings, users (including the user list), API keys and the audit log |

An in-place restore (`"safe_clone": false` or a `target_database`) and a restore into another connection than the backup's (`target_connection_id`) need `admin` even though the route itself needs `operator`. Keys created before scopes existed (and a key imported from `MONGORESCUE_API_KEY`) are `admin` keys.

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
| `GET` | `/api/v1/stats` | Dashboard KPIs | 200 | |
| `GET` | `/api/v1/settings` | All settings, secrets masked: `{general, security, encryption, restart_required}` | 200 | |
| `PUT` | `/api/v1/settings` | Partial update, e.g. `{"general": {...}}`; returns the full settings | 200 | 400 |
| `POST` | `/api/v1/settings/encryption/generate-key` | New X25519 key pair `{identity, recipient}` (not stored) | 200 | |
| `GET` / `POST` | `/api/v1/storage-targets` | List / create `{name, type, local \| s3}` (the first target becomes the default) | 200 / 201 | 400 |
| `GET` / `PUT` | `/api/v1/storage-targets/{id}` | Get / update a target | 200 | 400, 404, 409 changed meanwhile or location locked |
| `DELETE` | `/api/v1/storage-targets/{id}` | Delete a target | 200 | 404, 409 default or in use |
| `POST` | `/api/v1/storage-targets/{id}/test` | Test a saved target → `{ok, latency_ms, error}` | 200 | 404 |
| `POST` | `/api/v1/storage-targets/test` | Test an unsaved target (plus `id` when editing, for its masked secret) | 200 | 400 |
| `POST` | `/api/v1/storage-targets/{id}/default` | Make the target the default | 200 | 404 |
| `GET` | `/api/v1/jobs` | List scheduled jobs | 200 | |
| `POST` | `/api/v1/jobs` | Create or update a job (`connection_id` required; `storage_target_id` optional) | 201 | 400 |
| `DELETE` | `/api/v1/jobs/{id}` | Delete a job | 200 | 404 |
| `POST` | `/api/v1/jobs/{id}/run` | Run a job now | 202 | 400, 404, 409 |
| `GET` | `/api/v1/backups` | List backups (`?database=` filter) | 200 | |
| `POST` | `/api/v1/backups` | Start a backup `{connection_id, database, collections \| exclude_collections, storage_target_id, gzip}` | 202 | 400, 409 |
| `DELETE` | `/api/v1/backups/{id}` | Delete a backup and its artifact on the backup's storage target | 200 | 404 |
| `POST` | `/api/v1/restore` | Restore (safe clone by default; optional `target_connection_id` (admin), `verify`) | 202 | 400, 403, 404, 409, 422 |
| `GET` | `/api/v1/restores` | Restore audit history | 200 | |
| `GET` / `POST` | `/api/v1/notifications/channels` | List / create notification channels | 200 / 201 | 400 |
| `PUT` / `DELETE` | `/api/v1/notifications/channels/{id}` | Update / delete a channel | 200 | 400, 404 |
| `POST` | `/api/v1/notifications/channels/{id}/test` | Send a test notification | 200 | 404 |
| `GET` / `POST` | `/api/v1/notifications/rules` | List / create notification rules | 200 / 201 | 400 |
| `PUT` / `DELETE` | `/api/v1/notifications/rules/{id}` | Update / delete a rule | 200 | 400, 404 |
| `GET` | `/metrics` | Prometheus metrics | 200 | 401 |
| `POST` | `/mcp` | [MCP](mcp.md) Streamable HTTP endpoint (JSON-RPC; stateless, so `GET` and `DELETE` answer 405); API keys only | 200 | 401, 403 disabled or foreign origin |

Every protected endpoint also answers `401` without valid credentials, `403` for a cookie request with a missing or wrong `X-CSRF-Token` and `403` for an API key whose scope is too small.

## Connections

Connection strings are validated, encrypted at rest and only ever returned redacted (`mongodb://user:******@host/...`). To keep the stored password when editing, send the URI back exactly as it was returned; any other value containing `******` is rejected. A connection test succeeds or fails with HTTP `200` (`ok: false` plus a redacted `error`) and times out after 10 seconds.

Jobs and manual backups name a `connection_id`. Backup records keep `connection_id` and a `connection_name` snapshot. A restore goes to the backup's connection unless `target_connection_id` names another one, which restores across servers and needs `admin`; restore records always carry `source_connection_id`, `source_connection_name`, `target_connection_id` and `target_connection_name`, the names as they were when the restore started.

## Settings

`GET /api/v1/settings` returns every setting grouped as `general`, `security` and `encryption` ([descriptions](configuration.md#settings)), plus `restart_required` (always empty: every change applies to the next operation or request). Durations are Go duration strings (`"6h0m0s"`), secrets (`encryption.identity`, `encryption.passphrase`) are `"******"` when set and `""` when not, and `encryption.retired_keys` lists `{kind, recipient, retired_at}` without key material.

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

Restoring in place (into the source database, or into `target_database`) must be confirmed explicitly with `{"safe_clone": false, "confirm_in_place": true}`; any other in-place request is rejected with `400 Bad Request` before `mongorestore` starts. In-place restores are verified first under the default `auto` verify policy. A missing decryption key is rejected up front with `422 Unprocessable Entity`; a checksum mismatch or failed decryption found during verification marks the restore record as failed, and `mongorestore` is never started.

## Asynchronous operations

Every backup record has a `trigger`: `scheduled` (a cron run of `job_id`), `on_demand` (`POST /api/v1/jobs/{id}/run`), `manual` (`POST /api/v1/backups`) or `mcp` (an assistant's `start_backup` or `run_job`). It is set by the server, never taken from the request, and retention only prunes a job's `scheduled` backups ([configuration.md](configuration.md#general)).

`POST /api/v1/backups`, `POST /api/v1/jobs/{id}/run` and `POST /api/v1/restore` return `202 Accepted` as soon as the operation has started. The response body is the new backup or restore record with `"status": "in_progress"`; poll `GET /api/v1/backups` or `GET /api/v1/restores` until it becomes `completed` or `failed`. Operations keep running if the client disconnects.

| Status | Meaning |
| --- | --- |
| `202 Accepted` | The operation started |
| `400 Bad Request` | Invalid request, for example a missing `connection_id`, an unknown `storage_target_id` or an unconfirmed in-place restore |
| `401 Unauthorized` | Not signed in and no valid API key |
| `403 Forbidden` | The API key's scope does not allow the operation (for example a read key starting a backup, or an operator key restoring in place) |
| `409 Conflict` | A backup of the same database on the same connection, or a restore into the same target, is already running |
| `422 Unprocessable Entity` | The backup is encrypted and no decryption key is configured |
| `503 Service Unavailable` | The server is shutting down |

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
