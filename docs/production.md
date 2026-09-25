# Production deployment

MongoRescue holds credentials for your databases and your backup storage, and can overwrite databases on request. Treat the instance like a database administrator account.

## Checklist

- [ ] TLS terminated by a reverse proxy in front of MongoRescue, with **Settings → Security → Trust proxy headers** on (or *Secure cookies: always*) so session cookies are marked `Secure`.
- [ ] Setup completed right after the first start (until then, anyone who can reach the port and read the logs can claim the instance).
- [ ] The HTTP port reachable only from the proxy and your monitoring, not from the internet.
- [ ] One user per person; API keys (not user passwords) for automation, one per consumer.
- [ ] Backups encrypted with X25519 recipients; private key stored off the backup host.
- [ ] `mongorescue.db` backed up consistently (see [Data directory](#data-directory)) and `secret.key` (or `MONGORESCUE_SECRET_KEY`) stored somewhere safe, separately.
- [ ] Container image pinned to a release tag.
- [ ] An alert on stale backups ([metrics.md](metrics.md#alerting)) and a `backup.failed` notification rule ([notifications.md](notifications.md)).

## TLS and the reverse proxy

MongoRescue serves plain HTTP. Put it behind a proxy that terminates TLS, and make the MongoRescue port reachable only from the proxy: publish it on loopback (`-p 127.0.0.1:8080:8080`), or keep it on a private Docker network that only the proxy joins.

Two optional settings (Settings → Security) make the proxy setup complete. With *Trust proxy headers* on, MongoRescue takes the client address for login throttling from the last `X-Forwarded-For` entry and marks the session cookie `Secure` when `X-Forwarded-Proto` is `https`. Enable it only when a proxy sets these headers: otherwise clients could spoof them. If your proxy does not send `X-Forwarded-Proto`, set *Secure cookies* to `always` instead.

Caddy example:

```
backup.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Restores and large backup listings can run for a long time; raise the proxy's upstream read timeout accordingly.

## Authentication

Every API call needs a session or an API key; there is no unauthenticated mode. On a fresh data directory MongoRescue starts in **setup mode** and logs, at `WARN`, a one-time setup code (`Setup required: open http://.../ and enter setup code XXXX-...`). The code lives only in memory, changes on every restart and stops working once the first user exists. Complete setup immediately after deployment.

- **Users** sign in to the dashboard. Passwords are hashed with bcrypt and must be 12 to 72 bytes long. Every user is an administrator in this release.
- **Sessions** use an `HttpOnly`, `SameSite=Strict` cookie and a CSRF token that the dashboard sends with every change. They expire after 12 hours of inactivity or 7 days after login, and are revoked on logout, when the user's password changes (all other sessions) or when the user is deleted.
- **Login throttling**: 5 failures for the same client address and username lock that pair out for 30 seconds, doubling up to 15 minutes. After 20 failures from one address, each further username from it is locked after one failure; a correct password for an unlocked user is never refused, so users behind one proxy address cannot be locked out by someone else. Attempts are reserved before the password check (one at a time per address and username), so parallel requests cannot exceed the budget. The lockouts live in memory.
- **API keys** are created in Settings → API keys, shown once, and stored only as SHA-256 hashes; each records when it was last used. Send them as `Authorization: Bearer <key>` or `X-API-Key: <key>`. A `MONGORESCUE_API_KEY` from an earlier build is imported once as a key named *Imported from MONGORESCUE_API_KEY*; remove the variable afterwards.

The channel **test** endpoint and the connection test make outbound requests to whatever destination they name, so only give accounts and keys to people and systems you trust with network access from the MongoRescue host.

## Data directory

`MONGORESCUE_DATA_DIR` holds:

| File | Contents |
| :--- | :--- |
| `mongorescue.db` (+ `-wal`, `-shm`) | SQLite metadata database: jobs, backup and restore history, users, sessions, API key hashes, connections and notification settings. Mode `0600`. |
| `secret.key` | The key encrypting connection strings and notification channel secrets inside the database (unless `MONGORESCUE_SECRET_KEY` is set). Mode `0600`. |
| `mongorescue.lock` | Advisory lock that stops a second instance from using the same directory. |

Connection strings and channel secrets are encrypted with AES-256-GCM. **Losing the key loses them**: MongoRescue refuses to start when the key does not match the database (`the secret key does not match the key this database was encrypted with`). Store a copy of `secret.key`, or set `MONGORESCUE_SECRET_KEY` from your secret manager, and keep it apart from database backups so one leaked backup does not reveal both.

Back up the database consistently. It runs in WAL mode, so copying `mongorescue.db` alone while the server is running can miss recent changes or produce a torn copy. Use one of:

- stop MongoRescue (or the container), copy the data directory, start it again;
- take an online copy with the SQLite CLI: `sqlite3 /data/mongorescue.db ".backup '/backup/mongorescue-$(date +%F).db'"` (or `VACUUM INTO '/backup/mongorescue.db'`), which is safe while the server runs;
- snapshot the whole data volume atomically (LVM, ZFS, cloud disk snapshot), including the `-wal` file.

Losing the database does not lose the backup archives, but it loses the records that point to them, your schedules, users and connections.

Releases before the SQLite store kept metadata in `state.json`; it is imported automatically on the first start and renamed to `state.json.migrated-<timestamp>` (see [configuration.md](configuration.md#json-file)). Job connection strings from those releases become managed connections.

## Encryption

Use X25519 recipients (Settings → Encryption) rather than a passphrase: the instance taking backups then needs only the public key, and a compromise of that host or its storage credentials does not expose backup contents. Keep the private key on the host (or in the secret store) used for restores. See [encryption.md](encryption.md).

For restores into existing databases, keep the *Verify before restore* policy (Settings → General) at `auto` or `always` so a corrupted or undecryptable artifact is rejected before `mongorestore` touches the target.

## Container images

Images are published to `ghcr.io/yigitcittan/mongorescue` for `linux/amd64` and `linux/arm64`. Tags:

| Tag | Meaning |
| :--- | :--- |
| `0.1.0` | Exact release (recommended) |
| `0.1` | Latest patch of a minor release |
| `latest` | Current `main` branch; changes without notice |
| `sha-<commit>` | A specific commit |

Mount `/data` and `/backups` as volumes; the image already uses them as the data directory and the local backup directory, so no environment variable is needed.

The container runs as the unprivileged user `mongorescue` (UID and GID `10001`). Named Docker volumes inherit the ownership of the image directories, so they work as-is. Bind mounts keep the ownership of the host directory and must be writable by UID 10001, for example:

```bash
sudo install -d -o 10001 -g 10001 -m 750 /srv/mongorescue/data /srv/mongorescue/backups
docker run -v /srv/mongorescue/data:/data -v /srv/mongorescue/backups:/backups ghcr.io/yigitcittan/mongorescue:0.1.0
```

The image defines a `HEALTHCHECK` that calls `GET /api/v1/health` inside the container. Change the listening port with `MONGORESCUE_SERVER_PORT` rather than the `-port` flag, so the server and the health check use the same port.

## Docker Compose

The repository's `docker-compose.yml` is hardened by default. Keep these properties when you adapt it:

- **No required configuration.** The file needs no `.env`; every variable in `.env.example` is optional. Keep an `.env` you do create (for example with `MONGORESCUE_SECRET_KEY`) out of version control with mode `600`.
- **Port.** The dashboard is published as `${MONGORESCUE_PORT:-8080}:8080` on all interfaces so it works out of the box. Before exposing the host, either change the mapping to `127.0.0.1:8080:8080` and put a TLS-terminating proxy on the host (the Caddy example above works unchanged), or put the proxy in the same Compose project, let it reach `mongorescue:8080` over the project network and remove the `ports` entry entirely.
- **Pinned image.** `MONGORESCUE_VERSION` selects the image tag. Set it to an exact release such as `0.1.0` so a `docker compose pull` does not change the version unexpectedly.
- **Bind mounts.** The file uses named volumes, which get the right ownership automatically. If you replace them with host directories, create them owned by UID/GID `10001` first (see [Container images](#container-images)).
- **Locked-down container.** The service runs with `read_only: true`, a `tmpfs` on `/tmp` (where `mongodump` and `mongorestore` receive their temporary `--config` file), `cap_drop: [ALL]` and `no-new-privileges`. The only writable paths are `/data`, `/backups` and `/tmp`.
- **Back up the data volume.** The data volume (`/data`) holds the metadata database and `secret.key`. Back it up as described in [Data directory](#data-directory), for example with `docker compose stop mongorescue` before copying the volume.

To attach MongoRescue to a Compose project that already runs MongoDB, start from [examples/compose-existing-stack.yml](../examples/compose-existing-stack.yml).

## systemd

`scripts/mongorescue.service` runs the binary as a daemon with `-data-dir=/var/lib/mongorescue/data`; the default "Local disk" storage target is then `/var/lib/mongorescue/backups`. Everything else is configured in the dashboard. To supply `MONGORESCUE_SECRET_KEY` from a file, use an `EnvironmentFile=` readable only by the service user. The shipped unit runs as `root`; consider a dedicated user that owns the data and backup directories.
