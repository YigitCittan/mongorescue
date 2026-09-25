# Changelog

All notable changes to **MongoRescue** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Added
- MCP server for AI assistants ([docs/mcp.md](docs/mcp.md)), built on the official MCP Go SDK: Streamable HTTP at `/mcp` on the main listener (API keys only, stateless, JSON responses) and `mongorescue mcp`, a stdio bridge that forwards to a running instance with `MONGORESCUE_MCP_API_KEY`. Tools `list_connections`, `list_databases`, `list_collections`, `list_jobs`, `get_job`, `list_backups`, `get_backup`, `list_restores`, `get_restore`, `list_storage_targets`, `get_status` (read) and `start_backup`, `run_job`, `restore_to_safe_clone` (operator), with MCP tool annotations; resources `mongorescue://status`, `mongorescue://backups/{id}`, `mongorescue://jobs/{id}`, `mongorescue://restores/{id}`; prompts `diagnose_failed_backup`, `disaster_recovery_plan`, `verify_recent_backups`. Settings → Security → *MCP server for AI assistants* (`security.mcp_enabled`, on by default) switches it off.
- API key scopes: `read` (GET-only REST and read-only MCP tools), `operator` (plus backups, job runs and safe-clone restores) and `admin` (everything), enforced centrally from one route → scope table; chosen when creating a key (default `read`) and shown in the key list.
- Audit log of every MCP tool call (time, API key, transport, tool, redacted arguments, result, duration) in the new `audit_log` table, shown under Settings → Security → *Recent API/MCP activity* and served by `GET /api/v1/audit` (admin).
- Per-API-key rate limit for MCP (60 calls per minute, bursts of 20) and the `mongorescue_mcp_calls_total{tool,result}` metric.
- Embedded SQLite metadata store (`mongorescue.db` in the data directory) replacing `state.json`: pure-Go driver (no CGO), WAL mode, versioned schema migrations, transactional multi-step writes, files created with mode `0600`.
- Automatic migration from `state.json`: on the first start with an empty database, every job, backup and restore record, notification channel and rule is imported in one transaction, the counts are verified, and the file is renamed to `state.json.migrated-<timestamp>`. A failed import leaves `state.json` untouched and stops startup; if the database already has data, `state.json` is ignored with a warning.
- Zero-configuration first run: without users the server starts in setup mode and logs a one-time setup code; `POST /api/v1/setup` creates the first user.
- User accounts (bcrypt), server-side sessions (`mr_session` cookie: HttpOnly, SameSite=Strict, Secure over TLS or a trusted proxy; 12 h idle / 7 d absolute) with CSRF tokens, login throttling, user management and password changes that revoke other sessions.
- API keys (`mr_<prefix>_<secret>`) created in the dashboard, stored as SHA-256 hashes, shown once, with last-use tracking.
- Managed MongoDB connections: CRUD with masked URIs, connection tests (server version, latency), database and collection discovery, and cross-server restores via `target_connection_id`. Jobs gain `exclude_collections`; backup and restore records keep the connection name.
- Encryption at rest of connection strings and notification channel secrets (AES-256-GCM) with `MONGORESCUE_SECRET_KEY` or a generated `<data_dir>/secret.key`, a key check that refuses a wrong key at startup, and automatic encryption of existing plaintext secrets.
- Legacy job connection strings are migrated into managed connections.
- An advisory lock on the data directory keeps a second instance from starting on it.
- `examples/with-mongodb.yml` (demo MongoDB) and a `.dockerignore`.

- Everything is configured in the dashboard and stored in the database: `GET`/`PUT /api/v1/settings` (general limits and defaults, security, encryption; validated, secrets masked, applied without a restart) and `POST /api/v1/settings/encryption/generate-key`. Replaced encryption identities and passphrases are kept as retired keys, so older backups stay restorable.
- Storage targets: several local directories and S3-compatible buckets (`/api/v1/storage-targets` with tests and a default target), a "Local disk" default on first start, per-target drivers rebuilt on change, S3 key prefixes. Jobs and manual backups take `storage_target_id`; backup records keep `storage_target_id` and `storage_target_name`, and restores, deletions and retention use the record's target. Targets in use cannot be deleted (409).
- One-time import of the deprecated environment variables and `<data_dir>/config.json` into the database, with a warning per source.

