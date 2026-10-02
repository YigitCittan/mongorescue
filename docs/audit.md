# Audit log

MongoRescue keeps two logs of what happened:

- The **audit log** (this page, `GET /api/v1/audit`, Settings → **Audit log**) records every action: who did what to which object, with what result, from where. It is append-only and hash-chained, so a changed or removed entry is detected.
- The **API key activity log** (`GET /api/v1/audit/activity`, Settings → Security → *Recent API/MCP activity*) is the detailed trace of API keys and MCP clients, with their redacted arguments and durations. See [api.md](api.md#api-key-activity) and [mcp.md](mcp.md).

The activity log stores arguments, which the audit log never does, and merges repeated calls into one row, which a hash chain does not allow; that is why they stay separate. MCP tool calls and system actions recorded in the activity log are also written to the audit log.

## What is recorded

| Action | Actor | `action` |
| :--- | :--- | :--- |
| Every REST request that can change something (`POST`, `PUT`, `PATCH`, `DELETE`), from a dashboard session or an API key, including refused ones (wrong scope, missing CSRF token, validation errors) | `user` or `api_key` | The route pattern, e.g. `POST /api/v1/jobs/{id}/run`, or `POST (no route)` |
| Sign-in, successful or not, and setup | `user` on success, otherwise `anonymous` with the name that was tried | `POST /api/v1/auth/login`, `POST /api/v1/setup` |
| Sign-out | `user` | `POST /api/v1/auth/logout` |
| Downloads that copy data out: the recovery kit (`POST`) and the audit log export (`GET`) | `user` or `api_key` | `POST /api/v1/recovery-kit`, `GET /api/v1/audit/export` |
| MCP tool calls (not resource reads or prompts) | `api_key` | `MCP <tool>`, e.g. `MCP start_backup` |
| Actions MongoRescue takes on its own (retention deleting a backup) | `system` | `SYSTEM <action>` |

Other `GET` requests, `/metrics` scrapes and requests without valid credentials (other than sign-in and setup) are not recorded.

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
| `time` | When the entry was recorded (after the response was written), UTC, nanoseconds |
| `actor_kind` | `user` (session), `api_key`, `system` or `anonymous` |
| `actor_user_id`, `actor_name` | The user and a snapshot of their name. For a failed sign-in, the name that was tried (never the password); for system actions the component (`retention`) |
| `actor_key_id`, `actor_key_name` | The API key and a snapshot of its name |
| `action` | Route pattern, `MCP <tool>` or `SYSTEM <action>`; never the raw path or query |
| `targets` | The path parameters (`{"id": "job_1"}`), or the `id`/`*_id` arguments of a tool call or system action |
| `status`, `outcome` | HTTP status (0 for MCP and system entries) and `ok` (below 400), `denied` (401, 403), `rate_limited` (429) or `error` |
| `client_ip` | The client address; `X-Forwarded-For` (last entry) or `X-Real-IP` only with **Trust proxy headers** (`security.trust_proxy_headers`), the same rule as sign-in throttling |
| `user_agent` | The `User-Agent` header, cut at 256 bytes |
| `count` | `1`, or for a refusal the number of identical refusals it stands for (see below) |
| `hash` | The chain hash |

Request and response bodies are never stored. Strings are stored as valid UTF-8 without control characters, and lengths are bounded (names and IDs 128 bytes, actions and target values 256 bytes, at most 16 targets).

Identical refusals (`denied` or `rate_limited`, same actor, client, action and status) within 10 seconds are counted instead of stored one by one, so a client hammering a refused route or the sign-in form cannot fill the database. The next stored refusal of that caller carries the count of the ones it stands for. Counts of a burst that ends just before a restart are lost; the first refusal of every burst is always stored.

## Hash chain

Each entry's hash is

```
hash = lowercase hex( SHA-256( prev_hash || canonical_json(entry without hash) ) )
```

where `prev_hash` is the previous entry's `hash` as 64 ASCII characters (64 zeros for the first entry, or the anchor's hash after retention) and `canonical_json` is the entry's JSON with:

