# Production deployment

MongoRescue holds credentials for your databases and your backup storage, and can overwrite databases on request. Treat the instance like a database administrator account.

## Checklist

- [ ] TLS terminated by a reverse proxy in front of MongoRescue, with **Settings → Security → Trusted proxies** set to the proxy's address (or *Secure cookies: always*) so session cookies are marked `Secure`.
- [ ] Setup completed right after the first start (until then, anyone who can reach the port and read the logs can claim the instance).
- [ ] The HTTP port reachable only from the proxy and your monitoring, not from the internet.
- [ ] One user per person; API keys (not user passwords) for automation, one per consumer, with the smallest scope that works (`read` for dashboards and assistants that only report, `operator` for backup automation).
- [ ] The [MCP endpoint](mcp.md) switched off under Settings → Security if no AI assistant uses it.
- [ ] The [outbound connections](#outbound-connections) limited to the destinations in the table (network policy or firewall), with **Deny private network destinations** and **Require https for S3 endpoints** on when your S3, webhooks and SSO are public and use TLS, and the CA certificate of a private CA set on the target instead of `http://`.
- [ ] Backups encrypted with X25519 recipients; private key stored off the backup host.
- [ ] [Metadata backups](#metadata-backups) on (Settings → Recovery), encrypted, to a target other than the one the instance runs on, with a `metadata_backup.failed` notification rule.
- [ ] A [recovery kit](#recovery-kit) downloaded and stored offline, apart from the backups (it holds `secret.key`); a new one downloaded whenever the dashboard asks for it.
- [ ] Container image pinned to a release tag, and the [upgrade guide](upgrading.md) read before every feature release: back up first, upgrade standbys before the active instance, know how to [roll back](upgrading.md#rolling-back).
- [ ] An alert on stale backups ([metrics.md](metrics.md#alerting)) and a `backup.failed` notification rule ([notifications.md](notifications.md)).
- [ ] An [RPO](#rpo-and-rto) set on every job whose default does not match what you promised, a `job.rpo_missed` notification rule, and no `fail` row under Overview → *Recovery readiness*.
- [ ] Busy replica sets backed up from a secondary, with an upload cap and a backup window where the dump competes with production traffic; databases past the [practical limits of mongodump](#large-databases) covered by snapshots or your cloud provider's backups instead.

## Production checklist

The checklist above secures the instance. This one makes sure you can still restore after losing a database, a bucket, a cloud account or MongoRescue itself. Review it at every minor release, together with the README's [known limitations](https://github.com/YigitCittan/mongorescue/blob/main/README.md#known-limitations).

- [ ] **Object Lock in compliance mode, with versioning**, on every S3 target that holds backups ([Object Lock](configuration.md#immutable-backups-s3-object-lock), [threat model](security.md#immutable-backups-s3-object-lock)). Why: without it, anyone holding the bucket's credentials can delete every archive, and governance mode can be bypassed.
- [ ] **A separate IAM account and credentials for copy targets** ([least-privilege credentials](#least-privilege-storage-credentials), [copies](configuration.md#copies-on-a-second-target-3-2-1)). Why: one leaked key, or one compromised account, must not reach the primary and its copies at once.
- [ ] **A cross-region copy** of every important job, on its own locked target ([3-2-1 backups](#3-2-1-backups)). Why: a region outage or a lost account takes the primary target with it; restores then fall back to a healthy copy by themselves.
- [ ] **Locked copies required** with **Settings → Security → Require locked copies** (`security.require_locked_copies`) and on every job with copies ([cross-region disaster recovery](#cross-region-disaster-recovery)). Why: a copy target without Object Lock leaves the copy deletable by whoever holds its key, and the policy refuses such a job instead of letting it slip.
- [ ] **A DR drill from the cross-region copy** on every important job, passing at least monthly, and the [region failure runbook](#region-failure) walked through once with the recovery kit stored outside the primary's region ([DR drills](#dr-drills)). Why: a copy you never restored from is a hope, not a plan; the drill proves it restores without region A.
- [ ] **No `dr_same_region` or `dr_same_credentials` warning** under Overview → *Recovery readiness*, and a region set on every storage target ([region awareness](#region-awareness)). Why: a copy in the primary's region, or reachable with the primary's key, fails together with it.
- [ ] **A heartbeat and the alert rules** ([heartbeats](monitoring.md#global-heartbeat), [Prometheus alert rules](monitoring.md#prometheus-alert-rules)). Why: a crashed instance or a hung scheduler cannot report its own failure.
- [ ] **`runbook_url` in your alert notifications, a memory limit sized for the busiest run, and the SLOs agreed** ([incident runbooks](runbooks.md), [sizing](#sizing), [service level objectives](#service-level-objectives), [Grafana dashboard](monitoring.md#grafana-dashboard)). Why: an OOMKill shows up as a failed backup, and the person paged at night needs the diagnosis steps on the alert itself.
- [ ] **The recovery kit downloaded and stored offline**, apart from the backups, with metadata backups on ([recovery kit](#recovery-kit), [metadata backups](#metadata-backups)). Why: without `secret.key` the stored credentials and encryption keys are unrecoverable.
- [ ] **`secret.key` wrapped by a KMS** where your platform has one (`MONGORESCUE_KEY_WRAP`, [key wrapping](security.md#wrapping-secretkey-with-a-kms)), with deletion protection on the KMS key, an instance identity that may only encrypt and decrypt with it, and the KMS decrypt calls audited. Why: a copied volume or snapshot then holds no root key, and losing the data directory no longer loses it; deleting the KMS key, though, loses every stored credential.
- [ ] **Credentials from Vault** where you run HashiCorp Vault (`MONGORESCUE_VAULT_ADDR`, [credentials from Vault](security.md#credentials-from-hashicorp-vault)): connections with a dynamic user of the database secrets engine (`max_ttl` above your longest run) or a KV v2 secret, S3 targets with keys of the AWS engine (`assumed_role`) or a KV v2 secret, a Vault policy limited to MongoRescue's paths, and the `vault.unavailable` notification reaching someone. Why: `mongorescue.db` then holds no database or bucket credential at all, dynamic users exist only while runs use them, and rotation happens in Vault; but every run that needs them fails while Vault is down.
- [ ] **The two-person rule on, with at least two administrators** ([two-person rule](security.md#the-two-person-rule)). Why: one stolen administrator credential then cannot delete backups, lower protections or make another administrator alone; the rule cannot be turned on with fewer than two.
- [ ] **Read preference `secondary` (or `secondaryPreferred`), an upload cap and a backup window** on busy replica sets ([reading from a secondary](#reading-from-a-secondary), [throttling](#throttling), [backup windows](#backup-windows)). Why: a dump from the primary competes with production traffic and evicts its working set.
- [ ] **Scheduled restore tests, and chain tests for PITR streams** ([restore tests](verification.md#automated-restore-tests), [chain tests](pitr.md#chain-tests)). Why: a checksum proves the archive is intact, only a restore proves it is usable, and its duration is your measured RTO.
- [ ] **TLS to MongoDB**, with certificate and hostname verification on ([TLS settings](connections.md#tls-settings)). Why: the dump carries all your data and the connection carries its credentials.
- [ ] **Per-connection access** for every user and API key that is not an administrator, and few administrators ([per-connection access](security.md#per-connection-access), [roles](design/roles.md#roles-and-scopes)). Why: one team's credential should not read or restore another team's databases.
- [ ] **A key rotation runbook** you have walked through once: `secret.key`, the backup encryption key and the storage credentials ([key rotation](#key-rotation), [runbook](encryption.md#key-rotation-runbook)). Why: after a leak or a departure you rotate under pressure, and every rotation needs a new recovery kit.
- [ ] **MongoRescue in another failure domain than the databases**: another cloud account, region or cluster, with its metadata backups on a target outside the instance's own ([metadata backups](#metadata-backups), [failure domains](#failure-domains), [Kubernetes](kubernetes.md#what-the-chart-runs)). Why: an incident that takes the databases must not take the tool that restores them.
- [ ] **A standby instance** where missed backups or a gap in PITR collection would breach your RPO, with the `MongoRescueHANoActiveInstance` alert loaded ([high availability](#high-availability)). Why: a single instance that dies stops every schedule until someone restarts it.

## TLS and the reverse proxy

MongoRescue serves plain HTTP. Put it behind a proxy that terminates TLS, and make the MongoRescue port reachable only from the proxy: publish it on loopback (`-p 127.0.0.1:8080:8080`), or keep it on a private Docker network that only the proxy joins.

Set **Trusted proxies** (`security.trusted_proxies`, Settings → Security) to the address or CIDR of your reverse proxy, for example `10.0.0.0/8` or `172.18.0.0/16` for a Docker network. MongoRescue then reads forwarding headers only from connections whose peer address is in that list; a request that reaches the port by another route (a pod IP, a published Docker port) cannot choose its own address by sending `X-Forwarded-For`. For a trusted peer:

- **Client address** (login, setup and single sign-on throttling, the anonymous rate limit, the audit log). `X-Forwarded-For` wins: all header lines are joined and read right to left, skipping addresses in the trusted list, and the first other address is the client. Only when there is no `X-Forwarded-For` is `X-Real-IP` used. A value that is not an IP address discards the headers and the peer's address is used.
- **Scheme.** The session cookie is marked `Secure` when the (rightmost) `X-Forwarded-Proto` is `https`. If your proxy does not send it, set *Secure cookies* to `always` instead.
- **MCP.** The loopback Host check of the [MCP endpoint](mcp.md) is skipped for trusted peers. A proxy on the same host connects from `127.0.0.1`, so list it (`127.0.0.1/32`) for a different `Host`.

Your proxy must overwrite what the client sent, or the address is still the client's choice. nginx with `X-Real-IP` only: `proxy_set_header X-Real-IP $remote_addr; proxy_set_header X-Forwarded-For $remote_addr;` (the second line discards a client-supplied chain; `$proxy_add_x_forwarded_for` appends to it, which is fine too). Proxies that append to `X-Forwarded-For` (nginx `$proxy_add_x_forwarded_for`, HAProxy `option forwardfor`, Traefik, Caddy) need nothing more.

IPv6 clients are throttled by their `/64`: rotating addresses inside one prefix does not reset the failed-login count, the setup-code counter, the single sign-on attempt budget or the anonymous rate limit.

**Migrating from *Trust proxy headers*.** `security.trust_proxy_headers` (and `MONGORESCUE_TRUST_PROXY_HEADERS`) still works: on its own, with no trusted proxies listed, it trusts forwarding headers from every peer as before, and MongoRescue logs a warning at startup. Set `trusted_proxies` to switch to the safe behaviour; the checkbox may then stay on or be turned off, as a non-empty list alone enables header trust for its peers. To disable header trust entirely, clear the list and turn the checkbox off.

**A reverse proxy requires trusted proxies.** Without them every request comes from the proxy's address, so all clients share one [rate limit budget](security.md#rest-api-rate-limits) for sign-ins and unauthenticated requests, and one login throttle: a few bad clients, or one script with a wrong key, lock everybody out. MongoRescue logs a warning once when it sees forwarding headers and no proxy is trusted, and another once when a peer outside the list sends them (a client spoofing, or a proxy missing from the list).

Caddy example:

```
backup.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Restores run in the background (`POST /api/v1/restore` answers `202`; poll `GET /api/v1/restores`), so ordinary requests are short and no unusual proxy read timeout is needed; leave the proxy default (at least 60 s) in place.

**Idle timeout.** MongoRescue closes an idle keep-alive connection after 120 seconds. Keep the idle timeout of every proxy and load balancer in front of it **lower** (AWS ALB defaults to 60 s, nginx upstream `keepalive_timeout` to 60 s: set it to 55s or less, or close idle upstream connections sooner), or the proxy reuses a connection the server has just closed and answers an intermittent `502`.

**HSTS.** Over TLS, directly or behind a trusted proxy that sends `X-Forwarded-Proto: https`, MongoRescue can add `Strict-Transport-Security`: it is off by default; set `security.hsts_max_age` (for example `31536000`, one year) once HTTPS works for every client. Add the proxy to **Trusted proxies** for it to apply, since the server itself speaks plain HTTP.

**Request bodies.** The JSON bodies of the job, backup and restore endpoints are limited to 1 MiB, others to 64 KiB or less, and `/mcp` to 4 MiB; larger requests get `413`.

## Outbound connections

MongoRescue connects out to the systems you configure. This table is the whole list, for firewall and egress rules.

| Destination | Port | Protocol | Proxy (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`) | Destination policy |
| :--- | :--- | :--- | :--- | :--- |
| MongoDB servers of your connections (`mongodump`, `mongorestore`, the driver; `mongodb+srv` also looks up DNS SRV and TXT records) | 27017 or as in the URI | MongoDB wire protocol, TLS as configured | Not supported: direct TCP | None: the address is yours to choose; limit it with the network policy below |
| S3-compatible storage of targets (backups, probes, downloads, PITR chunks, metadata backups) | 443, or the endpoint's port | HTTPS (HTTP only if you allow it) | Honoured | Yes: [outbound policy](#outbound-policy) |
| HA lease and metadata snapshots (`MONGORESCUE_HA_TARGET`): an `s3://` location, or a storage target | as the S3 row | HTTPS | Honoured | Yes, as the S3 row (the settings apply once the instance is up) |
| Webhook notification channels (any receiver, such as chat servers or ntfy); Telegram (`api.telegram.org`) and Twilio (`api.twilio.com`) | 443, or the URL's port | HTTPS (HTTP if the URL says so) | Honoured | Yes |
| SMTP server of an email channel | 25, 465 or 587 | SMTP, STARTTLS or TLS as configured | Not supported: direct TCP | Yes |
| Heartbeat URL (Settings → Monitoring) | 443, or the URL's port | HTTPS | Honoured | Yes |
| Audit forwarder URL (Settings → Audit) | 443, or the URL's port | HTTPS | Honoured | Yes |
| OIDC issuer (SSO): discovery, token, JWKS and userinfo endpoints | 443, or the issuer's port | HTTPS | Honoured | Yes |
| Key management service of `MONGORESCUE_KEY_WRAP`: AWS KMS (`kms.<region>.amazonaws.com`), Azure Key Vault (`<vault>.vault.azure.net`), Google Cloud KMS (`cloudkms.googleapis.com`), or Vault Transit | 443, or Vault's port | HTTPS | Honoured | No: the endpoints are those of your provider |
| HashiCorp Vault (`MONGORESCUE_VAULT_ADDR`): connection and S3 credentials | 8200, or the address's port | HTTPS (`VAULT_CACERT` for a private CA) | Honoured | No |
| AWS credential sources of the default chain: instance metadata (`169.254.169.254`), container credentials (`169.254.170.2`), STS | 80 and 443 | HTTP, HTTPS | Honoured for STS | No, on purpose: the S3 requests are guarded, the credential lookups are not |
| Update checker of the desktop app only (`api.github.com`, `github.com`, `*.githubusercontent.com`) | 443 | HTTPS | Honoured | Redirects only to those hosts |

Prometheus scrapes MongoRescue, not the other way round, and outbound alerts go through the notification channels above.

### Outbound policy

Every destination an administrator can configure goes through one policy (`internal/netguard`), checked when the setting is saved for IP literals and again on every connection, after DNS resolution, on the address that is dialled (so DNS rebinding and redirects do not get around it, and the target of a proxied request is checked too):

- **Always refused:** link-local addresses (this includes the cloud metadata services at `169.254.169.254` and `fd00:ec2::254`), unspecified, multicast and broadcast addresses, the NAT64 forms of those (`64:ff9b::a9fe:a9fe`), and numeric host spellings such as `2130706433` or `0x7f.1`. S3 endpoints are guarded like webhooks: a target (or the HA location) pointing at the metadata service fails at save time and at use time.
- **`security.outbound_deny_private`** (Settings → Security → *Deny private network destinations*, off by default): also refuses loopback, private (RFC 1918), CGNAT (`100.64.0.0/10`) and unique-local (`fc00::/7`) addresses for notifications, heartbeats, the audit forwarder, OIDC and S3. It is off because on-premises MinIO, self-hosted chat servers and an internal IdP are common. Turn it on when every destination is public. It applies to new connections at once; a pooled keep-alive connection to S3 ends within 90 seconds. MongoDB servers are not covered (they usually are on a private network).
- **`security.require_https_endpoints`** (*Require https for S3 endpoints*, off by default): refuses to save an S3 target whose endpoint is `http://`, and refuses every S3 request over plain http at use time, redirects included. Targets that already use http are not changed silently: the dashboard shows the `storage_endpoint_insecure` warning that names them, and their backups fail with an error naming the setting until the endpoint is changed. The HA `s3://` location is covered by the use-time check once the instance is up.

Both settings are plain `PUT /api/v1/settings` fields (`security.outbound_deny_private`, `security.require_https_endpoints`); see [configuration.md](configuration.md#security).

### A private certificate authority for S3

An S3 service behind a private CA (typically MinIO) needs its CA certificate. Paste it in the target's **CA certificate (PEM)** field, or send `s3.ca_pem` through the API. It is validated when saved, added to the system roots for that target only (not process-wide, unlike `AWS_CA_BUNDLE` or `SSL_CERT_FILE`, which still work and affect every target), and used by the connection test and every request of the driver, downloads included. Certificates are public, so `ca_pem` is stored and returned as it is.

For the HA lease and snapshot location of `MONGORESCUE_HA_TARGET=s3://…`, set `MONGORESCUE_HA_CA_FILE` to the path of a PEM file (a mounted Secret or ConfigMap). It applies to that location only.

### Restricting egress

Kubernetes: set `networkPolicy.enabled` and list exactly these rules in `networkPolicy.egressTo` (DNS is always allowed); the chart ships the example in [kubernetes.md](kubernetes.md#network-policy). On a host or in Docker, allow the same destinations in the firewall. Pair it with `security.outbound_deny_private` and `security.require_https_endpoints` when your destinations are public and use TLS: the policy refuses at the application, the network policy at the network.

## Authentication

Every API call needs a session or an API key; there is no unauthenticated mode. On a fresh data directory MongoRescue starts in **setup mode** and logs, at `WARN`, a one-time setup code (`Setup required: open http://.../ and enter setup code XXXX-...`). The code lives only in memory, changes on every restart and stops working once the first user exists. Complete setup immediately after deployment.

- **Users** sign in to the dashboard. Passwords are hashed with bcrypt and must be 12 to 72 bytes long. Every user has a dashboard role, viewer, operator or admin ([design/roles.md](design/roles.md)).
- **Sessions** use an `HttpOnly`, `SameSite=Strict` cookie and a CSRF token that the dashboard sends with every change. They expire after 12 hours of inactivity or 7 days after login, and are revoked on logout, from the session list (user menu → *Active sessions*, or Settings → Sessions; *All users* shows everyone's), when the user's password changes (all other sessions) or when the user is deleted.
- **Single sign-on** through an OpenID Connect provider (Entra ID, Google, Okta, Keycloak) is set up in Settings → Single sign-on; see [sso.md](sso.md). Keep at least one local administrator with a strong password as the break-glass way in (MongoRescue refuses to remove the last one while single sign-on is on), consider *Password sign-in: local administrators only*, and shorten `security.session_absolute_timeout` (for example to `12h`): sessions do not follow the provider's, so a user disabled at the provider keeps a running session until it ends, and API keys a single sign-on user created work until the user is deleted in MongoRescue.
- **Login throttling**: 5 failures for the same client address and username lock that pair out for 30 seconds, doubling up to 15 minutes. After 20 failures from one address, each further username from it is locked after one failure; a correct password for an unlocked user is never refused, so users behind one proxy address cannot be locked out by someone else. Attempts are reserved before the password check (one at a time per address and username), so parallel requests cannot exceed the budget. The lockouts live in memory.
- **API keys** are created in Settings → API keys, shown once, and stored only as SHA-256 hashes; each records when it was last used. Send them as `Authorization: Bearer <key>` or `X-API-Key: <key>`. Each key has a scope: `read` (the default), `operator` (also backups, job runs and safe-clone restores) or `admin` ([api.md](api.md#api-key-scopes)). A `MONGORESCUE_API_KEY` from an earlier build is imported once as an admin key named *Imported from MONGORESCUE_API_KEY*; it is stored as an HMAC-SHA256 under a subkey of `secret.key` rather than a plain hash, since it was chosen by hand. Remove the variable afterwards, and replace admin keys with narrower ones where you can.
- **AI assistants** use the [MCP endpoint](mcp.md) `/mcp` with an API key; it never accepts sessions, cannot delete anything or restore in place, and records every call in the audit log (Settings → Security). Behind a reverse proxy on the same host, turn on *Trust proxy headers*; otherwise `/mcp` refuses requests whose `Host` is not local (DNS-rebinding protection).

The channel **test** endpoint and the connection test make outbound requests to whatever destination they name, so only give accounts and keys to people and systems you trust with network access from the MongoRescue host.

## Data directory

`MONGORESCUE_DATA_DIR` holds:

| File | Contents |
| :--- | :--- |
| `mongorescue.db` (+ `-wal`, `-shm`) | SQLite metadata database: jobs, backup and restore history, users, sessions, API key hashes, connections and notification settings. Mode `0600`. |
| `mongorescue.db.pre-v<from>-to-v<to>` | The copy of the database taken before a start applied schema migrations (the newest 3 are kept, `-pre-migration-copies`), the way back after an upgrade; never contains `secret.key`. See [upgrading.md](upgrading.md#what-a-start-does-to-the-database). Mode `0600`. |
| `secret.key` | The key encrypting connection strings and notification channel secrets inside the database (unless `MONGORESCUE_SECRET_KEY` is set). Mode `0600`. |
| `secret.key.wrapped` | With [key wrapping](security.md#wrapping-secretkey-with-a-kms) (`MONGORESCUE_KEY_WRAP`), the key wrapped by the KMS instead of `secret.key`, which then does not exist. Mode `0600`. |
| `mongorescue.lock` | Advisory lock that stops a second instance from using the same directory. The operating system holds the lock for the process, not the file: after a crash it is released with the process, so a leftover `mongorescue.lock` never has to be deleted. |

Connection strings and channel secrets are encrypted with AES-256-GCM. **Losing the key loses them**: MongoRescue refuses to start when the key does not match the database (`the secret key does not match the key this database was encrypted with`). Store a copy of `secret.key`, or set `MONGORESCUE_SECRET_KEY` from your secret manager, and keep it apart from database backups so one leaked backup does not reveal both. A refused start changes nothing in the database: put the original key back (file or variable) and the stored credentials open again. `MONGORESCUE_SECRET_KEY` takes precedence over `secret.key` when both exist, and a damaged `secret.key` is reported, never replaced. The age identity used for backup encryption is one of these stored secrets; escrow it separately as well (see [encryption.md](encryption.md#key-management-and-loss)).

Losing the database does not lose the backup archives, but it loses the records that point to them, your schedules, users and connections. Back it up with [metadata backups](#metadata-backups) and keep `secret.key` in a [recovery kit](#recovery-kit).

### Disk space

Keep the data directory on a filesystem with room to spare: every run writes its record to `mongorescue.db` and its log under `logs/`, and a full disk stops both.

- **Before every run** (backups, job runs, restores, PITR base backups) MongoRescue checks the free space of the data directory. Below `MONGORESCUE_MIN_FREE_SPACE_MB` (`-min-free-space-mb`, 100 MiB by default, `0` turns the check off) the run does not start: the API answers `503` with the free space and the minimum (and `Retry-After: 30`), and a scheduled run is recorded as failed, so `backup.failed` and the job's heartbeat `/fail` ping fire.
- **When a metadata write fails because the disk is full** (SQLite `database or disk is full`, `ENOSPC`), MongoRescue publishes the critical event `system.disk_full` (sent to every enabled notification channel, whatever the rules), shows the `data_dir_full` warning (a dashboard banner, `GET /api/v1/settings`, counted by `mongorescue_settings_warnings`), and refuses new backups and restores until there is enough free space again; it checks every 30 seconds and resumes on its own. Where the free space cannot be read, the episode ends as soon as a write succeeds (a small probe file in the data directory, at most every 5 seconds, or the save of a record that waited). `system.disk_full` is sent at most once an hour: an episode that starts sooner after the last alert is only logged and shown as the warning.
- **A backup whose final record cannot be saved never reports success.** The save is retried for a few seconds; if it still fails, the backup is reported as failed (`backup.failed`, a failed job run, the heartbeat's `/fail` ping, no copies, no retention), the failure is logged and written to the run's log, and the failed record is saved as soon as writes work again (at the latest by the next start, which fails every run left in progress). Its archive is complete, so it is kept (`the backup record could not be saved (data disk full: …); the archive was kept`, never `archive_cleanup_pending`, and copies already made stay): the [storage scan](verification.md#storage-scans) lists it as an orphan and *Import* revives the record as a completed backup. Only the artifact of a run that failed by itself (an upload that broke off) is cleaned up, as before.
- **At the next start**, a backup still recorded as in progress is failed. If its archive exists in storage it may be complete, so it is kept (`interrupted before the record was saved; the archive was kept`, no `archive_cleanup_pending`): the [storage scan](verification.md#storage-scans) lists it as an orphan and *Import* revives the record as a completed backup. Only an archive smaller than the size recorded for it goes to the purge; when the target cannot be read nothing is deleted.
- **Only a full disk fails a run.** Any other failure of the final save (a busy or locked database, a timeout) is transient: the record is kept as it is and saved in the background, retried with a backoff from 1 second to 1 minute until it succeeds, and the run reports its real outcome. A good archive or clone is never deleted over a lock. A restart before that save succeeds leaves the record to the startup recovery.
- **The same holds for restores** (also point-in-time restores and chain tests): the record is stored before `restore.succeeded` is published. A restore whose record cannot be saved is reported as failed (`restore.failed`), and what it restored is handled like the clones of any failed restore: a safe clone, or the clones of a point-in-time restore or chain test, are dropped; an in-place target is never touched.
- The writes at the start of a run (the in-progress records, the chunks of the PITR collector) report a full disk too, so the episode starts before a run gets far.

The rest of the server copes with a full data directory: requests that must write fail with an error, the database stays consistent (SQLite rolls back the failed transaction), and nothing needs repairing once space is freed. Alert on the filesystem itself as well (node exporter's `node_filesystem_avail_bytes`), well before the minimum.

### Metadata backups

Settings → Recovery → *Metadata backups* (the `metadata_backup` settings, off by default) takes a snapshot of `mongorescue.db` on a schedule (every 24 hours by default) and on demand (*Back up now*, `POST /api/v1/metadata-backup/run`):

1. SQLite's `VACUUM INTO` writes a consistent copy of the live database to a temporary directory inside the data directory (mode `0700`). It is the only temporary copy MongoRescue writes, and it holds metadata, never a dump.
2. The copy is streamed through age encryption, with the backup encryption settings (Settings → Encryption), to the chosen storage target as `_mongorescue/metadata/<install_id>/mongorescue-<UTC time>.db.age`, and the temporary copy is removed. Storage scans ignore these objects. The install ID (16 hex characters; part of the snapshot key shown under Settings → Recovery, `prefix` in `GET /api/v1/metadata-backup` and `metadata_prefix` in the recovery kit) is derived from `secret.key`: it stays the same on a host restored with the same key, and installations sharing a bucket and prefix write, and prune, only their own snapshots. Clones that share one `secret.key` share the install ID too; give each installation its own key or its own target prefix.
3. All but the newest `retention_count` snapshots (14 by default) under the installation's own prefix are deleted from the target.

With encryption off, snapshots are still uploaded (as `.db`), and the dashboard warns until encryption is on. Credentials inside a snapshot (connection strings, S3 keys, notification secrets, the age identity and passphrase) stay sealed with `secret.key` either way, so a snapshot alone opens nothing; keep `secret.key` apart from the snapshots. Write the snapshots to a target that does not live on the MongoRescue host, or they share its fate. A failed snapshot is retried after an hour, shown under Settings → Recovery, counted in `mongorescue_metadata_backups_total{result="error"}` and published as `metadata_backup.failed` for notification rules. Alert on `time() - mongorescue_last_successful_metadata_backup_timestamp_seconds` as well.

Other consistent copies still work: stop MongoRescue and copy the data directory, run `sqlite3 /data/mongorescue.db ".backup '/backup/mongorescue.db'"` against the live database, or snapshot the whole volume atomically. Never copy `mongorescue.db` alone while the server runs (WAL mode: the copy may miss changes or be torn).

### Restore MongoRescue from a snapshot

On a new host (or after losing the data directory), with the [recovery kit](#recovery-kit) at hand:

1. Install the same or a newer MongoRescue release. Do not start it yet (a start creates a new `secret.key` and an empty database).
2. Download the newest snapshot from the target named in the kit (`recovery.json` → `metadata_snapshot`; the installation's prefix is `metadata_prefix`, `_mongorescue/metadata/<install_id>/`), with the target's credentials from the kit, e.g. `aws s3 cp s3://<bucket>/<prefix>_mongorescue/metadata/<install_id>/mongorescue-<time>.db.age .`. Pick a newer snapshot under the same prefix than the kit names if there is one.
3. Decrypt it with age: `age -d -i identities.txt -o mongorescue.db mongorescue-<time>.db.age` when the server holds the private key (`identities.txt` is in the kit). In recipient-only mode the server has no private key and the kit no `identities.txt`: use the key file you keep for the recipients, `age -d -i <your key file> -o mongorescue.db mongorescue-<time>.db.age`. In passphrase mode run `age -d -o mongorescue.db mongorescue-<time>.db.age` and enter the backup passphrase (the kit never contains passphrases). The kit's `README.txt` names the step that fits. A `.db` snapshot is not encrypted: rename it.
4. Put `mongorescue.db` and the kit's `secret.key` into the data directory, owned by the MongoRescue user, with `chmod 600`. If the old instance used `MONGORESCUE_SECRET_KEY`, set it to the content of `secret.key` instead. The key must be the one the snapshot was written with: with another key MongoRescue refuses to start and changes nothing.
5. Start MongoRescue. Jobs, backup records, users, settings, storage targets and notification channels are back; sign in with your usual account. Backups taken after the snapshot have no record yet: run a storage scan (Settings → Storage → *Scan now*) and import them.

Without any snapshot, start a fresh instance with the kit's `secret.key`, recreate the storage targets from `recovery.json`, put the private keys from `identities.txt` (or your passphrase) under Settings → Encryption and import the archives a storage scan finds.

### Recovery kit

Settings → Recovery → *Download recovery kit* (or the dashboard banner) asks for a kit passphrase (at least 12 characters, typed twice) and your current password, and downloads `mongorescue-recovery-kit-<date>.tar.age`: a tar archive sealed with age scrypt under that passphrase. MongoRescue does not keep the passphrase. Open it with `age -d -o kit.tar mongorescue-recovery-kit-<date>.tar.age` and `tar -xf kit.tar`. It contains:

| File | Contents |
| :--- | :--- |
| `README.txt` | The recovery steps above |
| `secret.key` | The key sealing the credentials in `mongorescue.db` and its snapshots. With [key wrapping](security.md#wrapping-secretkey-with-a-kms), `secret.key.wrapped` and the KMS key URI instead (recovering then needs access to the KMS key), unless the plaintext key was asked for (`plaintext_secret_key` in the [API](api.md#metadata-backups-and-the-recovery-kit)) |
| `secret.key.previous` | Only when asked for (*Include the previous key*): the key the last `secret.key` rotation replaced, which opens the snapshots taken before it |
| `identities.txt` | The age X25519 private keys, current and retired (only when one is configured) |
| `recovery.json` | Encryption settings (passphrases are only flagged as configured, never included), every storage target with its credentials, and the location of the latest metadata snapshot |

The kit holds every secret in plain form once opened: store it offline (a password manager, an encrypted USB stick, a safe), apart from the backups and from the kit passphrase. Only a signed-in administrator can download it, after confirming their password; API keys are refused whatever their scope. Every download is written to the audit log (who and when, never the passphrase or the content).

Until a kit has been downloaded, and again after `secret.key`, the encryption keys or a storage target change, the dashboard shows a reminder. *Remind me later* hides it until the next such change.

Since v0.14.0 the database records a checksum of every schema migration applied to it, and MongoRescue refuses to start (`ErrMigrationChanged`, naming the migration, nothing written) when a binary embeds an edited version of one; starting an older release on a database migrated by a newer one is refused as before (`ErrSchemaTooNew`); [upgrading.md](upgrading.md#rolling-back) explains how to go back with the pre-migration copy.

A single damaged row (a record whose stored JSON no longer fits, or whose credentials cannot be decrypted) does not take a list down: it is skipped, logged with its table and ID, shown to administrators in a dashboard banner and left unchanged on disk. [troubleshooting.md](troubleshooting.md#unreadable-records) shows how to back up the database, inspect the row with `sqlite3`, and repair, export or remove it.

Releases before the SQLite store kept metadata in `state.json`; it is imported automatically on the first start and renamed to `state.json.migrated-<timestamp>` (see [configuration.md](configuration.md#upgrading-deprecated-environment-variables-and-configjson)). Job connection strings from those releases become managed connections.

## RPO and RTO

The recovery point objective (RPO) is how much data you can afford to lose: how old the newest good backup may be when disaster strikes. Every job has one: its *Recovery point objective* in the job form (`rpo_minutes`, 15 minutes to 90 days), or by default twice the longest gap between its runs plus an hour, at least six hours (an hourly job gets 6 hours, a daily one 49, a weekday job 145, since Friday to Monday is its longest gap). MongoRescue checks every database of every enabled job every 5 minutes and whenever a job is saved, paused, resumed or backed up, publishes `job.rpo_missed` once when its newest successful backup is older than that and `job.rpo_recovered` when it is fresh again, and exports `mongorescue_job_rpo_seconds` and `mongorescue_job_rpo_met` ([metrics.md](metrics.md#recovery-point-objectives)). A database without a successful backup counts from when it joined the job, so adding one to an old job does not alert at once. Set the RPO to what you promised, not to the schedule: a daily job whose data must never be more than a day old needs `24` hours, not the default 49.

Pausing a job forgets its breaches without an event. Resuming a job whose newest backup is still older than its objective therefore alerts right away (`job.rpo_missed`), before its next run had a chance to catch up; run it once by hand after resuming if you expect that.

The recovery time objective (RTO) is how long a restore may take. MongoRescue estimates it per database from the newest passed [restore test](verification.md#automated-restore-tests) (restoring into a temporary database, so the figure includes the copy and the index builds), else from the newest completed real restore of the whole database; without either it is *Unknown*. Turn on restore tests to keep the estimate current, and compare it with your objective: the estimate grows with the data. A large restore is bounded by its stream: the S3 download is watched for stalls and resumed after a connection reset (a restore no longer starts over at 90%), and `mongorestore` can restore more collections and insertion workers at once ([restore parallelism](configuration.md#restore-parallelism)); measure with your own data, because the best values depend on the target's CPU, disks and write load, and restore tests record the values they ran with.

Overview → *Recovery readiness* (and `GET /api/v1/readiness`, [api.md](api.md#recovery-readiness)) brings this together per database: the last good, verified and restore-tested backups, the RPO with its current age, the estimated RTO, and whether the keys are in a recovery kit. A row is *Not ready* when an RPO is missed or the newest restore test or verification failed, and shows a *Warning* when it was never verified or restore-tested, its jobs are paused, or its encrypted backups have no escrowed key.

**Filtered backups and the RPO.** A backup with a [collection filter](api.md#collection-filters-per-database) (only some collections, or all but some) counts towards its database's RPO like any successful backup: the database is "fresh" as soon as the filtered backup completes. It does not cover the collections it left out: their newest backup may be much older, or there may be none, and neither the RPO check nor the restore tests notice. Such backups carry `filtered: true` with the collections they kept (`collections`) or skipped (`exclude_collections`), show a *Filtered* badge in the Backups list and details, and the readiness row says *Covered partially: N collections excluded* (or *only N collections*). If the excluded collections matter, back them up with a job of their own and give it its own RPO.

## Service level objectives

An alert tells you something broke; a service level objective (SLO) says how much breakage you accept. These are suggested starting points for the backup service itself, each with the indicator (SLI) as PromQL over the [metrics](metrics.md). Adjust the targets to what you promised the data owners, and write them into the job RPOs first: the SLO measures how well you keep them.

| SLO | SLI | Suggested target (30 days) |
| :--- | :--- | :--- |
| **RPO compliance**: every database is backed up within its job's recovery point objective | Share of time `mongorescue_job_rpo_met` is 1 | 99.5% (about 3.6 hours of breach) |
| **Backup success**: scheduled runs end `ok` | Runs that ended `ok` over runs that ended `ok`, `partial` or `failed` | 99% |
| **Restore-test success**: restores that were proven to work | Restore tests that ended `ok` over all of them, and the newest passed test no older than 8 days | 95% of tests, freshness always |
| **PITR lag**: the recovery window reaches almost to now | Share of time a stream's `mongorescue_pitr_lag_seconds` is under 5 minutes | 99% |

```promql
# RPO compliance, per job and database, and overall (30 days).
avg_over_time(mongorescue_job_rpo_met[30d])
avg(avg_over_time(mongorescue_job_rpo_met[30d]))

# Backup success.
sum(increase(mongorescue_job_runs_total{status="ok"}[30d]))
  / sum(increase(mongorescue_job_runs_total{status=~"ok|partial|failed"}[30d]))

# Restore-test success, and tests that are too old (should return nothing).
sum(increase(mongorescue_restore_tests_total{result="ok"}[30d]))
  / sum(increase(mongorescue_restore_tests_total[30d]))
time() - mongorescue_last_successful_restore_test_timestamp_seconds > 8 * 24 * 3600

# PITR lag: the share of the last 30 days a stream was less than 5 minutes behind.
avg_over_time((mongorescue_pitr_lag_seconds < bool 300)[30d:1m])
```

Notes:

- The gauges only have samples while Prometheus can scrape MongoRescue, so `avg_over_time` ignores the time the instance was down. Count that time against the objective too: multiply by `avg_over_time(up{job="mongorescue"}[30d])`, or use `MongoRescueDown` and the external heartbeat as the signal that the SLI is blind.
- `skipped` runs (outside a backup window) and `cancelled` ones are neither success nor failure and are left out of the backup success ratio.
- A job that never succeeded has no restore-test series; the staleness rule only sees jobs that have passed one. Pair it with `MongoRescueRestoreTestFailed` and the *Recovery readiness* report (`no_restore_test`).
- The `increase` and subquery forms are heavy over 30 days. Record them, for example `mongorescue:rpo_compliance:ratio_30d`, with a recording rule evaluated every few minutes, and graph the recorded series (the [Grafana dashboard](monitoring.md#grafana-dashboard) shows the same indicators over the selected range).
- The error budget is what is left of the target: at 99.5% over 30 days, 3.6 hours of RPO breach. Page when the budget burns fast ([MongoRescueRPOMissed](runbooks.md#mongorescuerpomissed) already pages on a single breach) and review it monthly; a budget that is always unused means the RPO is looser than it needs to be.

The [incident runbooks](runbooks.md) say what to do when an alert fires.

## 3-2-1 backups

The 3-2-1 rule asks for **3** copies of your data (the live database and two backups), on **2** different kinds of storage, with **1** of them off site. MongoRescue gets there with [copy targets](configuration.md#copies-on-a-second-target-3-2-1):

1. Add a second storage target in another place than the primary. If the primary is a local disk or a NAS, use an S3 bucket in another region or with another provider. If the primary is S3, use a bucket at another provider, or a local disk you take off site.
2. In the job form, check that target under *Copy targets* (up to three). Keep *Copy mode* on **async** unless a run must not count as done until its copy exists (**sync**).
3. Every backup is then copied byte for byte, checked against its SHA-256, under the same key. Encryption stays as it is: an encrypted archive stays encrypted on every target, so the copy target never sees plaintext and needs no key.
4. Give the off-site copy its own immutability: an S3 target with [Object Lock](configuration.md#immutable-backups-s3-object-lock) locks each copy with its own retention, so even credentials stolen from the primary cannot delete it.

What you get:

- **Restore from either target.** A restore reads the primary, and falls back to a healthy copy by itself when the primary archive is missing or has the wrong size; the restore record says so (`source_fallback`). The dashboard's restore dialog (*Read the archive from*), `--from-target` on the CLI and `source_target_id` in the API choose a copy explicitly.
- **Verification of every copy.** The integrity sweep re-reads every copy like the primary and copies a damaged or missing one again.
- **Alerts.** `backup.copy_failed` and `backup.copy_recovered` events (subscribe to them in notification rules), the `mongorescue_backup_copy_queue_depth` metric, and the readiness warning `copy_missing` when a backup's copies are not done six hours after it finished. The readiness row shows "N copies" for the newest backup.
- **One lifecycle.** Retention and deletion apply to the backup with all its copies: a deleted backup can be undeleted with its copies during the grace period, and the purge then removes every copy, each after its own target's lock.

Copies read storage, not MongoDB, so they run outside backup windows too; they upload at the job's (or the general) `max_upload_mbps`. Size the copy target like the primary: it holds the same archives for as long as the primary does.

## Cross-region disaster recovery

"We use S3" is not disaster recovery: a region outage, a lost account or one leaked key can take the primary bucket and every copy in the same failure domain with it. Real DR keeps an **immutable copy in another region, behind other credentials**, and proves regularly that a restore works **from that copy alone**. MongoRescue checks each part and sums it up per database under Overview → *Recovery readiness* (*DR: cross-region ✓* or *DR: same region ✗*).

### Locked copies

A job with **Require locked copies** (`require_locked_copies` in the API) is refused (`400`) unless every one of its copy targets has [S3 Object Lock](configuration.md#immutable-backups-s3-object-lock), so a copy is never left deletable. **Settings → Security → Require locked copies** (`security.require_locked_copies`) applies to every job while it is on: no job can opt out (a new job without the policy, or a job turning it off, is refused with `400`), and the copy targets of every job and PITR stream must have Object Lock. Turning the setting off, or a job's policy while the setting is off, is a lowered protection: it takes effect after the [delete grace period](security.md#lowering-a-protection-takes-the-grace-period-too) and, under the [two-person rule](security.md#the-two-person-rule), only once a second administrator approved it (pending change and approval kinds `disable_locked_copies` and `job_locked_copies`).

### Region awareness

Every storage target has a **region**: an AWS S3 target takes its `s3.region`, or, when that is empty or `auto`, the bucket's location (`GetBucketLocation`, needs `s3:GetBucketLocation`); any other target (MinIO, another S3 provider, a local disk or NAS) only has the label you give it (for example `dc-frankfurt`), since its `s3.region` says nothing about where the data is. Set the label on every such target. For a database whose jobs have copy targets the readiness report then warns:

- `dr_same_region` when no copy target is in a known region other than the primary's (an unknown region never counts as another one);
- `dr_same_credentials` when every copy target is reachable with the primary's credentials or account, where this is detectable (see [copy targets in another account](#copy-targets-in-another-account));
- `dr_unlocked_copy` when a job that requires locked copies has a copy target without Object Lock (a job saved before the setting was turned on). The check of a job's copy targets runs when they or its policy change, so pausing or renaming such a job still works; removing the Object Lock of a copy target that a requiring job or stream uses is refused (`400`).

### DR drills

A **DR drill** is a [restore test](verification.md#automated-restore-tests) that reads the archive from a copy target instead of the primary: in the job form, *Restore test* → *Read from* a copy target (`restore_test.source_target_id`, one of the job's `copy_targets`). Pick the copy in the other region. A drill tests the newest backup whose copy on that target is complete (with async copies that may be the previous backup); a scheduled drill is skipped while no backup has a complete copy there yet, and a manual one is refused. The result names the target it read (`source_target_id`), and the readiness row shows the last drill and its result. `dr_drill_stale` warns when no drill passed in the last 30 days.

**DR: cross-region.** A database's readiness row shows *DR: cross-region* once one of its jobs has a copy target in another region with Object Lock, and a drill from such a copy passed within the last 30 days. Otherwise it shows what is missing (*same region*, *not locked*, *no recent drill*).

### Region failure

The runbook for "region A is gone" (the primary target, maybe the databases and MongoRescue with it). Prepare it before you need it:

- The **recovery kit** (and its passphrase) and the backup **encryption key** are stored **outside region A**: in another region, another account, or offline. Without `secret.key` the stored credentials are unreadable, and without the private key encrypted archives are.
- [Metadata backups](#metadata-backups) go to a target in region B, so the instance's own database survives too.
- A drill from the region B copy passed recently (no `dr_drill_stale`).

When region A fails:

1. **Start MongoRescue in region B** (or anywhere with access to region B). If the instance in region A is gone, restore it from the metadata backup in region B with the recovery kit ([restore MongoRescue from a snapshot](#restore-mongorescue-from-a-snapshot)), or start a fresh instance and add the region B copy target with its own credentials.
2. **Check what region B holds.** Every backup lists its copies; the region B target must show them *done*. Copies that were still pending when region A went are lost with it: the newest restorable backup is the newest one with a done copy in region B.
3. **Restore from region B.** A restore falls back to a healthy copy by itself when the primary archive cannot be read; to be explicit, choose the region B target under *Read the archive from* (`source_target_id`, `--from-target` on the CLI). Restore into a new cluster in region B, as a safe clone first.
4. **Point the jobs at region B.** Until region A is back, make the region B target the primary of the jobs (and add a new copy target in a third region), so new backups do not fail against the lost one.
5. **After region A returns**, run the integrity sweep: it verifies every copy and copies missing or damaged ones again.

## High availability

A single instance is a single point of failure: while it is down, no scheduled backup runs and the PITR oplog is not collected. A restart policy (Docker `restart: unless-stopped`, systemd `Restart=on-failure`, a Kubernetes StatefulSet) covers a crashed process; **active-passive high availability** also covers a lost node, zone or host. The design, with its failure matrix, is in [design/ha.md](design/ha.md).

Two (or more) instances share a **storage target** and a **secret key**, and nothing else: each has its own data directory.

- The instance holding the **lease** (a small object on the HA target, renewed every 10 seconds) is **active**: it runs schedules, PITR collection, copies, integrity sweeps, metadata backups, notifications and the heartbeat, exactly like a single instance.
- The others are **standbys**. A standby serves no data and accepts no sign-in: its database is only as current as its last takeover, so its users, sessions and API keys are not trusted. It serves only `/api/v1/health` (`"role": "standby"` and the active instance), `/metrics` without credentials but with nothing but process, build and HA series, and a static page in place of the dashboard that names the active instance. Everything else, the API, signing in and MCP included, answers `503` with `Retry-After`.
- The active instance writes an encrypted **snapshot of its metadata database** to the HA target as soon as it becomes active, after every finished backup or restore and at least every 5 minutes. A standby takes over without a snapshot only when there is no lease, no snapshot under the cluster's prefix and no HA history for the cluster in its `ha.json`: a new cluster, or HA enabled on an existing single instance, which keeps its own database. This is never silent: `MongoRescueHAStartedFromEmptyState` fires and `ha.empty_start` goes to every channel; if you did not expect it, check the HA target and run a storage scan of every storage target to re-import the archives. When a lease existed it stays standby and `MongoRescueHATakeoverFailed` fires, since its own database would lose the previous holder's metadata; an instance with HA history that finds the HA target empty refuses as a suspected rollback (`MongoRescueHARollbackSuspected`) until it is forced.
- When the active instance dies, a standby **takes over** once the lease has not changed for its TTL (30 s): it restores the newest snapshot whose encrypted header confirms its cluster and epoch (keeping its own database as `mongorescue.db.bak-<UTC time>-epoch<N>`, the newest three copies), marks the runs that were in progress as interrupted, imports the archives written after the snapshot, and runs once every job whose cron fire fell in the gap, when it can verify that the fire did not run (else it sends `ha.catch_up_skipped` and leaves the run to you). With the defaults it is active about 40 seconds after the last renewal, and the takeover work takes seconds.
- The active instance stops working **20 seconds** (`ttl − renew`) after its last successful renewal if it cannot reach the HA target, so two instances never work at once. Runs check the lease before they start and before they are recorded as completed; every backup, restore and job run carries the lease **epoch** (`ha_epoch`) it ran under.
- A graceful shutdown (`SIGTERM`) of the active instance writes a last snapshot and releases the lease, so a standby takes over within one renew interval.

### Setting it up

1. Create the shared key once (`openssl rand -base64 32`) and give it to every instance as `MONGORESCUE_SECRET_KEY`. An instance with another key refuses to start.
2. Choose the HA target: an S3 bucket (or prefix) **without Object Lock** that every instance can reach, ideally in another failure domain than any single instance. AWS S3 and MinIO enforce the conditional writes the lease needs; HA refuses to start on a service that ignores them. Give the instances credentials through the AWS default chain (environment, profile, instance role, IRSA).
3. Start every instance with the same HA options ([configuration.md](configuration.md#high-availability)):

   ```bash
   MONGORESCUE_SECRET_KEY=... \
   MONGORESCUE_HA_MODE=active-passive \
   MONGORESCUE_HA_TARGET='s3://mongorescue-ha/prod?region=eu-central-1' \
   MONGORESCUE_HA_CLUSTER=prod \
   AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
   mongorescue -data-dir /var/lib/mongorescue
   ```

   On Kubernetes, install the chart twice ([kubernetes.md](kubernetes.md#high-availability)).
4. Complete the setup, and every later change, on the **active** instance (`GET /api/v1/health` says which one it is). Changes reach the standby with the next snapshot.
5. Load the alert rules ([monitoring.md](monitoring.md#prometheus-alert-rules)) and scrape every instance: `MongoRescueHANoActiveInstance` fires when no instance of a cluster has been active for a minute.

Things to know:

- **Use the HA target for nothing else that needs Object Lock.** Lease renewals would leave a locked version behind every 10 seconds.
- **Storage targets that only one instance can reach** (a local disk, a NAS mounted on one host) do not survive a takeover. Back up to S3 targets every instance can reach; the default "Local disk" target of a standby points to the active instance's directory after a takeover.
- **Backup encryption keys** (the age identity or passphrase in the settings) are stored in the database, sealed with `secret.key`, and travel with the snapshot: nothing to copy.
- **`secret.key` is not rotated in HA mode.** An instance rotating alone would leave the others unable to read its snapshots and lease, so the rotation is refused with `409` (`keyrotation: secret.key cannot be rotated while active-passive HA is on…`). Follow [the procedure below](#rotating-the-secret-key-of-an-ha-cluster) instead.
- **The lease is authenticated and remembers the last snapshot.** Deleting the newest snapshots or the lease, or replacing the lease, makes the next takeover refuse (`MongoRescueHARollbackSuspected`) instead of restoring older metadata; after checking the HA target, force it with `mongorescue ha force-takeover` on the instance that should become active ([details](design/ha.md#forcing-a-takeover)). **Upgrading from 0.27 or earlier** needs no operator: upgrade the standbys first and the active instance last. The old lease has no MAC; such a lease is trusted only while the HA target holds no snapshot with an authenticated header and the instance has not seen an authenticated lease (recorded in `ha.json`), so the upgraded standby takes over and signs the lease, and every instance knows it within one renew interval. Known edge, a double failure in the middle of the upgrade: if an instance of the old version becomes active again after an upgraded one wrote snapshots with a header (for example the upgraded active instance fails while the old one is still running), the old instance's lease and its release carry no MAC, and every upgraded instance refuses to take over from them. Finish the upgrade, check the HA target, and run `mongorescue ha force-takeover --from-snapshot <newest key>` on the instance that should become active.
- **Snapshots are authenticated.** Each snapshot's encrypted header names its cluster, epoch and time; a snapshot copied or renamed on the HA target (an old one under a higher epoch, or one of another cluster that shares the key) is refused, never restored nor deleted, and `MongoRescueHASnapshotRejected` fires until you delete it (the log names its key; [details](design/ha.md#security)). Snapshots written by versions before this check have no header: they are restored only while the cluster has no snapshot with one, with a warning in the log, and are deleted once the upgraded active instance has written its first snapshot. An instance of an earlier version cannot restore a snapshot with a header (its takeover fails and `MongoRescueHATakeoverFailed` fires), so upgrade the standbys first and the active instance last.
- **Metadata changed after the last snapshot** (a job edited a minute before the active instance died) is lost; backups recorded after it are imported from the storage again, as manual backups. Notifications pending in the snapshot may be sent twice; receivers can de-duplicate on the event ID.
- **A shared data directory** (two processes on one volume) is not supported: the data directory lock refuses the second process. Where the volume survives the node, run one instance with a restart policy instead.
- **Use the active instance** for the dashboard, the API, the CLI and MCP. Make a load balancer health-check `GET /api/v1/ready`, which is `200` only on the active, non-draining instance with a writable database and free disk (and `503` with a reason `code` otherwise), so it follows a failover; `GET /api/v1/health` is the liveness check and is `200` on the standby too. On Kubernetes see [Readiness and the shared Service](kubernetes.md#readiness-and-the-shared-service). Without such a check, requests that reach the standby get `503`.
- **Load balancer health checks: treat `503` as not ready, not as down.** An instance keeps its port open while it changes role (a takeover or a step-down takes a second or more): every request but the health check and `/metrics` answers `503` with `Retry-After: 1`, and the health check answers `200` with `"role": "switching"`. Route only to `"role": "active"`; retry a `503` (honouring `Retry-After`) instead of marking the instance failed. A refused connection means the process is down. See [Role changes and the HTTP listener](design/ha.md#role-changes-and-the-http-listener).

### Checklist

- [ ] The same `MONGORESCUE_SECRET_KEY` on every instance, stored in your secret manager and in the [recovery kit](#recovery-kit).
- [ ] The instances on different hosts, zones or nodes (Kubernetes: anti-affinity), each with its own data directory.
- [ ] The HA target reachable from every instance, without Object Lock, with credentials only MongoRescue holds.
- [ ] Every storage target of every job reachable from every instance.
- [ ] `MongoRescueHANoActiveInstance`, `MongoRescueHATakeoverFailed`, `MongoRescueHASnapshotRejected`, `MongoRescueHARollbackSuspected`, `MongoRescueHAStartedFromEmptyState` and `MongoRescueDown` alerts loaded, every instance scraped.
- [ ] A takeover tested once: stop the active instance (or kill it), watch the standby report `"role": "active"`, check that the next scheduled backup runs there, and start the old instance again (it comes back as standby).

### Rotating the secret key of an HA cluster

Every instance seals its database, the lease's `key_id` and the metadata snapshots with the same key, so the key changes for the whole cluster at once, in a maintenance window during which no backup runs:

1. Stop every standby.
2. Restart the active instance as a single instance (`MONGORESCUE_HA_MODE=off`, same data directory) and rotate the key there: **Settings → Security → Rotate secret key** for a key in `secret.key`, or the [manual procedure](encryption.md#key-rotation-runbook) for a key set by `MONGORESCUE_SECRET_KEY`. Download a new [recovery kit](#recovery-kit). Stop it.
3. Delete the cluster's objects on the HA target, which are sealed with the old key: the lease `_mongorescue/ha/<cluster>/lease.json` and the snapshots under `_mongorescue/metadata/ha/<cluster>/`.
4. Give the new key to every instance. Empty the data directory of every former standby (its database is sealed with the old key; it receives the metadata at its first takeover).
5. Run `mongorescue ha force-takeover --keep-local-database` on the former active instance (it has seen the cluster, so without a lease it would refuse the takeover as a suspected rollback), then start it in HA mode first: it takes over with its own database and writes a snapshot at once. Then start the standbys; they check that they can read that snapshot.

### Failure domains

High availability protects against losing a MongoRescue instance, not against losing what it shares with the databases it protects. Run MongoRescue in **another failure domain than the protected databases**: another cloud account, region or Kubernetes cluster, so an incident, a compromised account or a misapplied change that takes the databases does not take the tool that restores them. Give it **storage credentials that only it holds**: the applications and their operators should not be able to delete or overwrite backups, and MongoRescue's credentials should not reach anything else. Combine this with [locked copies in another region](#cross-region-disaster-recovery): HA keeps the schedules running, the copies keep the data.

## Large databases

MongoRescue streams `mongodump --archive`: a logical copy that reads every document through the query layer of the member it connects to. That is simple, portable across versions and restorable into any database, but its cost grows with the data:

- **Load.** A dump scans every collection, pulls the whole data set through the member's cache (evicting the working set your application relies on) and competes for disk and network. On a busy primary that shows as latency.
- **Time.** Expect roughly the read throughput of the member's disks, often 50 to 200 MB/s before compression; a restore is slower, since `mongorestore` rewrites every document and rebuilds every index. Measure both with a [restore test](verification.md#automated-restore-tests): its duration is the RTO MongoRescue reports.
- **Consistency.** A dump without the oplog is consistent per document, not across collections: writes during a long dump land in some collections and not in others. Use [point-in-time recovery](design/pitr.md) when that matters.

As a rule of thumb, `mongodump` is comfortable up to a few hundred GB per database and a dump window of a few hours. Beyond about 1 TB, when the restore must finish in less time than a logical restore takes, or when the dump cannot fit into a quiet window even from a secondary, use a physical backup instead: filesystem or volume snapshots (LVM, ZFS, EBS, persistent disk snapshots) of a secondary with journaling on the same volume, your cloud provider's backups (MongoDB Atlas cloud backups, Ops Manager or Cloud Manager), or Percona Backup for MongoDB. Keep MongoRescue for the databases that fit, for portable copies, and for restore tests of what you can restore logically.

### Reading from a secondary

Each connection may set a **read preference** (`primary`, `primaryPreferred`, `secondary`, `secondaryPreferred` or `nearest`, with optional tag sets such as `dc=east,use=backup`), and each job may override it. Empty keeps whatever the connection string says, which is the primary unless it sets `readPreference` itself. MongoRescue adds the preference to the connection string it hands `mongodump` (through the private tools config file, never on the command line) and to the driver that captures the [manifest](verification.md), so both read from the same kind of member.

- `secondaryPreferred` reads from a secondary and falls back to the primary when none is available; it is the usual choice.
- `secondary` never reads from the primary: a backup fails before `mongodump` starts when no secondary is reachable, also on a standalone server or a single-member replica set.
- Tag sets pick dedicated members, such as a hidden or delayed member tagged for backups; they are tried in order.
- A `directConnection=true` URI talks to one member only and ignores read preferences: list the replica set members (or use `mongodb+srv://`) with `replicaSet=` instead.

Before `mongodump` starts, MongoRescue asks the member the preference selects (`hello`) and records it on the backup (`source_member`: host, `primary` or `secondary`, and the replica set); the run log names it too, and *Test connection* reports which member a backup would read from. With several eligible secondaries `mongodump` may choose another one of them; it still satisfies the same preference. Remember that a secondary may lag: a backup from it is as old as its replication lag.

### Sharded clusters (mongos)

Sharded clusters are not supported ([#58](https://github.com/YigitCittan/mongorescue/issues/58)): `mongodump` through a `mongos` is not consistent across shards (the balancer and chunk migrations can leave documents missing or duplicated in an archive that still verifies), and the restore test only compares estimated counts. MongoRescue therefore checks what a connection reaches (`hello`, `msg: isdbgrid`) before it acts:

- A **backup** through a `mongos` fails before `mongodump` starts with `sharded clusters are not supported`, and so does a backup whose topology cannot be determined (the check fails closed).
- A **restore** into a `mongos` is refused by its preflight (check `topology`) with the same error; `"force": true` and dry runs do not override it.
- **Point-in-time streams** are refused on a `mongos` as on any server that is not a replica set, and `allow_mongos` does not change that.
- The connection setting **`allow_mongos`** (dashboard: *Allow backups through mongos*, API: `allow_mongos` on a connection; default off) accepts the risk. Backups through it carry the warning `sharded_unsupported`, which Recovery readiness reports as the reason `sharded_unsupported` (the row warns) for as long as the newest good backup has it, and restore preflights warn. Take such backups only when writes to the sharded collections are stopped and the balancer is off.

A connection test reports the server as `mongos` (`read_member.state`).

### Throttling

- **Upload cap.** `max_upload_mbps` (megabits per second) caps the stream to storage with a token bucket: per job, or for every job without one under Settings → General (`general.max_upload_mbps`). The dump slows down with it, since `mongodump` writes into the same pipe, so it also lowers the read rate on the member.
- **Parallel collections.** `num_parallel_collections` (1 to 16) is `mongodump --numParallelCollections`; `mongodump` dumps 4 collections at once by default, and 1 is the gentlest.
- **Concurrent backups per connection.** `max_concurrent_backups` on a connection limits how many backups (from all of its jobs, scheduled, on demand or manual) read from it at once; `0` is unlimited. Further backups wait in arrival order, shown as *Waiting for a slot* (`phase: "waiting"` in the active runs), and start as soon as one ends. Waiting does not count against the backup timeout, and a waiting backup can be cancelled.

### Backup windows

A job's **backup window** (`backup_window`: a time zone, the days it opens on and a start and end time) lets its scheduled runs start only within those hours; an end before the start closes the window the next day, so `22:00` to `02:00` on `sat` covers Saturday night into Sunday. Times follow the zone's daylight saving changes: a window opening at a time that the clock skips opens when the clock jumps.

- A scheduled run outside the window does not start. Consecutive skips share one *skipped* job run (`status: "skipped"`, `skip_reason: "outside window"`, `skipped_runs`), shown as "N scheduled runs skipped outside the window"; the first skip of each gap emits `backup.skipped`, which only rules that name it receive. A skip is neither a success nor a failure: it does not count in the failure counters, the heartbeat, the history or the job's last status.
- With `cancel_at_window_end` a scheduled run still going when the window closes is cancelled (recorded as cancelled by `system`, "the backup window ended"). Without it, the run finishes.
- Manual and on-demand runs ignore the window; the dashboard warns before starting one.
- The default RPO counts only the runs the window allows: an hourly job with a window of 01:00 to 05:00 runs at 01, 02, 03 and 04 o'clock, so its longest gap is 21 hours and its default RPO 43 hours.

### S3 multipart limits

Archives are uploaded to S3-compatible storage with multipart uploads, two parts at a time, so the archive is never buffered. S3 allows at most 10,000 parts per object, so the part size caps one archive (after compression and encryption). Each S3 target sets its part size (`s3.part_size_mb`, Settings → Storage → *Upload part size*), from 5 to 512 MiB:

| Part size | Largest archive | Upload memory (part size × 2) |
| :--- | :--- | :--- |
| 5 MiB | about 48.8 GiB | 10 MiB |
| 16 MiB (default) | about 156 GiB | 32 MiB |
| 64 MiB | about 625 GiB | 128 MiB |
| 128 MiB | about 1.2 TiB | 256 MiB |
| 512 MiB | about 4.9 TiB | 1 GiB |

The memory is per running upload, so multiply it by the number of backups that can run at once (jobs, and each connection's `max_concurrent_backups`) and leave room for it in the container's memory limit. The dashboard shows the largest archive next to the field. Raise the part size before a database grows into the limit:

- Before each backup to an S3 target, MongoRescue estimates the archive from the size of the database's last completed backup on the same connection, or from the server's `dbStats` data size (uncompressed) when there is none. Above **80%** of the target's largest archive the backup is recorded with a warning (`warnings` on the backup, shown in the backups list and the run log) and logged; it still runs. The job form's database preview shows the same warning for the selected target.
- An upload that still runs out of parts fails with an error that names the limit and the `part_size_mb` setting, not a raw SDK error.

Single objects are limited to 5 TiB by S3 anyway, and some S3-compatible services have lower limits on parts or objects. Compress (`gzip`), back up large databases per collection with several jobs, or use a local or file-system target (for example a mounted volume) for archives near that size, and see [Large databases](#large-databases) for when a logical dump is no longer the right tool.

## Sizing

MongoRescue streams every dump, so its memory does not grow with the size of the database; it grows with **how many streams run at once** and **how many collections each reads in parallel**. A container that is too small does not report that: the kernel kills it (`OOMKilled`, exit code 137) and the run shows up as a failed backup ([MongoRescueBackupFailed](runbooks.md#mongorescuebackupfailed)). Size it for the busiest moment, not the average.

What uses memory and CPU while a backup runs:

- **The MongoRescue process** (a Go program): the API, the scheduler, the metadata database cache and the run bookkeeping. Small and independent of the data size.
- **One `mongodump` per running backup**, a separate process in the same container: it reads `--numParallelCollections` collections at once (4 by default, 1 to 16 in the job; `0` keeps the default), each with buffers of its own, so more parallel collections mean more memory and more load on the member.
- **The S3 upload buffer**: every running upload holds part size × 2 in memory ([S3 multipart limits](#s3-multipart-limits)), 32 MiB with the default 16 MiB part.
- **Compression and encryption**: `gzip` and age each use about one core per stream while they run; CPU, not memory, limits how fast a compressed, encrypted backup goes.
- **A restore** runs `mongorestore`, with `--numParallelCollections` and `--numInsertionWorkersPerCollection` ([restore parallelism](configuration.md#restore-parallelism)); its memory grows with those too.

How many streams run at once is the sum of: the job `parallelism` (1 to 4 databases of a job run at once; every database of a job run is its own `mongodump`), the number of jobs whose schedules overlap, manual backups, restores and restore tests, the PITR collectors and base backups, and the copies (which read and write storage, not MongoDB). `max_concurrent_backups` on a connection caps the backups reading from one MongoDB deployment at once; the others wait in line.

Starting points by database size (the largest database a single backup reads, uncompressed), for **one backup stream at a time**. They come from the buffers above and are meant to be measured against your data, not trusted: run a backup and a restore test, watch `container_memory_working_set_bytes` and CPU throttling, and keep 30 percent of headroom.

| Largest database | Memory limit | CPU limit | Job `parallelism` | `num_parallel_collections` | S3 part size |
| :--- | :--- | :--- | :--- | :--- | :--- |
| up to 10 GB | 512 MiB to 1 GiB | 0.5 to 1 | 1 | default (4) | 16 MiB (default) |
| 10 to 100 GB | 1 GiB | 1 to 2 | 1 or 2 | 4 | 16 to 32 MiB |
| 100 to 500 GB | 2 GiB | 2 | 2 | 4 to 8 | 64 MiB |
| above 500 GB | 4 GiB or more | 2 to 4 | 2 | 4 to 8 | 128 MiB or more, and see [large databases](#large-databases) |

Scaling rules:

- **Memory.** Add the memory of every stream that can overlap: limit ≈ base (the table's lowest row) + (concurrent streams − 1) × (2 × part size + roughly 100 to 200 MiB for the `mongodump` or `mongorestore` process and its parallel collections). Doubling `num_parallel_collections` or `parallelism` roughly doubles the part of that term that belongs to `mongodump`.
- **CPU.** One core per concurrent compressed and encrypted stream, plus a core for the PITR collector. Below that, backups stretch and the lag of PITR streams grows ([MongoRescuePITRLagHigh](runbooks.md#mongorescuepitrlaghigh)); a CPU limit that throttles the process also delays the scheduler tick ([MongoRescueSchedulerStale](runbooks.md#mongorescueschedulerstale)). Prefer a CPU request that matches your steady use and a limit with room above it.
- **Part size.** The part size caps the largest archive (10,000 parts). Raise it before the archive reaches 80 percent of the cap, and count its buffer in the memory limit.
- **Throttles.** `max_upload_mbps`, a backup window and `read_preference: secondary` lower the load on MongoDB, not the memory of MongoRescue. A lower `num_parallel_collections` lowers both.
- **Disk.** The data volume holds `mongorescue.db` and the run logs (metadata and text, far smaller than the archives; the [metadata snapshot](#metadata-backups) needs the free space of a copy of the database while it runs), and the local backup target holds archives for the whole retention. `MongoRescueDiskFillingUp` watches both.

### Keeping the Go heap under the container limit

The Go runtime does not know the container limit. Without a soft limit it lets the heap grow until the collector decides to run, which can pass the limit between two collections. Setting `GOMEMLIMIT` makes the collector work harder as the heap approaches it. Set it **below** the container limit: `mongodump` and `mongorestore` are Go programs too and inherit the variable, and each applies it to itself, so neither the sum of the processes nor the non-heap memory (stacks, buffers, the page cache charged to the cgroup) is capped by it.

The Helm chart can compute it for you. Set `config.goMemLimitRatio` (default `0`, off) to a number between 0 and 1, and the chart sets `GOMEMLIMIT` to that share of `resources.limits.memory` in bytes at render time:

```yaml
resources:
  limits:
    memory: 2Gi
config:
  goMemLimitRatio: 0.7        # GOMEMLIMIT=1503238553 (70 percent of 2 GiB)
```

A ratio of 0.6 to 0.8 is a reasonable start. The chart refuses a ratio without a memory limit or with a limit it cannot read (use `Ki`, `Mi`, `Gi`, `Ti`, `k`, `M`, `G`, `T` or plain bytes), and does nothing when you set `GOMEMLIMIT` yourself in `extraEnv`. Outside Kubernetes set the variable by hand (`GOMEMLIMIT=1500MiB` for a 2 GiB limit; Docker: `-e GOMEMLIMIT=1500MiB --memory 2g`; systemd: `Environment=GOMEMLIMIT=1500MiB` with `MemoryMax=2G`). Kubernetes cannot derive a fraction of a limit through the downward API (`resourceFieldRef` returns the limit itself, not a share of it), which is why the chart does the arithmetic.

## Encryption

Use X25519 recipients (Settings → Encryption) rather than a passphrase: the instance taking backups then needs only the public key, and a compromise of that host or its storage credentials does not expose backup contents. Keep the private key on the host (or in the secret store) used for restores. See [encryption.md](encryption.md).

For restores into existing databases, keep the *Verify before restore* policy (Settings → General) at `auto` or `always` so a corrupted or undecryptable artifact is rejected before `mongorestore` touches the target.

## Key rotation

After a suspected leak, when someone who held a key leaves, or on a schedule, rotate from **Settings → Security → Key rotation**. The full runbook, with what each rotation changes, is in [encryption.md](encryption.md#key-rotation-runbook).

| What | How | Afterwards |
| :--- | :--- | :--- |
| `secret.key` | *Rotate secret.key* (your password; a second administrator under the two-person rule), or `POST /api/v1/security/rotate-secret-key` | Everyone signs in again. Download a new recovery kit; keep the previous one (or include `secret.key.previous`) for snapshots taken before the rotation. Those snapshots are deleted once the delete grace period has passed, then `secret.key.previous` too (after a new kit; otherwise a warning asks for one) |
| `MONGORESCUE_SECRET_KEY` | Not rotated by MongoRescue: move the key into `secret.key`, rotate, then set the variable to the new key ([procedure](encryption.md#keys-from-mongorescue_secret_key)) | As above |
| Backup encryption key | *Rotate encryption key*, optionally re-encrypting existing backups, or `POST /api/v1/encryption/rotate` | Escrow the new identity (new recovery kit); keep the old ones until no backup needs them |
| S3 credentials | *Test and rotate* with the new key pair, or `POST /api/v1/storage-targets/{id}/rotate-credentials` | Revoke the old key at the provider |
| Credentials from Vault | Rotate them in Vault: a new version of the KV v2 secret, or the database or AWS engine's own rotation (their leases end anyway) | Nothing in MongoRescue: static values are read again after `MONGORESCUE_VAULT_CACHE_TTL`, the PITR collector reconnects with them, and runs in progress keep their authenticated session |

A rotation of `secret.key` is crash-safe: a start after an interruption completes or rolls it back (logged), and never accepts a key the rotation did not name.

### Least-privilege storage credentials

Give MongoRescue a key limited to its bucket and prefix, with only the actions it uses: `s3:PutObject` (backups, snapshots, probes), `s3:GetObject` (restores, verification), `s3:ListBucket` (scans, retention, probes; restrict it with an `s3:prefix` condition) and `s3:DeleteObject` (retention, purges, re-encryption, probes), plus `s3:AbortMultipartUpload` and `s3:ListMultipartUploadParts` for large uploads. A credential rotation refuses a key that cannot write, read, list or delete. With object lock or versioning, deletions keep a version: size the bucket's lifecycle rules accordingly. Add a lifecycle rule that aborts incomplete multipart uploads after a day (`AbortIncompleteMultipartUpload`): a failed upload is aborted when the storage answers, but a backup cut off by a crash, a SIGKILL or a storage outage longer than the 30-second abort window leaves its uploaded parts behind; they are not an object and no backup refers to them, but they are billed until removed ([fault-injection tests](testing.md#fault-injection-suite)). Example AWS policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": "arn:aws:s3:::my-backups",
     "Condition": {"StringLike": {"s3:prefix": ["mongorescue/*"]}}},
    {"Effect": "Allow", "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject",
      "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"],
     "Resource": "arn:aws:s3:::my-backups/mongorescue/*"}
  ]
}
```

Use a separate key per installation, so revoking one never stops another.

#### Copy targets in another account

A copy only protects against a leaked key or a compromised account if that key or account cannot reach it. Put every copy target in **another account** (AWS account, Cloudflare account, MinIO tenant) than the primary, with **its own key** that MongoRescue alone holds:

1. Create the copy bucket in the second account, in another region than the primary, with **Object Lock (compliance mode) and versioning** enabled at creation.
2. Create a user (or role) in that account whose policy allows only what copies need on that bucket and prefix: `s3:PutObject`, `s3:GetObject` and `s3:ListBucket` (copies, verification, restores and drills), `s3:DeleteObject` (the purge, which Object Lock holds back until the retention ends), `s3:AbortMultipartUpload` and `s3:ListMultipartUploadParts` (large copies), plus `s3:GetBucketObjectLockConfiguration`, `s3:GetBucketVersioning`, `s3:PutObjectRetention` (and `s3:PutObjectLegalHold` with *legal hold on pin*) for Object Lock, and `s3:GetBucketLocation` so the target's region is detected. Never give it `s3:BypassGovernanceRetention`, `s3:PutBucketObjectLockConfiguration` or `s3:DeleteBucket`.
3. Do not reuse the primary's access key, and do not rely on the default credentials (instance profile, environment) for both: two targets without keys on one endpoint share the process's credentials.
4. Keep the second account's root and administrator credentials out of the place that runs MongoRescue, and out of region A.

The readiness report checks the part it can see: `dr_same_credentials` warns when every copy of a job uses the primary's access key ID, both use the default credentials on the same endpoint, or both are on the same Cloudflare R2 account (the account is part of the endpoint). It cannot see AWS account IDs or role trust, so a different key in the same account passes the check: the account split is yours to keep.

## Container images

Images are published to `ghcr.io/yigitcittan/mongorescue` for `linux/amd64` and `linux/arm64`. Tags:

| Tag | Meaning |
| :--- | :--- |
| `0.1.0` | Exact release (recommended) |
| `0.1` | Latest patch of a minor release (not moved by pre-releases) |
| `latest` | Newest stable release (never a pre-release) |
| `edge` | Current `main` branch, unreleased; changes without notice, for testing only |
| `sha-<commit>` | A specific commit; on a release commit it is the build that was promoted to the version tags |

Version tags appear only after a release succeeded: the release workflow builds the image from the tag, runs the rest of the release, and only then gives that image its `<version>`, `<major>.<minor>` and `latest` tags, right before it publishes the GitHub release. Pulling `latest` therefore never returns unreleased code. `edge` can carry a database schema that no release can open (a later downgrade fails with `ErrSchemaTooNew`), so do not point production data at it.

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
- **Pinned image.** `MONGORESCUE_VERSION` selects the image tag. The default is `latest`, the newest release; set it to an exact release such as `0.1.0` so a `docker compose pull` does not change the version unexpectedly.
- **Bind mounts.** The file uses named volumes, which get the right ownership automatically. If you replace them with host directories, create them owned by UID/GID `10001` first (see [Container images](#container-images)).
- **Locked-down container.** The service runs with `read_only: true`, a `tmpfs` on `/tmp` (where `mongodump` and `mongorestore` receive their temporary `--config` file), `cap_drop: [ALL]` and `no-new-privileges`. The only writable paths are `/data`, `/backups` and `/tmp`.
- **Back up the data volume.** The data volume (`/data`) holds the metadata database and `secret.key`. Back it up as described in [Data directory](#data-directory), for example with `docker compose stop mongorescue` before copying the volume.

- **Stopping during a backup.** `docker compose stop` gives the container 10 seconds by default, after which running backups are cancelled (recorded as cancelled, their partial archives removed). To let them finish, set `MONGORESCUE_SHUTDOWN_GRACE` (for example `9m`) and a longer `stop_grace_period` (for example `10m`); see [Graceful shutdown](kubernetes.md#graceful-shutdown).

A complete Compose file with Caddy, automatic HTTPS and the trusted-proxies setting explained is [examples/compose-caddy.yml](https://github.com/YigitCittan/mongorescue/blob/main/examples/compose-caddy.yml) ([recipe](deployment-recipes.md#caddy-with-automatic-https)).

To attach MongoRescue to a Compose project that already runs MongoDB, start from [examples/compose-existing-stack.yml](https://github.com/YigitCittan/mongorescue/blob/main/examples/compose-existing-stack.yml).

## systemd

The deb and rpm packages of every release ([README](https://github.com/YigitCittan/mongorescue/blob/main/README.md#install-options)) install the binary in `/usr/bin`, the unit `mongorescue.service` and the conffile `/etc/default/mongorescue`, and create the system user `mongorescue` with the state directory `/var/lib/mongorescue` (`0750`). The service is installed but not started:

```bash
sudo apt install ./mongorescue_<version>_linux_amd64.deb   # or: sudo dnf install ./mongorescue_<version>_linux_amd64.rpm
sudo systemctl enable --now mongorescue
sudo journalctl -u mongorescue | grep -i setup     # the one-time setup code
```

Then open http://127.0.0.1:8080. `/etc/default/mongorescue` (mode `0600`, kept across upgrades and removal) holds the bootstrap options as `KEY=value` lines: it enables the dashboard (`MONGORESCUE_DASHBOARD=true`) and listens on `127.0.0.1` only, because MongoRescue serves plain HTTP; put a [TLS reverse proxy](#tls-and-the-reverse-proxy) in front of it and set `MONGORESCUE_SERVER_HOST=0.0.0.0` only when you must expose it. `MONGORESCUE_SECRET_KEY` can be supplied from that file. The data directory is `/var/lib/mongorescue/data` and the default "Local disk" storage target `/var/lib/mongorescue/backups`; everything else is configured in the dashboard.

- **Database Tools.** The packages only recommend `mongodb-database-tools` (apt and dnf install it from [MongoDB's repositories](https://www.mongodb.com/docs/database-tools/installation/installation/); the distributions do not package it). `mongodump` and `mongorestore` must be on `PATH` or in `MONGORESCUE_TOOLS_DIR` ([details](configuration.md#mongodb-database-tools)).
- **Hardening.** The unit runs as `mongorescue` with no capabilities, `NoNewPrivileges`, `ProtectSystem=strict` (only `/var/lib/mongorescue` and a private `/tmp` are writable), `ProtectHome`, `PrivateDevices`, kernel and control-group protections, `RestrictAddressFamilies` (IPv4, IPv6, Unix), `MemoryDenyWriteExecute`, and a `@system-service` system call filter. A Local disk target outside `/var/lib/mongorescue` needs a drop-in (`systemctl edit mongorescue`): `[Service]` and `ReadWritePaths=/srv/backups`, owned by `mongorescue`.
- **Graceful stop.** To let running backups finish on `systemctl stop`, set `MONGORESCUE_SHUTDOWN_GRACE=9m` in `/etc/default/mongorescue` and `TimeoutStopSec=10min` in a drop-in ([graceful shutdown](kubernetes.md#graceful-shutdown)).
- **Upgrades** restart a running service; remove keeps `/var/lib/mongorescue`, the `mongorescue` user and the conffile (they hold the secret key). Back the directory up ([data directory](#data-directory)). Offline commands run as the service user: `sudo -u mongorescue mongorescue key status --data-dir /var/lib/mongorescue/data`.
- **Verification.** The release workflow installs the produced deb on Ubuntu and the rpm in Fedora, validates the unit with `systemd-analyze verify`, and starts the service as `mongorescue`; packages are covered by `checksums.txt`, SBOMs and cosign bundles ([verifying releases](security.md#verifying-releases)).

From an archive instead, install the binary, the unit and the user yourself: copy `scripts/mongorescue.service` to `/etc/systemd/system/`, change `ExecStart=` to the binary's path, run `useradd --system --home-dir /var/lib/mongorescue --shell /usr/sbin/nologin mongorescue`, and `systemctl enable --now mongorescue` (`StateDirectory=` creates and owns `/var/lib/mongorescue`; the `EnvironmentFile=` is optional).