### Changed
- API keys created without a `scope` are read-only; existing keys and the key imported from `MONGORESCUE_API_KEY` become `admin` keys (migration `0004`), so current automation keeps working. In-place restores need an admin key or a session.
- The backup, job-run and restore use cases moved from the HTTP handlers into `internal/operations`, shared by the REST API and the MCP server; unexpected errors of these endpoints answer `internal error` instead of the raw error text.
- Only bootstrap options remain outside the database: `-data-dir`/`MONGORESCUE_DATA_DIR`, `-host`/`-port` (`MONGORESCUE_SERVER_HOST`/`_PORT`), `-log-level`, the optional `MONGORESCUE_SECRET_KEY` and `-version`.
- The API always requires a session or an API key; the unauthenticated mode is gone.
- Jobs and manual backups require `connection_id`; `mongo_uri` is no longer accepted in jobs, backups or restores.
- `docker-compose.yml` runs only MongoRescue and has no `environment` block.
- Concurrency keys and retention are per connection, so the same database name on two servers is independent.
- The MongoDB Go driver is a production dependency, confined to `internal/mongoconn`.

- `GET /api/v1/auth/me` answers signed-out visitors with `200` and `{user: null, csrf_token: "", auth: ""}`; restore records carry `source_connection_id` and `source_connection_name` next to the target connection fields.

### Removed
- The JSON configuration file (`-config`), `config.example.json`, `.env.example`, the flags `-api-key`, `-mongo-uri`, `-storage` and `-gen-age-key` (the dashboard generates keys), `database_path`/`MONGORESCUE_DB_PATH` (the database is always `<data_dir>/mongorescue.db`), `GET /api/v1/config` and every other environment variable (static API key, MongoDB URI, storage, encryption, restore, timeouts, CORS, metrics, cookies, proxy headers).

### Security
- The MCP endpoint never accepts session cookies, refuses foreign `Origin` headers and non-local `Host` names on a loopback listener (DNS rebinding) unless proxy headers are trusted, exposes no tool that deletes, restores in place or changes configuration, filters and re-checks tools by scope, and never returns connection strings or storage credentials.
- Login attempts are reserved atomically before the password check (one in flight per client IP and username, four per IP), so parallel requests cannot exceed the failure budget.
- Every login performs exactly one bcrypt comparison whether or not the user exists; passwords over 72 bytes are rejected before the user lookup.
- The per-IP failure count only tightens the per-user budget and never refuses a correct password for an unlocked user; wrong setup codes are throttled only after 100 per IP in 15 minutes and never block the correct code.
- Deleting a user revokes the API keys they created; keys whose creator no longer exists are refused.
- Credentials at rest use the `sb2:` format, whose associated data binds each value to its table, record ID and field; `sb1:` and plaintext values are re-sealed once, recorded by a `secrets_format` marker, and afterwards refused at startup (naming table, record and field) and on read.
- Secrets are always encrypted on write, even when they look sealed already (a value starting with `sb1:` was stored as is and broke the channel list).
- The startup check for encrypted values without a key check value inspects only secret fields, so a name containing `sb1:` no longer blocks startup.
- Storage targets are updated with optimistic concurrency and never recreated by a concurrent test or edit; the location of a target holding backups cannot change; local paths must be absolute and outside the data directory; tests of unsaved targets create no directories.
- Generated job, backup and restore IDs carry a random suffix, and creating a job inserts (409 on an existing ID) instead of silently overwriting another job created in the same second; job updates and scheduler writes never recreate a deleted job.
- Logins are never refused because other clients behind the same address are busy; bcrypt work is bounded by a global pool in which logins wait (up to 5 s).
- A generated `secret.key` is made durable (file and directory fsync) before the database records its key check value.
- `POST /api/v1/setup` and `/api/v1/auth/login` require `Content-Type: application/json` (415) and a same-origin or allowed `Origin` header when present (403), preventing login CSRF.

## [Unreleased]

### Changed
- Building from source now requires Go 1.26 or newer; Go 1.25 is no longer supported upstream.
- Updated golang.org/x dependencies; the container image is built with Go 1.27.

## [0.1.0] - 2026-09-25

Requires Go 1.25+ to build.

### Added
- **Stream-First Core Engine**:
  - Unix pipe streaming from `mongodump` directly into storage drivers with constant $O(1)$ memory usage (~15–20 MB).
  - In-flight SHA-256 integrity hash calculation and atomic byte counting without buffering.
- **Disaster Recovery Engine**:
  - Safe clone namespace routing (`<db>_rescue_<timestamp>`) preventing accidental production data overwrite.
  - Granular collection filtering via REST payload (`selected_collections: ["users"]`).
  - Dry-run validation mode and target drop protection safeguards.
- **Pluggable Storage Abstraction**:
  - Local filesystem storage driver with directory traversal validation and atomic temporary files (`.tmp`).
  - S3-compatible storage driver with official AWS SDK v2 multipart uploader (AWS S3, MinIO, Cloudflare R2, Wasabi).
- **Automated Scheduler & Retention**:
  - Thread-safe JSON metadata disk store.
  - Multi-job cron scheduler with graceful shutdown context cancellation.
  - Retention policies supporting both elapsed days and max archive count pruning.