- exactly the fields `id, time, actor_kind, actor_user_id, actor_name, actor_key_id, actor_key_name, action, targets, status, outcome, client_ip, user_agent, count`, in this order;
- no whitespace, UTF-8, no escaping beyond `\"` and `\\` (stored strings hold no control characters), `<`, `>` and `&` unescaped;
- `targets` as an object with its keys sorted (`{}` when empty);
- `time` as RFC 3339 in UTC with nanoseconds, trailing zeros removed (`2026-10-02T12:31:00Z`, `2026-10-02T12:30:45.123456789Z`), exactly as in the exported entry.

An exported line therefore verifies with nothing but its predecessor: drop `hash`, re-encode the fields in this order (in Python, `json.dumps(fields, separators=(",", ":"), ensure_ascii=False)` with the dict built in this order and `targets` sorted), prepend the previous hash, hash. The test vectors in `internal/auditlog/auditlog_test.go` (`TestCanonicalVector`, two chained entries with their canonical JSON and hashes, checked independently with Python's `json` and `hashlib`) pin the encoding.

Entries are appended in one write transaction that reads the newest entry (or the anchor), so concurrent requests never fork the chain. The database refuses updates of `audit_events` and deletions ahead of the anchor with triggers; the store has no update or delete path other than retention.

`GET /api/v1/audit/verify` (admin) and **Verify chain** in the dashboard walk the chain from the anchor and report the first entry whose ID does not follow its predecessor (removed or reordered entries) or whose hash does not match (a changed entry, or a changed predecessor):

```json
{"ok": false, "checked": 1203, "anchor": {"last_id": 640, "last_hash": "…", "pruned_at": "2026-09-30T00:00:00Z"},
 "head_id": 1843, "head_hash": "…", "broken_id": 1844,
 "reason": "the hash does not match the entry's content and its predecessor's hash: the entry, or the one before it, was changed",
 "verified_at": "2026-10-02T12:40:00Z"}
```

What the chain does and does not prove: it detects any change to an entry and any removal from the middle of the log by someone who does not also rewrite every later entry. Someone with write access to `mongorescue.db` can drop the triggers, rewrite the chain from some point on, or cut off the newest entries; the hash function has no secret. Keep a copy outside the host to detect that: forward entries to a webhook (below), export the log regularly, or note `head_id` and `head_hash` from a verification and compare them later.

## Retention

`audit.retention_days` (Settings → Audit log, default 365, at least 30, at most 36500) is applied at start and hourly. Pruning removes every entry recorded before the cutoff, and first stores the last removed entry's ID and hash as the chain **anchor** (one row in `audit_chain_anchor`, which only moves forward), so verification starts from a known hash and still detects changes to every kept entry. The anchor is part of the verification result.

## Export

`GET /api/v1/audit/export` (admin, **Export JSON Lines** in the dashboard) streams the entries matching the same filters as the list (`actor`, `actor_kind`, `action`, `result`, `since`, `until`), oldest first, one JSON entry per line (`application/x-ndjson`, `Content-Disposition: attachment`). It reads the log in batches, so it never holds the database while a slow client downloads. Every export is itself recorded.

## Forwarding

With `audit.webhook_url` set, every stored entry is also sent as `POST <url>` with the entry's JSON as the body (the same fields as above, `hash` included), `Content-Type: application/json` and, when `audit.webhook_secret` is set, `X-MongoRescue-Signature: sha256=<hex HMAC-SHA256 of the body>` (the same signature as [notification webhooks](notifications.md)).

- Delivery runs in the background and never delays a request. Entries wait in a queue of 1024; when it is full, new entries are dropped from forwarding (never from the log) and counted.
- Each entry is sent once, in order, by one worker, with a 10-second timeout and without redirects. A failure (non-2xx answer or no connection) is counted, not retried: the database stays authoritative, and the receiver can spot gaps by `id`.
- The URL and the secret are stored encrypted (secretbox) like other credentials; API responses show the URL only up to its host (`https://siem.example.com/******`) and the secret as `******`. Link-local and cloud metadata addresses are refused when connecting, as for notifications.
- `GET /api/v1/audit` reports `forwarding: {enabled, queued, sent, failed, dropped, last_error, last_error_at}` since start, and `mongorescue_audit_forward_total{outcome="sent|failed|dropped"}` counts them ([metrics.md](metrics.md)).

## Not yet

- Syslog (RFC 5424) forwarding.
