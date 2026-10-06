# Audit log

MongoRescue keeps two logs of what happened:

- The **audit log** (this page, `GET /api/v1/audit/events`, Settings → **Audit log**) records every action: who did what to which object, with what result, from where. It is append-only and hash-chained, so a changed or removed entry is detected.
- The **API key activity log** (`GET /api/v1/audit`, Settings → Security → *Recent API/MCP activity*) is the detailed trace of API keys and MCP clients, with their redacted arguments and durations. See [api.md](api.md#api-key-activity) and [mcp.md](mcp.md).

The activity log stores arguments, which the audit log never does, and merges repeated calls into one row, which a hash chain does not allow; that is why they stay separate. MCP tool calls and system actions recorded in the activity log are also written to the audit log.

## What is recorded

| Action | Actor | `action` |
| :--- | :--- | :--- |
| Every REST request that can change something (`POST`, `PUT`, `PATCH`, `DELETE`), from a dashboard session or an API key, including refused ones (wrong scope, missing CSRF token, validation errors) | `user` or `api_key` | The route pattern, e.g. `POST /api/v1/jobs/{id}/run`, or `POST (no route)` |
| The same requests without valid credentials (`401`), and sign-in or setup requests refused before they are read (`415` wrong media type, `403` cross-origin) | `anonymous` | The route pattern |
| Settings changes: the names of the changed sections and keys in `targets` (`{"sections": "audit,general", "keys": "audit.retention_days,general.default_gzip"}`), never their values | `user` or `api_key` | `PUT /api/v1/settings` |
| User and key changes: the `role` of a new user, `role_from` and `role_to` of a [role change](design/roles.md), the `scope` of a new API key and `ceiling_applied` (`true` when the key has a creator whose role caps it) | `user` or `api_key` | `POST /api/v1/users`, `PUT /api/v1/users/{id}/role`, `POST /api/v1/api-keys` |
| Sign-in, successful or not, and setup. A password sign-in refused because the password form is limited to local administrators has `targets` `reason: local_login_disabled` | `user` on success, otherwise `anonymous` with the name that was tried | `POST /api/v1/auth/login`, `POST /api/v1/setup` |
| [Single sign-on](sso.md) callbacks (status `302`), with `targets` `provider: oidc`; on success also `created` and the role (`role_to`, and `role_from` when the user existed), on failure the `reason` (the `oidc_error` code). Tokens, codes and claims are never recorded; starts of a sign-in are not recorded | `user` on success, otherwise `anonymous`, named only once the ID token's signature verified | `GET /auth/oidc/callback` |
| Sign-out | `user` | `POST /api/v1/auth/logout` |
| Downloads that copy data out: the recovery kit (`POST`) and the audit log export (`GET`) | `user` or `api_key` | `POST /api/v1/recovery-kit`, `GET /api/v1/audit/events/export` |
| MCP tool calls (not resource reads or prompts) | `api_key` | `MCP <tool>`, e.g. `MCP start_backup` |
| Actions MongoRescue takes on its own: retention deleting a backup, and the audit log's own retention (`targets`: `removed`, the new `anchor_id` and `anchor_hash`) | `system` | `SYSTEM <action>`, `SYSTEM audit.prune` |
| Every [post-restore command](api.md#post-restore-commands) a restore ran, successful or not (`targets`: `restore_id`, `connection_id`, `database` (the clone), `source_database`, `collection`, `command`, `index`, the counts `n`, `modified` and `upserted`, and a redacted `error`); never the command document | `system` (`post_restore`) | `SYSTEM restore.post_restore_command` |

Other `GET` requests (with or without credentials), `/metrics` scrapes and MCP requests other than tool calls are not recorded.

## Entry

```json
{"id": 1842, "time": "2026-10-02T12:30:45.123456789Z", "actor_kind": "user", "actor_user_id": "usr_1a2b3c4d5e6f7a8b",
 "actor_name": "admin", "actor_key_id": "", "actor_key_name": "", "action": "DELETE /api/v1/backups/{id}",
 "targets": {"id": "bkp_shop_20260901_030000_1a2b3c4d"}, "status": 200, "outcome": "ok",
 "client_ip": "192.0.2.10", "user_agent": "Mozilla/5.0 ...", "count": 1,
 "hash": "5d0c9e…"}
```

| Field | Meaning |
| :--- | :--- |
| `id` | Position in the chain: the previous entry's ID + 1 |
| `time` | When the entry was recorded (after the response was written), UTC, always nine fractional digits (`2026-10-02T12:31:00.000000000Z`) |
| `actor_kind` | `user` (session), `api_key`, `system` or `anonymous` |
| `actor_user_id`, `actor_name` | The user and a snapshot of their name. For a failed sign-in, the name that was tried (never the password); for system actions the component (`retention`) |
| `actor_key_id`, `actor_key_name` | The API key and a snapshot of its name |
| `action` | Route pattern, `MCP <tool>` or `SYSTEM <action>`; never the raw path or query |
| `targets` | The path parameters (`{"id": "job_1"}`), names the handler adds (changed settings), the `id`/`*_id` arguments of a tool call or system action, or the window of a summary entry (below) |
| `status`, `outcome` | HTTP status and `ok` (below 400), `denied` (401, 403), `rate_limited` (429) or `error`. Status 0 for MCP and system entries, and with outcome `error` for a request whose handler panicked or wrote no status |
| `client_ip` | The client address; `X-Forwarded-For` (last entry) or `X-Real-IP` only with **Trust proxy headers** (`security.trust_proxy_headers`), the same rule as sign-in throttling |
| `user_agent` | The `User-Agent` header, cut at 256 bytes |
| `count` | `1`, or for a summary entry the number of identical refusals it stands for (see below) |
| `hash` | The chain hash |

Request and response bodies are never stored. Strings are stored as valid UTF-8 without control characters, and lengths are bounded (names and IDs 128 bytes, actions and target values 256 bytes, at most 16 targets).

Refusals (`denied` or `rate_limited`) are coalesced so that a client hammering a refused route or the sign-in form cannot fill the database, without losing any of them. The first refusal of a caller opens a 10-second window and is stored at once; identical refusals within the window are counted. When the window closes (checked every second), when the service stops, or when 4096 windows are open at once, a **summary entry** is written with the same actor, action, status, client and `reason`, `count` set to the number of counted refusals and `targets` `coalesced_from` and `coalesced_until` (the window). Authenticated callers are matched by user or key; anonymous ones (failed sign-ins, missing credentials) by client address, action, outcome and `reason` (a code the server chooses, such as a single sign-on failure) only, whatever name they try, so the entries per address stay bounded: the summary keeps up to 10 distinct attempted names (`name_1` … `name_10`, 64 bytes each) and counts the refusals whose name it did not keep in `names_more`. So each caller produces at most two entries per window.

Entries are written by one writer goroutine through a queue of 4096, off the request path. When the queue is full, the request writes its entry itself rather than dropping it (`mongorescue_audit_sync_writes_total`); at shutdown the open windows are summarised and the queue is drained before the database closes. A failed write is logged and counted (`mongorescue_audit_write_failures_total`); `mongorescue_audit_queue_depth` shows the backlog ([metrics.md](metrics.md)).

## Hash chain

Each entry's hash is

```
hash = lowercase hex( SHA-256( prev_hash || canonical_json(entry without hash) ) )
```

where `prev_hash` is the previous entry's `hash` as 64 ASCII characters (64 zeros for the first entry, or the anchor's hash after retention) and `canonical_json` is the entry's JSON with:

- exactly the fields `id, time, actor_kind, actor_user_id, actor_name, actor_key_id, actor_key_name, action, targets, status, outcome, client_ip, user_agent, count`, in this order;
- no whitespace, UTF-8, no escaping beyond `\"` and `\\` (stored strings hold no control characters), `<`, `>` and `&` unescaped;
- `targets` as an object with its keys sorted (`{}` when empty);
- `time` as RFC 3339 in UTC with exactly nine fractional digits (Go layout `2006-01-02T15:04:05.000000000Z`: `2026-10-02T12:31:00.000000000Z`), exactly as in the exported entry.

An exported line therefore verifies with nothing but its predecessor: drop `hash`, re-encode the fields in this order (in Python, `json.dumps(fields, separators=(",", ":"), ensure_ascii=False)` with the dict built in this order and `targets` sorted), prepend the previous hash, hash. The test vectors in `internal/auditlog/auditlog_test.go` (`TestCanonicalVector`, two chained entries with their canonical JSON and hashes, checked independently with Python's `json` and `hashlib`) pin the encoding.

Entries are appended in one write transaction that reads the newest entry (or the anchor), so concurrent requests never fork the chain. The database refuses updates of `audit_events` and deletions ahead of the anchor with triggers; the store has no update or delete path other than retention.

`GET /api/v1/audit/events/verify` (admin) and **Verify chain** in the dashboard walk the chain from the anchor and report the first entry whose ID does not follow its predecessor (removed or reordered entries) or whose hash does not match (a changed entry, or a changed predecessor):

```json
{"ok": false, "checked": 1203,
 "anchor": {"last_id": 640, "last_hash": "…", "last_time": "2025-09-29T23:58:12.000000000Z", "pruned_at": "2026-09-30T00:00:00Z"},
 "head_id": 1843, "head_hash": "…", "broken_id": 1844,
 "reason": "the hash does not match the entry's content and its predecessor's hash: the entry, or the one before it, was changed",
 "retention_days": 365, "verified_at": "2026-10-02T12:40:00Z"}
```

A chain that verifies can still carry a `warning`: when the last removed entry (`anchor.last_time`) is newer than now minus `audit.retention_days` (with a day of tolerance), the anchor moved further than retention explains, so entries may have been removed early (or the retention was raised after the last prune). The dashboard shows the anchor, when retention last pruned, and the warning.

What the chain does and does not prove: it detects any change to an entry and any removal from the middle of the log by someone who does not also rewrite every later entry. Someone with write access to `mongorescue.db` can drop the triggers, rewrite the chain from some point on, or cut off the newest entries; the hash function has no secret. Keep a copy outside the host to detect that: forward entries to a webhook (below), export the log regularly, or note `head_id` and `head_hash` from a verification and compare them later.

## Retention

`audit.retention_days` (Settings → Audit log, default 365, at least 30, at most 36500) is applied at start and hourly. Pruning removes every entry recorded before the cutoff, and first stores the last removed entry's ID, hash and time as the chain **anchor** (one row in `audit_chain_anchor`, which only moves forward), so verification starts from a known hash and still detects changes to every kept entry. The anchor is part of the verification result, and every prune writes a `SYSTEM audit.prune` entry with the number of removed entries and the new anchor.

## Export

`GET /api/v1/audit/events/export` (admin, **Export JSON Lines** in the dashboard) streams the entries matching the same filters as the list (`actor`, `actor_kind`, `action`, `result`, `since`, `until`), oldest first, one JSON entry per line (`application/x-ndjson`, `Content-Disposition: attachment`). It reads the log in batches, so it never holds the database while a slow client downloads. Every export is itself recorded.

## Forwarding

With `audit.webhook_url` set, every stored entry is also sent as `POST <url>` with the entry's JSON as the body (the same fields as above, `hash` included), `Content-Type: application/json` and, when `audit.webhook_secret` is set, `X-MongoRescue-Signature: sha256=<hex HMAC-SHA256 of the body>` (the same signature as [notification webhooks](notifications.md)).

- Delivery runs in the background and never delays a request. Entries wait in a queue of 1024; when it is full, new entries are dropped from forwarding (never from the log) and counted.
- Each entry is sent once, in order, by one worker, with a 10-second timeout and without redirects. A failure (non-2xx answer or no connection) is counted, not retried: the database stays authoritative, and the receiver can spot gaps by `id`.
- The URL and the secret are stored encrypted (secretbox) like other credentials; API responses show the URL only up to its host (`https://siem.example.com/******`) and the secret as `******`. Link-local and cloud metadata addresses are refused when connecting, as for notifications.
- `GET /api/v1/audit/events` reports `forwarding: {enabled, queued, sent, failed, dropped, last_error, last_error_at}` since start, and `mongorescue_audit_forward_total{outcome="sent|failed|dropped"}` counts them ([metrics.md](metrics.md)).

## Not yet

- Syslog (RFC 5424) forwarding.