- **Embedded Web Dashboard**:
  - Single-binary distribution with zero npm/Node.js runtime dependencies via Go `embed.FS`.
  - Modern dark-mode dashboard with real-time KPI metrics and one-click recovery modals.
  - 8-language instant internationalization (i18n) supporting English, Turkish, German, Spanish, French, Chinese, Japanese, and Russian with browser auto-detection and `localStorage` persistence.
- **Encryption at Rest**:
  - Streaming [age](https://age-encryption.org) encryption of backups with X25519 recipients or a passphrase (scrypt); encrypted artifacts are stored with a `.age` suffix.
  - `MONGORESCUE_ENCRYPTION_*` settings (`encryption` in JSON) and a `-gen-age-key` flag that prints a new key pair.
  - Recipient-only instances can take backups without holding the private key; unencrypted backups keep restoring unchanged.
- **Verify-Before-Restore**:
  - Restores can first stream the artifact end to end, checking its SHA-256 and decrypting it, before `mongorestore` starts; failures return HTTP 422 and leave the target untouched.
  - Policy via `MONGORESCUE_RESTORE_VERIFY` / `restore.verify_before_restore` (`always`, `auto`, `never`; `auto` verifies non-safe-clone restores) and a per-request `verify` override.
- **Notifications**:
  - Dashboard-managed channels: signed webhook (HMAC-SHA256, versioned JSON payload), Telegram bot, SMTP email (`none`/`starttls`/`tls`) and Twilio SMS.
  - Rules mapping backup and restore success/failure events, optionally filtered by job, to channels; asynchronous delivery with retries and a per-attempt timeout that never blocks backups.
  - "Send test" action and masked secrets in API responses; REST API under `/api/v1/notifications`.
- **Prometheus Metrics**:
  - `GET /metrics` with backup/restore counters, durations, sizes, last-success timestamps, notification outcomes and build info; protected by the API key unless `MONGORESCUE_METRICS_PUBLIC=true`.
- **Integration Test Suite**:
  - `integration`-tagged tests driving real `mongodump`/`mongorestore`, the full HTTP API, and a storage conformance suite across local disk and an S3 provider matrix (MinIO, LocalStack, AWS, Cloudflare R2, Backblaze B2, DigitalOcean Spaces, Wasabi).
  - `make test-integration-docker` and `make docker-smoke`; CI runs emulator suites on every push and cloud providers on `main` and tags.
- **Security**:
  - API Key authentication supporting Bearer tokens and `X-API-Key` headers.
  - Opt-in CORS middleware driven by an explicit origin allowlist.
  - Automatic MongoDB URI credential sanitization across logs and API outputs.
- **CI/CD & Release Automation**:
  - Multi-OS matrix testing (Ubuntu, macOS on Go 1.25.x and 1.26.x) with race detection (`-race`).
  - Multi-arch Docker images (`linux/amd64`, `linux/arm64`) published to GitHub Container Registry (`ghcr.io`).
  - Cross-platform release archive builds via GoReleaser with SHA-256 checksums.

- `config.example.json` with every configuration key, shipped in the release archives.
- The container image runs as an unprivileged user (UID 10001) and defines a `HEALTHCHECK`.
- Unit tests run on Windows in CI in addition to Linux and macOS.

### Security
- Hardened API key authentication: hash-based constant-time key comparison, rejection of empty Bearer tokens, and a startup warning when the server runs unauthenticated on a non-loopback address.
- CORS is disabled by default; cross-origin access requires an explicit allowlist via `MONGORESCUE_CORS_ORIGINS`.
- Fixed stored XSS in the web dashboard via server-side ID validation and removal of inline event handlers.
- MongoDB connection URIs are redacted in all API responses.
- The scrypt work factor for passphrase encryption and decryption is capped (a crafted backup cannot exhaust memory), and a warning is logged when an age identity file is readable by group or others.
- Notification channel secrets are masked in API responses and scrubbed from delivery errors; header values are checked for CR/LF injection.

### Fixed
- Scheduler persists the final job and backup state on graceful shutdown.
- S3 uploads only send integrity checksums when required, restoring compatibility with Cloudflare R2, Backblaze B2 and MinIO.
- A backup no longer hangs when the storage upload fails.
- Failed or cancelled backups no longer leave truncated artifacts in storage.
- `mongodump`/`mongorestore` runs are bounded by configurable timeouts and a stall watchdog, and whole process trees are terminated on cancel or shutdown.
- Manual backups, job runs and restores run in the background (`202 Accepted`) and survive client disconnects; concurrent runs for the same database return `409 Conflict`.
- API restores default to a safe clone; overwriting a database requires `confirm_in_place`.
- Scheduled jobs report their next run time correctly.
- The container image stores state and local backups in its declared `/data` and `/backups` volumes.
- Local storage rejects absolute and drive-rooted keys on every platform (previously accepted on Windows).
