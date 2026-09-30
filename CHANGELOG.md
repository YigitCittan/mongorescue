# Changelog

All notable changes to **MongoRescue** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Added
- Desktop app on Windows: the app keeps running in the background with a tray icon, so scheduled backups continue while the window is closed. Closing the window hides it (a one-time notification says so), and clicking the icon shows it again. The tray menu (Turkish or English, following the Windows display language) offers **Open MongoRescue**, a status line (*Idle*, *Running: 1 backup*), **Start with Windows** (`HKCU\…\Run` value `MongoRescue` = `"<exe>" --hidden`), **Quit** and **Force quit**. **Quit** waits for running backups and restores and can be cancelled. Meanwhile scheduled runs are skipped, and new runs from the dashboard, the API or MCP are refused with `503` *MongoRescue is shutting down*. **Force quit** asks for confirmation, then cancels the runs and records them as failed with `cancelled: application force quit (<detail>)`. Without a working tray icon, closing the window quits the app. `--hidden` starts the app hidden in the tray. macOS and Linux are unchanged. See [docs/desktop.md](docs/desktop.md#running-in-the-background).
- Desktop app on Windows: when an older per-machine copy is still installed in Program Files, a one-time bar offers **Remove** (runs that copy's uninstaller silently; Windows asks for administrator rights) or **Dismiss**. Both copies share the data in `%APPDATA%\MongoRescue`, so nothing is migrated; the update moves a per-machine copy to a per-user one, and the old copy's shortcuts remain until it is removed. See [docs/desktop.md](docs/desktop.md#updates).

### Changed
- Desktop app on Windows: **Update** now happens inside the app, with no installer window and no administrator prompt, when the app's folder is writable (the new per-user install and portable copies). The app downloads the release's portable zip, verifies its SHA-256, unpacks it next to `MongoRescue.exe` (only `MongoRescue.exe` and plain, safely named files in `tools\`, streamed through a root-scoped handle, size-limited), waits while a backup or restore runs (*Update will install after the running backup finishes*), renames the running `MongoRescue.exe` and `tools\` to `*.old`, moves the new ones in, starts the new version with `--after-update=<pid>` and closes; any failure rolls every rename back and keeps the running version. The bar and the header button show *Downloading x%*, *Installing…* and *Restarting…*; the new version waits for the old process to exit (and up to 30 s more for the data directory, with an error dialog if it stays locked), comes back in a second or two, repairs a swap cut short by a crash and removes only the update's own leftovers. Mandatory updates use the same path. See [docs/desktop.md](docs/desktop.md#updates).
- Windows installer: installs per user by default, without a UAC prompt, into `%LOCALAPPDATA%\Programs\MongoRescue`, with shortcuts and the uninstall entry under HKCU (it was `Program Files\MongoRescue\MongoRescue`, per machine). `-DWAILS_INSTALL_SCOPE=machine` builds the per-machine installer.
- Desktop app on Windows, copies the user cannot write to (a per-machine install in Program Files): the update runs the verified installer silently with `/S /RELAUNCH=<DOMAIN\user> /WAITPID=<pid>` instead of opening the setup wizard. The installer waits up to 180 s for the app to exit (outlasting its shutdown) and starts the new version only when it runs as that same user, so another administrator's UAC credentials never start the app under their account. When it gives up waiting, it starts the installed version again before aborting. If the installer does not start, the app stays open with **Try again** and the release page.

## [0.6.0] - 2026-09-30

### Added
- Retry a failed backup: `POST /api/v1/backups/{id}/retry` (operator scope) starts a new manual backup with the failed backup's connection, database, collections and storage target (and its job's exclusions and compression when the job still exists), through the same use case as "Back up now". It answers `409` for a backup that has not failed and `422` when its connection or storage target was deleted. The failed record is kept unchanged; the new record's `retry_of` names it (schema migration `0009_backup_retry_of`). See [docs/api.md](docs/api.md#retrying-a-failed-backup).
- Dashboard: failed backups get a **Retry** button, rows note "Retry of …" and "Retried at … → …", and a **Details** dialog shows the full error message, absolute start and end times, duration, trigger, connection, database, storage target and the retry chain.

### Fixed
- Backups and restores of a connection whose URI has credentials but no `authSource` (for example `mongodb://user:pass@host:27017`) failed with `AuthenticationFailed`, although the connection test passed: `mongodump`/`mongorestore` authenticated against the database being backed up or restored. The tools now authenticate against the same database as the driver, the URI path database or `admin`. URIs with an explicit `authSource`, `mongodb+srv://` URIs and `$external` mechanisms (`MONGODB-X509`, `GSSAPI`, `PLAIN`, `MONGODB-AWS`) are unchanged.

## [0.5.1] - 2026-09-30

### Fixed
- `mongodump not found` in the desktop app on machines without the MongoDB Database Tools: every release package bundles `mongodump` and `mongorestore` from the MongoDB Database Tools 100.12.2 (Apache License 2.0, with MongoDB's `LICENSE.md` and `THIRD-PARTY-NOTICES`), so the tools no longer have to be installed: `<install dir>\tools` on Windows (installer and portable zip; the uninstaller removes them), `MongoRescue.app/Contents/Resources/tools` on macOS (universal binaries) and `tools/` next to the binary on Linux. See [docs/desktop.md](docs/desktop.md#install).

### Changed
- Release workflow: the `desktop` job downloads the MongoDB Database Tools archives from `fastdl.mongodb.org`, verifies them against SHA-256 values pinned in the workflow, and builds without `wails build -clean` so the staged tools survive. See [docs/desktop.md](docs/desktop.md#building).

## [0.5.0] - 2026-09-29

### Added
- Dashboard: the build version (for example `v0.4.1`, as reported by `GET /api/v1/health`) is shown next to the brand in the header.
- Desktop app: while an optional update is available, an **Update** button stays in the header, also after **Later** hid the bar; it shows the bar again and starts the update, or opens the release page when the release has no file for your system. See [docs/desktop.md](docs/desktop.md#updates).

### Changed
- Release workflow: can sign the Windows desktop exe and installer through SignPath once the `SIGNPATH_API_TOKEN` secret and `SIGNPATH_ORGANIZATION_ID` variable are configured; until then Windows builds stay unsigned as before. See [docs/desktop.md](docs/desktop.md#code-signing).

### Fixed
- `mongodump not found` when the MongoDB Database Tools are installed but not on the app's `PATH` (the Windows MSI installs them to `C:\Program Files\MongoDB\Tools\<version>\bin` without adding it to `PATH`; the macOS desktop app launched from Finder gets a minimal `PATH` without Homebrew): after `PATH`, MongoRescue also searches well-known install directories (`/opt/homebrew/bin`, `/usr/local/bin`, `/opt/local/bin` on macOS; `%ProgramW6432%` / `%ProgramFiles%\MongoDB\Tools\<version>\bin` on Windows, newest version first; `/usr/local/bin`, `/usr/bin`, `/snap/bin` on Linux). See [docs/configuration.md](docs/configuration.md#mongodb-database-tools).
- Windows desktop shortcuts and taskbar pins kept showing the default Wails "W" icon of an older install: the installer (and uninstaller) now tells the shell to refresh its icon cache (`SHChangeNotify(SHCNE_ASSOCCHANGED)`). See [docs/desktop.md](docs/desktop.md#windows-installer-and-webview2).

### Security
- Dashboard: the storage provider shown for an S3 target is derived from the endpoint's host name (exact domain or subdomain match) instead of a substring anywhere in the URL.
- Stored credentials: sealing a value larger than 16 MiB fails with `secretbox.ErrTooLarge` instead of risking an oversized allocation.
- Logs: importing the deprecated static API key no longer logs the key's lookup prefix, and the "deprecated" warnings take the source name from constants rather than from the configuration next to the key.
- Docker image: the `golang` and `alpine` base images are pinned by digest; the release workflow installs a fixed NSIS version with checksums required.

## [0.4.0] - 2026-09-28

### Added
- `-tools-dir` flag and `MONGORESCUE_TOOLS_DIR` environment variable: an absolute directory searched first for `mongodump` and `mongorestore`. Without it, MongoRescue looks in `tools/` next to its executable, in `Contents/Resources/tools/` of a macOS `.app`, then on `PATH`, and logs the paths found at startup. See [docs/configuration.md](docs/configuration.md#mongodb-database-tools).
- Dashboard: a "New" button next to the storage target list in the "Back up now" dialog and the job form opens the storage target form on top of the dialog and selects the new target once it is saved; cancelling leaves the choice unchanged.
- Releases: `MongoRescue-desktop_<version>_checksums.txt`, the SHA-256 sums of the desktop installer and archives in `sha256sum` format. See [docs/desktop.md](docs/desktop.md#building).
- Desktop app: checks GitHub for a new release at startup and every 6 hours, and updates itself. A higher major version with installable files is mandatory (a blocking "Update required" screen, also after a reload); minor and patch updates, and releases without a file for your system, are offered in a dismissible bar. Both show the release notes. The installer (Windows) or archive (macOS, Linux) is downloaded over HTTPS and verified against the release's SHA-256 checksums before it is used; on Windows it stays locked against changes until the installer has started. Offline, the app starts as usual. See [docs/desktop.md](docs/desktop.md#updates).
- Release: GitHub releases are created as drafts and published once the desktop builds and their checksums are attached.

### Changed
- Releases: the GitHub release notes are the version's `CHANGELOG.md` section (`scripts/changelog-section.sh`) instead of a generated commit list; a release without a section fails.
- Desktop app: the setup form fills in the one-time setup code itself and hides the field, so the first administrator only chooses a username and a password. The setup-code banner and the clipboard copy are gone; the server still verifies the code. If setup fails, the code field reappears and can be edited.

### Fixed
- Backups and restores no longer depend on the MongoDB Database Tools being on `PATH`: tools bundled next to the executable (such as with the desktop app on Windows) are found, and a missing tool fails with an actionable `mongodump not found: install MongoDB Database Tools or set MONGORESCUE_TOOLS_DIR` (searched locations are logged) instead of `executable file not found in %PATH%`.
- Desktop app: the window, taskbar, `.exe`, macOS `.app`, Linux window and the Windows installer and uninstaller show the MongoRescue logo instead of the Wails default.
- Windows installer: no longer looks hung while it installs the Microsoft Edge WebView2 runtime (typical on Windows Server, which ships without it). It detects per-machine and per-user runtimes, shows what it is doing and the bootstrapper's progress window, and reports a failed runtime install instead of ignoring it. See [docs/desktop.md](docs/desktop.md#windows-installer-and-webview2).

## [0.3.1] - 2026-09-28

### Fixed
- Release: the Windows desktop job now finds NSIS and attaches the installer (`MongoRescue-desktop_<version>_windows_amd64_installer.exe`) and a portable `.zip`; v0.3.0 shipped without Windows desktop builds.

## [0.3.0] - 2026-09-28

### Breaking
- **Breaking: the web dashboard is off by default outside Docker.** The server binary no longer serves it: `GET /` returns 404 and only the REST API, `/mcp` and `/metrics` are served. Pass `-dashboard` or set `MONGORESCUE_DASHBOARD=true` to serve it. The container image sets `MONGORESCUE_DASHBOARD=true`, so Docker and Compose deployments are unchanged; installs from the release binaries should switch to the desktop app or add the flag.

### Added
- Desktop app (`cmd/mongorescue-desktop`, Wails v2): the dashboard in a native window on Windows (NSIS installer), macOS (`.app`) and Linux, with the REST API served in-process to the webview and no TCP listener. Data lives in the per-user configuration directory, the setup code is shown in a banner in the window (and copied to the clipboard), and only one instance runs at a time. Built with `make desktop` / `make desktop-windows` (CGO, `desktop` build tag) and attached to every release. See [docs/desktop.md](docs/desktop.md).
- `-dashboard` flag and `MONGORESCUE_DASHBOARD` environment variable.
- `App.Handler`, `App.Start` and `App.Stop` in `internal/app` run the application without an HTTP listener.

## [0.2.0] - 2026-09-25

### Added
- MCP server for AI assistants ([docs/mcp.md](docs/mcp.md)), built on the official MCP Go SDK: Streamable HTTP at `/mcp` on the main listener (API keys only, stateless, JSON responses) and `mongorescue mcp`, a stdio bridge that forwards to a running instance with `MONGORESCUE_MCP_API_KEY`. Tools `list_connections`, `list_databases`, `list_collections`, `list_jobs`, `get_job`, `list_backups`, `get_backup`, `list_restores`, `get_restore`, `list_storage_targets`, `get_status` (read) and `start_backup`, `run_job`, `restore_to_safe_clone` (operator), with MCP tool annotations; resources `mongorescue://status`, `mongorescue://backups/{id}`, `mongorescue://jobs/{id}`, `mongorescue://restores/{id}`; prompts `diagnose_failed_backup`, `disaster_recovery_plan`, `verify_recent_backups`. Settings → Security → *MCP server for AI assistants* (`security.mcp_enabled`, on by default) switches it off.
- API key scopes: `read` (GET-only REST and read-only MCP tools), `operator` (plus backups, job runs and safe-clone restores) and `admin` (everything), enforced centrally from one route → scope table; chosen when creating a key (default `read`) and shown in the key list.
- Audit log of every MCP tool call (time, API key, transport, tool, redacted arguments, result, duration) in the new `audit_log` table, shown under Settings → Security → *Recent API/MCP activity* and served by `GET /api/v1/audit` (admin).
- Per-API-key rate limit for MCP (60 calls per minute, bursts of 20) and the `mongorescue_mcp_calls_total{tool,result}` metric.

### Changed
- API keys created without a `scope` are read-only; existing keys and the key imported from `MONGORESCUE_API_KEY` become `admin` keys (migration `0004`), so current automation keeps working. In-place restores need an admin key or a session.
- The backup, job-run and restore use cases moved from the HTTP handlers into `internal/operations`, shared by the REST API and the MCP server; unexpected errors of these endpoints answer `internal error` instead of the raw error text.
- Building from source now requires Go 1.26 or newer; Go 1.25 is no longer supported upstream.
- Updated golang.org/x dependencies; the container image is built with Go 1.27.

### Security
- The MCP endpoint never accepts session cookies, refuses foreign `Origin` headers and non-local `Host` names on a loopback listener (DNS rebinding) unless proxy headers are trusted, exposes no tool that deletes, restores in place or changes configuration, filters and re-checks tools by scope, and never returns connection strings or storage credentials.
- Retention can no longer be abused to delete good backups: it runs only after scheduled (cron) runs and only on the job's own scheduled backups. Backup records gain a `trigger` (`scheduled`, `on_demand`, `manual`, `mcp`; migration `0008` backfills existing records); on-demand, manual and MCP backups are never pruned automatically, the newest `retention_count` scheduled backups (at least one) are always kept, and count-based retention never deletes a backup less than 24 hours old. The dashboard shows each backup's trigger.
- MCP checks the per-key rate limit before the scope, and repeated denied, rate-limited and REST read calls are coalesced into one audit entry per 10 seconds with a `count` and their distinct IDs (up to 20), so no client can flush the audit log (migrations `0006`, `0007`).
- The audit log also records MCP resource reads and prompt requests, and every REST request made with an API key (route pattern, path parameters, status, key).
- The stdio bridge never follows redirects and sends the API key only to the configured scheme and host.
- MCP tool text summaries carry only IDs, counts, statuses and timestamps; names and error messages stay in the structured result (prompt-injection hardening).
- `GET /api/v1/users` needs admin, and a restore into another connection than the backup's needs admin (REST and MCP).
- Values that clients can influence are stripped of control characters before they are logged.

### Fixed
- A job's `last_run` is stored before the run's final backup record, so a client that sees the run finished also sees the job updated.

## [0.1.0] - 2026-09-25

Requires Go 1.25+ to build.

### Added
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

### Changed
- Only bootstrap options remain outside the database: `-data-dir`/`MONGORESCUE_DATA_DIR`, `-host`/`-port` (`MONGORESCUE_SERVER_HOST`/`_PORT`), `-log-level`, the optional `MONGORESCUE_SECRET_KEY` and `-version`.
- The API always requires a session or an API key; the unauthenticated mode is gone.
- Jobs and manual backups require `connection_id`; `mongo_uri` is no longer accepted in jobs, backups or restores.
- `docker-compose.yml` runs only MongoRescue and has no `environment` block.
- Concurrency keys and retention are per connection, so the same database name on two servers is independent.
- The MongoDB Go driver is a production dependency, confined to `internal/mongoconn`.
- `GET /api/v1/auth/me` answers signed-out visitors with `200` and `{user: null, csrf_token: "", auth: ""}`; restore records carry `source_connection_id` and `source_connection_name` next to the target connection fields.

### Removed
- The JSON configuration file (`-config`), `config.example.json`, `.env.example`, the flags `-api-key`, `-mongo-uri`, `-storage` and `-gen-age-key` (the dashboard generates keys), `database_path`/`MONGORESCUE_DB_PATH` (the database is always `<data_dir>/mongorescue.db`), `GET /api/v1/config` and every other environment variable (static API key, MongoDB URI, storage, encryption, restore, timeouts, CORS, metrics, cookies, proxy headers).
