# Configuration

Everything is configured in the dashboard and stored in MongoRescue's database (`mongorescue.db` in the data directory): storage targets, backup encryption, security options, limits, MongoDB connections, jobs, notifications, users and API keys. There is no configuration file and nothing to set before the first start.

- [Bootstrap options](#bootstrap-options)
- [Settings](#settings) · [General](#general) · [Security](#security) · [Encryption](#encryption)
- [Storage targets](#storage-targets)
- [Upgrading: deprecated environment variables and config.json](#upgrading-deprecated-environment-variables-and-configjson)

## Bootstrap options

Only what MongoRescue needs to find its database and serve the dashboard is read from flags or the environment. Flags take precedence over environment variables.

| Flag | Environment variable | Default | Description |
| :--- | :--- | :--- | :--- |
| `-data-dir` | `MONGORESCUE_DATA_DIR` | `./data` (`/data` in the image) | Holds `mongorescue.db`, `secret.key` and the instance lock |
| `-host` | `MONGORESCUE_SERVER_HOST` | `0.0.0.0` | Listen address |
| `-port` | `MONGORESCUE_SERVER_PORT` | `8080` | Listen port |
| `-log-level` | | `info` | `debug`, `info`, `warn` or `error` |
| | `MONGORESCUE_SECRET_KEY` | generated | Base64 32-byte key encrypting stored credentials (`openssl rand -base64 32`), instead of the generated `<data_dir>/secret.key` |
| `-version` | | | Print the version and exit |

`mongorescue mcp`, the stdio bridge for AI assistants, reads only `--url`/`MONGORESCUE_MCP_URL` and `MONGORESCUE_MCP_API_KEY` and never opens the data directory; see [mcp.md](mcp.md#transports).

The metadata database is always `<data_dir>/mongorescue.db`. The key (from `secret.key` or `MONGORESCUE_SECRET_KEY`) encrypts connection strings, notification secrets, storage credentials and encryption keys in the database. Losing it makes them unrecoverable, and MongoRescue refuses to start with a key that does not match the database rather than silently discard them. Keep a copy apart from database backups; see [production.md](production.md#data-directory).

## Settings

In v0.1.0 every signed-in user and every API key is a full administrator: they can change settings and storage targets, and the test endpoints make outbound connections to the hosts they name. Grant access accordingly; roles (RBAC) are on the roadmap.

**Settings** in the dashboard has the sections General, Storage, Encryption, Security, Users and API keys. Changes apply to the next backup, restore or request without a restart. The same settings are available through `GET` and `PUT /api/v1/settings` ([API](api.md#settings)); durations are Go duration strings such as `90m` or `6h` (`0s` disables a limit), and secrets come back as `******`.

### General

| Setting | Default | Description |
| :--- | :--- | :--- |
| `default_retention_days` | `30` | Pre-filled for new jobs; `0` keeps backups forever |
| `default_retention_count` | `10` | Pre-filled for new jobs; `0` keeps any number |
| `default_gzip` | `true` | Compress new jobs and manual backups that do not say otherwise |
| `backup_timeout` | `6h` | Maximum duration of one backup (`0s` = unlimited) |
| `backup_stall_timeout` | `10m` | Abort a backup when `mongodump` produces no output for this long (`0s` = off) |
| `restore_timeout` | `12h` | Maximum duration of one restore, verification included (`0s` = unlimited) |
| `restore_verify_policy` | `auto` | `always`, `auto` (in-place restores only) or `never`; see [encryption.md](encryption.md#verify-before-restore) |

Retention is applied only after a successful **scheduled** (cron) run of a job. Running a job on demand (`POST /api/v1/jobs/{id}/run`, the MCP `run_job` tool) and manual backups never prune. Two floors protect good backups: the newest `retention_count` completed backups (at least one) are always kept, and count-based retention never deletes a backup less than 24 hours old.

### Security

| Setting | Default | Description |
| :--- | :--- | :--- |
| `session_idle_timeout` | `12h` | End a session after this long without requests (1m to 8760h) |
| `session_absolute_timeout` | `168h` | End a session this long after login; not shorter than the idle timeout. Shortening it also ends existing older sessions |
| `secure_cookies` | `auto` | `auto`: `Secure` over TLS or behind a trusted proxy reporting `X-Forwarded-Proto: https`; `always`: for a TLS proxy that does not send it; `never`: plain-HTTP tests only |
| `trust_proxy_headers` | `false` | Honour `X-Forwarded-For` / `X-Real-IP` (login throttling) and `X-Forwarded-Proto`. Enable only behind a proxy that sets them |
| `cors_origins` | empty | Exact origins (`https://ops.example.com`) allowed to call the API cross-origin; no wildcards. Empty disables CORS |
| `metrics_public` | `false` | Serve `/metrics` without an API key |
| `mcp_enabled` | `true` | Serve the [MCP endpoint](mcp.md) `/mcp` for AI assistants (API keys only). When off it answers `403` and the stdio bridge cannot connect |

### Encryption

Backups can be encrypted with [age](https://age-encryption.org) before they leave the host; see [encryption.md](encryption.md).

| Setting | Description |
| :--- | :--- |
| `enabled` | Encrypt new backups. Turning it off never affects existing backups |
| `mode` | `x25519` (public keys, recommended) or `passphrase` (scrypt) |
| `recipients` | Public keys (`age1…`) new backups are encrypted to; at least one is required in `x25519` mode |
| `identity` | Private key(s) (`AGE-SECRET-KEY-1…`) used by restores. Optional: an instance with recipients only can back up but not restore |
| `passphrase` | Passphrase for `passphrase` mode (at least 16 characters); also used to restore scrypt backups |
| `retired_keys` | Read-only. Replaced or removed identities and passphrases, kept encrypted so that older backups stay restorable |

**Generate key pair** (`POST /api/v1/settings/encryption/generate-key`) creates an X25519 key pair without storing it. The private key is shown once: copy or download it and keep it somewhere safe, apart from the backups. **Losing every copy of the private key makes the backups encrypted to it unrecoverable.** Choosing *Use this key* stores the private key (encrypted) and adds its public key to the recipients; the previous identity is retired, not deleted.

## Storage targets

<img src="assets/screenshots/storage.png" alt="Storage targets in Settings" width="800">

A storage target is where backup archives are written: a directory on the MongoRescue host (or a mounted volume) or an S3-compatible bucket (AWS S3, Cloudflare R2, Backblaze B2, MinIO, DigitalOcean Spaces, Wasabi). On the first start MongoRescue creates **Local disk**, a local target at `backups` next to the data directory (`/backups` in the image, `./backups` for the binary with the default data directory), and makes it the default.

- **Default target.** Jobs and manual backups that name no `storage_target_id` use the default target. The default is changed with *Set default*; it cannot be deleted.
- **Backups stay with their target.** Every backup record stores the target it was written to (`storage_target_id` and a name snapshot). Restores, deletions and retention always use that target, never the current default. Retention counts per target: backups a job left on a previous target are not pruned by runs on the new one.
- **Deleting a target** is refused (`409`) while a job uses it or a completed or running backup is stored on it, so every restorable backup keeps its target. Reassign the jobs and delete or let retention remove those backups first.
- **The location is fixed once backups are stored.** While completed or running backups are on a target, its type, local path, S3 endpoint, bucket and prefix cannot change (`409`): the records would point to a place that does not hold them. Create a new target instead and move the jobs to it; the name, credentials, region and path style can always be changed.
- **Concurrent edits** are detected: an update of a target that was changed or deleted meanwhile answers `409` or `404` instead of overwriting or recreating it.
- **Test** writes, reads back and deletes a small object named `.mongorescue-probe-<random>` in the target's root (or prefix); it creates no directory. Testing an unsaved local target whose directory does not exist reports that instead of creating it; saving the target creates the directory. The dashboard tests before saving.

| Field | Description |
| :--- | :--- |
| `name` | Display name |
| `type` | `local` or `s3` |
| `local.path` | Absolute path. `..`, the file system root, the data directory and anything inside it (symbolic links resolved) are refused; the directory is created on save and must be writable |
| `s3.endpoint` | S3 API URL; empty for AWS S3 (e.g. `https://<account>.r2.cloudflarestorage.com`, `https://s3.<region>.backblazeb2.com`, `http://minio:9000`) |
| `s3.region` | Bucket region (`auto` for R2; default `us-east-1`) |
| `s3.bucket` | Bucket name (must exist) |
| `s3.prefix` | Optional key prefix, e.g. `mongorescue/` |
| `s3.access_key_id`, `s3.secret_access_key` | Static credentials. Leave both empty to use the AWS default credential chain (instance role, `AWS_*` variables of the container). The secret key is encrypted in the database and shown as `******`; sending `******` back keeps it only while endpoint, bucket and access key are unchanged |
| `s3.use_path_style` | Path-style URLs (required by MinIO and some gateways) |

Each target gets its own driver, built on first use and rebuilt after the target changes.

## Upgrading: deprecated environment variables and config.json

Earlier builds read every setting from environment variables or a JSON file. Those are no longer used; the flags `-config`, `-api-key`, `-mongo-uri`, `-storage` and `-gen-age-key` are gone (the dashboard generates encryption keys).

To keep existing deployments working, MongoRescue imports them **once**: when one of the variables below, or a legacy file at `<data_dir>/config.json`, is present at startup and the database has no value for that setting yet, the value is imported into the database and a warning asks you to remove the variable. Every source is recorded as imported, so later starts ignore it (with a warning) even if the setting is changed in the dashboard afterwards. Values that cannot be interpreted are skipped with a warning.

| Deprecated source | Imported as |
| :--- | :--- |
| `MONGORESCUE_STORAGE_TYPE`, `MONGORESCUE_LOCAL_PATH` (a relative path is resolved against the working directory at import time and stored absolute), `AWS_S3_BUCKET`, `AWS_S3_ENDPOINT`, `AWS_S3_USE_PATH_STYLE` (with `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` for S3) | The default storage target, when no target exists yet |
| `MONGORESCUE_ENCRYPTION_ENABLED`, `_RECIPIENTS`, `_IDENTITY`, `_IDENTITY_FILE` (file content), `_PASSPHRASE` | Encryption settings (short passphrases from earlier builds are accepted) |
| `MONGORESCUE_BACKUP_TIMEOUT`, `MONGORESCUE_BACKUP_STALL_TIMEOUT`, `MONGORESCUE_RESTORE_TIMEOUT`, `MONGORESCUE_RESTORE_VERIFY` | General settings |
| `MONGORESCUE_TRUST_PROXY_HEADERS`, `MONGORESCUE_SECURE_COOKIES` (`true` → `always`), `MONGORESCUE_CORS_ORIGINS`, `MONGORESCUE_METRICS_PUBLIC` | Security settings |
| `MONGORESCUE_MONGO_URI` | A connection named `default` (when there is none), also assigned to legacy jobs without a connection string |
| `MONGORESCUE_API_KEY` | An API key named *Imported from MONGORESCUE_API_KEY* (stored as a hash; revoke it in the dashboard) |
| `<data_dir>/config.json` | The same settings from the former JSON keys (`server.*`, `storage.*`, `defaults.*`, `backup.*`, `restore.*`, `encryption.*`) |

`MONGORESCUE_DB_PATH` (and `database_path`) is not imported: if it points to an existing database outside the data directory, MongoRescue refuses to start and asks you to move that file (with its `-wal` and `-shm` files) to `<data_dir>/mongorescue.db`. A `secret_key` in a legacy `config.json` is still used when `MONGORESCUE_SECRET_KEY` is not set, with a warning to move it there.

Releases before the SQLite store kept metadata in `<data_dir>/state.json`. On the first start with an empty database it is imported automatically in one transaction and renamed to `state.json.migrated-<timestamp>`; keep that file until you have checked the imported data. If the import fails, startup stops and `state.json` is left untouched. Job connection strings (`mongo_uri`) from those releases become managed connections, one per distinct URI and named after its hosts. Existing jobs and backup records without a storage target are assigned to the default target.
