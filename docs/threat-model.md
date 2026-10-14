# Threat model

This page is the threat model of MongoRescue, component by component, using STRIDE (**S**poofing, **T**ampering, **R**epudiation, **I**nformation disclosure, **D**enial of service, **E**levation of privilege). For each component it names the assets, the trust boundaries, the threats, the mitigations that exist today with the code that implements them, the residual risk and the open items. It describes the code as it is; a mitigation that is not listed here does not exist.

Related pages: [security.md](security.md) (delete protection, the two-person rule, Object Lock, and [verifying releases](security.md#verifying-releases)), [architecture.md](architecture.md#security-boundaries) (security boundaries), [design/roles.md](design/roles.md), [design/oidc.md](design/oidc.md), [design/ha.md](design/ha.md), [design/pitr.md](design/pitr.md), [encryption.md](encryption.md). Report vulnerabilities as described in [SECURITY.md](../SECURITY.md).

## Scope and assumptions

In scope: the `mongorescue` server binary (HTTP API, dashboard, MCP, scheduler, engines), the CLI and MCP bridge, the storage drivers, the Helm chart and the container image, and the release pipeline. Out of scope: MongoDB itself, the S3 provider, the identity provider, the Kubernetes cluster, the desktop app's webview and OS integration, and the host operating system.

Assumptions:

- The host (or pod) running MongoRescue and its data directory (`mongorescue.db`, `secret.key`) are trusted. Whoever controls them controls MongoRescue ([what delete protection does not cover](security.md#what-this-does-not-protect-against)).
- TLS for the dashboard and API is terminated by a reverse proxy or ingress: the server itself only listens on plain HTTP (`internal/server/server.go`, `Server.Start`).
- MongoDB Database Tools (`mongodump`, `mongorestore`) on the `PATH` are trusted; the container image ships them from the Alpine package repository.
- Administrators are trusted not to collude; the [two-person rule](security.md#the-two-person-rule) limits what one compromised administrator can do.

## Actors

| Actor | Trust | Reaches MongoRescue through |
| :--- | :--- | :--- |
| Anonymous network client | None | The five public routes (`/api/v1/health`, `/api/v1/setup/status`, `/api/v1/setup`, `/api/v1/auth/login`, the sign-in methods), static dashboard files, `/metrics` when made public |
| Viewer, operator, administrator (local user or SSO) | Their role | Dashboard session |
| Automation (CI, scripts, CLI, MCP clients, AI assistants) | The API key's scope, capped by its creator's role and connections | `Authorization: Bearer` / `X-API-Key` |
| Storage provider and anyone with the bucket's or directory's credentials | Untrusted for integrity and confidentiality of archives | Backup archives, PITR chunks, metadata snapshots, the HA lease |
| MongoDB servers | Trusted for their own data; their output (names, oplog entries, tool output) is untrusted input | Driver connections and the tools' output |
| Notification receivers, identity provider | Untrusted network peers | Outbound HTTP, SMTP |
| Release consumers | Verify what they download | GitHub Releases, GHCR |

## Assets

- **Backups**: archives, PITR base backups and oplog chunks on storage, and their records (checksums, encryption mode, retention) in `mongorescue.db`.
- **Credentials**: MongoDB connection URIs, TLS client keys, S3 secret keys, notification secrets, the age identity or passphrase, all sealed with `secret.key` in the database; `secret.key` itself; password hashes, session and API key hashes. Connections and S3 targets with `credential_source: vault` store only a Vault reference: their credentials exist in MongoRescue's memory while they are used (component 6).
- **Source databases**: restores and post-restore commands write into MongoDB.
- **Control**: who can delete, restore in place, change retention, protections, users and keys.
- **Evidence**: the audit log and run logs.
- **Availability**: scheduled backups and PITR collection keep running.

## Trust boundaries

1. Network → HTTP listener (`internal/server`): every request is authenticated and authorised there.
2. MongoRescue → MongoDB (driver in `internal/mongoconn`, tools via `internal/mongotools`).
3. MongoRescue → storage (`internal/storage`): everything written there is assumed readable and writable by the storage's other principals.
4. MongoRescue → outbound HTTP/SMTP (notifications, SSO): the destination is untrusted, and the request must not be steerable into the host's own network services.
4a. MongoRescue → HashiCorp Vault (`internal/vault`, for key wrapping and credentials): the address is a bootstrap setting, never request input; requests carry the Vault token, so they never follow a redirect and need `https` unless an operator opts out for development.
5. Process → host: the data directory, temporary files and child processes.
6. Release pipeline → users: archives, images and their signatures.

---

## 1. HTTP API and dashboard

**Assets**: every API operation and the data it returns; the browser session of dashboard users.
**Trust boundary**: network to `internal/server`.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| S | Calling the API without credentials | `authMiddleware` (`internal/server/auth.go`) requires a session or API key for everything under `/api/` except `publicPaths` (exactly five routes, `TestPublicRoutesAreExactlyTheseFive`); `/mcp` accepts API keys only; `/metrics` needs a key unless `security.metrics_public`. |
| S | Login CSRF and cross-site POSTs to the public routes | `allowPublicWrite` (`internal/server/auth.go`) requires a JSON body and a same-origin or allowed `Origin`. |
| T | Cross-site request forgery with a session | `auth.CheckCSRF` (`internal/auth/service.go`): unsafe methods with a session need the session's CSRF token (constant-time compare); session cookie `SameSite=Strict`, `HttpOnly` (`sessionCookie`, `internal/server/auth.go`). |
| T | Clickjacking, XSS, content sniffing | `securityHeadersMiddleware` (`internal/server/server.go`): CSP `default-src 'self'` without inline scripts, `frame-ancestors 'none'`, `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: no-referrer`. |
| T | Cross-origin reads | `corsMiddleware`: off by default; an allow-list of exact origins, never `*`. |
| R | Denying an action | Every authenticated mutating request and every refused one is recorded in the hash-chained, append-only audit log (`auditsAction`, `internal/server/auditlog.go`; triggers in `internal/store/migrations/0017_audit_events.sql`; see [audit.md](audit.md)). |
| I | Credentials in responses, logs or errors | URIs go through `internal/redact`; other secrets are masked in API responses; client-supplied values go through `internal/logsafe` before logs. API, MCP and metrics responses carry `Cache-Control: no-store`. |
| I | Spoofed client IPs in logs and throttling | `clientIP` honours `X-Forwarded-For` only with `security.trust_proxy_headers`. |
| D | Slow or huge requests | `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout` on the server (`internal/server/server.go`); `http.MaxBytesReader` on JSON bodies (`maxAuthBody`, `maxBulkBody`, `maxRotateBody`, …). |
| E | A route without authorisation | One route → scope table (`routeScopes`, `internal/server/scopes.go`); an unlisted pattern requires admin (`requiredScope`); `TestEveryRouteHasAScope`, `TestScopesAreEnforcedForEveryRoute`. A standby instance serves no data (`standbyGate`, `internal/server/ha.go`). |

**Residual risk**: plain-HTTP deployments send credentials in the clear (the session cookie is `Secure` only over TLS or behind a trusted proxy, `secureRequest`). Rate limits (#191) bound requests per API key and session (a general budget and a tighter one for routes that start work; MCP keeps its own per-key limit) and unauthenticated requests per client address (`allowRate`, `internal/server/ratelimit.go`; token buckets in `internal/ratelimit`), but they are kept in memory per instance and only slow a leaked key down.

**Open items**: HSTS when the server knows it is behind TLS.

## 2. Authentication: sessions, API keys, SSO, CSRF, throttling

**Assets**: password hashes, session tokens, API keys, the setup code, role and connection assignments.
**Trust boundary**: network to `internal/auth` (through `internal/server`); identity provider to `internal/auth/oidc`.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| S | Password guessing | Per (IP, username) throttling with lockouts and in-flight caps, reserved before the password check (`internal/auth/throttle.go`); bcrypt hashes (`internal/auth/service.go`). |
| S | User enumeration by timing | Unknown users are compared against `dummyHash`, so every login costs one bcrypt comparison (`internal/auth/service.go`). |
| S | Session or key theft from the database | Session tokens (256 bits) and generated API keys (160 bits) come from `crypto/rand` and are stored as SHA-256 hashes; the key imported from `MONGORESCUE_API_KEY` as an HMAC-SHA256 under an HKDF subkey of `secret.key` (`internal/auth/credentials.go`). |
| S | Taking over a fresh instance | Setup mode until the first user exists, which needs the one-time setup code printed in the logs (`Service.Init`, `Service.Setup`). |
| S | Forged SSO sign-in | Authorization code flow with S256 PKCE, a nonce and a single-use state in a sealed `SameSite=Lax` flow cookie (`sealFlow`, `internal/server/oidc.go`); ID tokens verified by go-oidc with RS256/ES256 only plus `azp`, `iat`, `nbf` (`internal/auth/oidc/oidc.go`); users matched by issuer and subject, never by e-mail. |
| T | CSRF | See component 1. |
| R | Unattributed actions | Sessions and keys name their user (`CreatedBy`); the audit log records the principal and, for approvals, requester and approver. |
| I | Provider responses abused for SSRF or memory exhaustion | All provider traffic goes through the guarded notification client (`notify.NewHTTPClient`, wired in `internal/app/app.go`) with responses capped at `MaxResponseBytes` (1 MiB). |
| D | Lockout of a legitimate user | The per-IP failure count only tightens the budget; a correct password from an unlocked pair still succeeds (`internal/auth/throttle.go`). |
| E | A key outliving its creator's rights | A key's effective scope and connections are capped by its creator's current role and connections on every request (`EffectiveScope`, `internal/auth/types.go`; [design/roles.md](design/roles.md)); deleting a user revokes their keys. |
| E | One stolen admin credential doing damage | The two-person rule (`internal/auth/approval.go`, [security.md](security.md#the-two-person-rule)); a password reset by someone else forces a new password (`PasswordChangeRequired`). |

**Residual risk**: local accounts have no second factor; an API key without an expiry (allowed while no maximum lifetime is set) stays valid until it is revoked. API keys can expire and a maximum lifetime can be enforced since #190 (`APIKeyPolicy`, `internal/auth/expiry.go`; `security.api_key_max_lifetime_days`). Whoever controls the identity provider's groups controls SSO roles.

**Open items**: MFA for local accounts (until then, use SSO with MFA at the provider and keep local administrators to the break-glass account).

## 3. MCP

**Assets**: the same operations as the REST API, reachable by AI assistants and agents.
**Trust boundaries**: network to `/mcp` (`internal/server/mcp.go`, `internal/mcp`); the stdio bridge (`mongorescue mcp`) to a running instance; tool output to the model.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| S | A web page driving a local MCP endpoint (DNS rebinding, cross-origin) | `mcpGuard` (`internal/server/mcp.go`): MCP can be turned off, a present `Origin` must be same-origin or allowed, and a request that arrived on loopback must name a loopback host unless proxy headers are trusted. Sessions are never accepted on `/mcp`. |
| T | An assistant deleting or overwriting data | No tool deletes, unpins, approves, restores in place or reconfigures; restores go into fresh `<db>_rescue_<timestamp>` clones; `pitr_restore` never sets the in-place options (`pitrRestoreRequest`, `internal/mcp/pitr.go`). |
| R | Unaudited tool calls | Every call is audited with redacted arguments and counted (`internal/mcp/middleware.go`). |
| I | Secrets in tool results | Tools call the same application services as the REST API (`internal/operations`, `internal/connections`, `internal/targets`), which return records with URIs redacted and secrets masked. |
| D | A runaway agent | Per-key rate limit, 60 calls per minute with a burst of 20 by default (`DefaultRateLimit`, `internal/mcp/ratelimit.go`). |
| E | Calling tools above the key's scope | `tools/list` shows only the tools the key may call, and the scope is checked again on every call (`internal/mcp/server.go`, `internal/mcp/middleware.go`); `pitr_restore` needs admin. |

**Residual risk**: database, collection and job names reach the model in tool results and could carry prompt-injection text; an agent with an operator key can still start backups and safe-clone restores that consume storage and MongoDB resources within its rate limit.

**Open items**: none beyond the REST API's.

## 4. CLI and API client

**Assets**: the API key on the operator's machine.
**Trust boundary**: the user's machine to the server (`internal/cli`, `internal/apiclient`).

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| S | Key sent to another host (redirect, wrong origin) | The bearer transport authenticates only requests to the configured origin and the client never follows redirects (`internal/apiclient/client.go`, `internal/apiclient/transport.go`). |
| I | Key visible in the process list or shell history | Keys come from `--api-key-file`, `MONGORESCUE_CLI_API_KEY_FILE` or `MONGORESCUE_CLI_API_KEY`; `--api-key` is documented as discouraged and used only when nothing else is set (`internal/cli/cli.go`); key files are read up to 4 KiB (`internal/cli/session.go`). |
| I | Key sent in clear text | A warning when the URL is plain HTTP to a non-loopback host (`session.connect`). |
| I | URLs with credentials in error messages | Transport errors are reported without the request URL, redacted (`transportMessage`, `internal/apiclient/client.go`). |
| T, R, D, E | — | The server enforces scope, audit and limits; the CLI holds no authority of its own. |

**Residual risk**: plain HTTP to a remote host is warned about, not refused.

**Open items**: none.

## 5. Storage drivers (local, S3, Object Lock)

**Assets**: archives, chunks, snapshots, the HA lease; S3 credentials.
**Trust boundary**: MongoRescue to the storage and its other principals.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| T | Path traversal through object keys | `resolvePath` (`internal/storage/local.go`) refuses backslashes, drive letters, control characters and non-local paths (`filepath.IsLocal`); fuzzed by `FuzzLocalKey`. |
| T | Archive replaced or corrupted on storage | Each record keeps the SHA-256 of the stored bytes; a verified restore checks it before `mongorestore` starts and every restore checks it while streaming (`ErrChecksumMismatch`, `internal/restore/engine.go`); integrity sweeps re-read archives (`internal/integrity`). The recorded checksum is the only authenticity check: age detects corrupted or truncated ciphertext, but anyone with an X25519 recipient can produce a valid encrypted archive (component 7). |
| T | Deletion through MongoRescue | Soft deletes with a grace period, pins, the two-person rule ([security.md](security.md#delete-protection)). |
| T | Deletion on the storage side | S3 Object Lock (`internal/storage/s3_lock.go`): every upload carries the target's retention; compliance mode resists every principal; a target with a lock mode is refused unless the bucket has Object Lock and versioning; lowering a lock waits for the grace period. |
| T | Two targets overwriting each other | Overlapping locations are refused and the purge checks aliases ([security.md](security.md#storage-targets)). |
| I | Archive contents read on storage | Optional age encryption (component 7); PITR requires it (`ErrEncryptionRequired`, `internal/pitr/collector/collector.go`). Local files are written `0640` in `0750` directories (`internal/storage/local.go`). |
| I | S3 credentials at rest | Sealed with `secret.key` (component 6), or not stored at all: a target with `credential_source: vault` keeps only a Vault reference and its driver reads the keys through an AWS credentials cache that asks Vault again before they expire (`TargetFactoryWith`, `internal/storage/target.go`; `S3Provider`, `internal/vaultcreds/s3.go`), never falling back to the default credential chain. |
| D | A full data directory, stalled writes | `internal/diskguard` refuses new runs while the data directory is short of space; a stall watch on local writes (`WithStallWatch`). |
| E | — | The drivers act only for MongoRescue's own requests. |

**Residual risk**: without Object Lock (and always on local targets), anyone with the storage's credentials or file system access can delete archives. S3 endpoints may be plain `http://` (`internal/targets/targets.go`), in which case unencrypted archives travel in clear text. An administrator can point a target at any endpoint, including internal services; target tests then connect there.

**Open items**: an option to require `https` S3 endpoints; applying the notification destination policy to S3 endpoints.

## 6. secretbox, secret.key and key rotation

**Assets**: `secret.key` and every credential it seals.
**Trust boundary**: the data directory and the database file.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| T | A sealed value moved into another record or field | AES-256-GCM with associated data binding table, record ID and field (`Binding`, `internal/secretbox/secretbox.go`); reads refuse unsealed and legacy values once migrated (`ErrUnsealedSecret`). |
| T | A wrong or missing key silently used | A key check value makes startup fail with `ErrSecretKeyMismatch`. |
| I | Key file readable by others | Generated with mode `0600`, written atomically with fsync (`LoadKey`, `internal/secretbox/keyfile.go`); looser permissions are logged as a warning. |
| I | A leaked key | Rotation (`internal/keyrotation`; `POST /api/v1/security/rotate-secret-key`, a signed-in administrator who confirms their password, API keys refused, an approval first with the two-person rule; `handleRotateSecretKey`, `internal/server/keyrotation.go`) re-seals every value in one transaction, signs out every session, and recovers from a crash at any step; snapshots sealed with the old key are deleted after the grace period and `secret.key.previous` after them once a new recovery kit was downloaded ([encryption.md](encryption.md)). |
| I | The recovery kit (which contains `secret.key`) downloaded with a stolen key | `POST /api/v1/recovery-kit` needs a signed-in administrator who confirms their password; API keys are refused; the kit is sealed with a passphrase from the request (`handleDownloadRecoveryKit`, `internal/server/recovery.go`). |
| R | Unrecorded rotation | Rotations are announced durably and audited. |
| I | A copy of the data directory or volume (a backup, a snapshot, a stolen disk) | Key wrapping (#61, [security.md](security.md#wrapping-secretkey-with-a-kms)): with `MONGORESCUE_KEY_WRAP` the data directory holds only `secret.key.wrapped`, which AWS KMS, Google Cloud KMS, Azure Key Vault or Vault Transit unwraps at every start with the instance's own identity (`Load`, `internal/keywrap/load.go`; providers in `internal/keywrap/provider`); the unwrapped key is checked against the recorded fingerprint (`File.Open`, `internal/keywrap/file.go`), never falls back to a plaintext file, and rotated keys are wrapped before they are written (`internal/keyrotation`). A recovery kit with the plaintext key needs an approved `plaintext_recovery_kit` request under the two-person rule (`internal/server/recovery.go`). |
| D | Crash mid-rotation | The marker and the `secret.key.next` / `.previous` files let `keyrotation.Open` settle it at the next start. |
| I | Connection and S3 credentials at rest (with the key, a copy of the volume opens them) | Credentials from Vault (#62, [security.md](security.md#credentials-from-hashicorp-vault)): a connection or S3 target with `credential_source: vault` stores only a reference (mount, path or role, field names; `models.VaultRef`), and the credentials are read when used (`connections.Service.Resolve` and `Hold`, `internal/connections/vault.go`; `internal/vaultcreds`). Nothing read is stored, logged or returned: the store saves the URI without user information (`mongouri.HasUserinfo` refuses one), test results are recorded from the stored connection, errors name the Vault path and field only, and `POST /api/v1/vault/test` reports field names and lease TTLs. Static values are cached in memory only (`MONGORESCUE_VAULT_CACHE_TTL`); dynamic leases (database, AWS engines) are renewed while a run holds them and revoked when it ends or MongoRescue stops; a lease that cannot be kept valid ends the run (`ErrLeaseLost`) and the PITR collector reconnects (`collector.StaleSession`). Vault failures fail the use without a fallback and raise `vault.unavailable` to every channel. |
| E | — | — |

**Residual risk**: without key wrapping, `secret.key` next to `mongorescue.db` on the same volume means one copy of the volume holds both; with it, whoever can use the KMS key with an instance's identity, or read the running process's memory, still gets the key. Keys from `MONGORESCUE_SECRET_KEY` are never rotated by the application. With credentials from Vault, the credentials are in the process's memory while used and whoever holds the instance's Vault identity can read what MongoRescue reads; Vault becomes a dependency of every run that uses it. Connections and targets that keep `credential_source: stored`, notification secrets and TLS client keys stay sealed with `secret.key`.

**Open items**: none; the former gap "no external key provider (KMS) for `secret.key`" is mitigated by key wrapping (#61).

## 7. age encryption

**Assets**: archive and chunk contents; the age identity or passphrase.
**Trust boundary**: storage (ciphertext) to MongoRescue (keys).

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| T | Modified ciphertext | age detects modification of every 64 KiB chunk and of the header under the file key (integrity, not sender authenticity: see the S row below); failures are `ErrDecryptionFailed` (`internal/encryption/encryption.go`); fuzzed by `FuzzDecrypt`, `FuzzRoundTrip`. |
| I | Reading archives on storage | X25519 recipients (the backup server needs no private key) or scrypt passphrases; identities and passphrases are sealed in the database. |
| I | Key material in errors | Errors name recipients and identities by position only. |
| D | A crafted header forcing an expensive scrypt | `MaxScryptWorkFactor` (18) caps decryption as well as encryption. |
| S | An archive forged with the public recipient | Anyone with an `age1…` recipient can encrypt; restores accept an archive only when its SHA-256 matches the record in the database (component 5). |
| R, E | — | — |

**Residual risk**: retired keys are kept so old backups stay restorable; an archive imported from a storage scan has no earlier checksum to compare with.

**Open items**: none.

## 8. Backup and restore engines (mongodump and mongorestore)

**Assets**: source databases, connection URIs, the archive stream.
**Trust boundaries**: MongoRescue to child processes; storage (archives) to `mongorestore`; MongoDB to the tools.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| T | Shell injection through names or URIs | Tools are started with `exec.CommandContext` and argument slices, never a shell (`internal/mongotools/process.go`); namespace patterns are escaped (`escapeNamespace`, `internal/restore/engine.go`); restore namespaces fuzzed by `FuzzPrepareNamespaces`. |
| I | URI in the process list | The URI goes into a `0600` tools config file (`WriteURIConfig`, `internal/mongotools/config.go`), never `--uri`; control characters are refused; fuzzed by `FuzzWriteURIConfig`. |
| T | Overwriting production by mistake | Restores default to `<db>_rescue_<timestamp>` clones; in-place needs `safe_clone: false` and `confirm_in_place: true` (`ErrInPlaceNotConfirmed`, `internal/models/restore.go`); `drop_target` needs the two-person rule when on; users and roles are restored only in place and only on request (`buildRestoreArgs`). |
| T | A tampered archive fed to `mongorestore` | Checksum verification (component 5); the archive prelude is read with a size limit (`ReadArchivePrelude`, `internal/mongotools/archive.go`; fuzzed by `FuzzReadPrelude`). |
| T | Post-restore commands escaping the clone | An allow-list of single-collection data commands, no server-side JavaScript, no database names, and only clones the restore created (`internal/postrestore`). |
| D | Memory exhaustion | Archives stream through pipes; nothing buffers a dump. Runs are bounded by timeouts and concurrency keys. |
| R | — | Runs, their logs (`internal/runlog`, redacted) and their actors are recorded. |
| E | A viewer or operator restoring in place | In-place restores need admin (`TestInPlaceRestoreNeedsAdmin`). |

**Residual risk**: `mongorestore` parses archives that came from storage; an archive that passes the checksum is trusted. The tools config file exists on disk (mode `0600`) for the duration of a run.

**Open items**: none.

## 9. PITR collector

**Assets**: the oplog of each stream's replica set (all writes, including documents), chunks and chains.
**Trust boundaries**: MongoDB to the collector (`internal/pitr/collector`); collector to storage.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| I | Oplog contents on storage | PITR refuses to run without backup encryption (`ErrEncryptionRequired`); chunks go through scanner, gzip, age and SHA-256 (`internal/pitr/collector/chunk.go`). |
| T | Malformed or hostile oplog data | The oplog codec validates framing and contents (`internal/oplog`; fuzzed by `FuzzScanner`, `FuzzFilter`, `FuzzArchiveWriter`). |
| T | Chunks replaced or removed on storage | Each chunk's SHA-256 is committed with the collector's position; gaps and divergences end the chain; verification re-reads chunks (`internal/pitr/collector/verify.go`). |
| D | Retention deleting what a restore needs | Running restores pin their base and chunks (`internal/pitr/pins.go`). |
| S, R, E | — | Streams are configured by administrators; runs are audited. |

**Residual risk**: the connection used by the collector needs read access to `local.oplog.rs`, which exposes every write on the replica set to MongoRescue.

**Open items**: none.

## 10. In-place point-in-time restore

**Assets**: the original databases being replaced.
**Trust boundary**: an administrator's request to production data.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| T | Replacing production by mistake or with a stolen credential | Admin only, every replaced database confirmed (`ErrPITRInPlaceNotConfirmed`); with the two-person rule, a second administrator must approve (`pitr_in_place`); not offered over MCP (`internal/operations/pitrinplace.go`). |
| T | Losing the current data | The restore goes into clones first; a safety backup of every original is taken before the swap; a failed swap keeps the clones and the safety backups (`finishPITRInPlace`). |
| R | — | Starting publishes `security.destructive_action` and logs who started and approved it (`inPlaceStarted`). |
| D | Concurrent runs during the swap | Concurrency keys for every replaced database and the connection (`acquireInPlace`); the swap is bounded by `MaxSwapDuration` and a shutdown waits for it. Refused in HA mode (`ErrInPlaceHAMode`). |
| S, I, E | — | Covered by components 1 and 2. |

**Residual risk**: two colluding administrators can replace databases; writes between the safety backup and the swap are lost unless the application is stopped.

**Open items**: none.

## 11. HA lease and metadata handover

**Assets**: the lease (who is active), metadata snapshots (the whole database, credentials sealed).
**Trust boundary**: instances to the HA storage target.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| S | Two active instances | Compare-and-swap writes of the lease (`If-None-Match`/`If-Match`, `internal/storage/lease_s3.go`; a lock file locally), an epoch fencing token, and a fencing check before any run starts or completes (`Manager.Check`, `internal/ha/manager.go`). |
| I | Snapshots read on storage | age encryption with a passphrase derived from `secret.key` (`SnapshotPassphrase`, `internal/ha/snapshot.go`); credentials inside stay sealed. The lease holds only an HKDF key ID. |
| T | A forged snapshot | Only a holder of `secret.key` can produce a snapshot that decrypts; an instance with another key refuses to start. |
| T | A genuine snapshot replayed under a higher epoch, a later time or another cluster | Each snapshot starts with an authenticated header (`format`, `cluster`, `epoch`, `instance_id`, `created_at`) inside the age plaintext (`SnapshotHeader`, `parseSnapshotHeader`, `verify`, `internal/ha/snapshotheader.go`); every decision (takeover, epoch floor, retention, startup check) verifies it against the object key and the cluster and refuses a mismatch (`ErrSnapshotRejected`, `SnapshotChecker`), alerted by `mongorescue_ha_snapshots_rejected_total` and `mongorescue_ha_refused_snapshots`. Fuzzed (`FuzzParseSnapshotHeader`). |
| T | Metadata rolled back by deleting snapshots or replacing the lease | The lease carries an HMAC-SHA256 with an HKDF subkey of `secret.key` (domain `ha-lease`) over cluster, epoch, holder and the last snapshot the active instance wrote (`LeaseMACKey`, `Lease.Authentic`, `internal/ha/lease.go`; `Manager.RecordSnapshot`, `Manager.Confirm`, `internal/ha/manager.go`). A takeover refuses an untrusted or missing lease, a replayed or stale lease, and a newest verified snapshot older than the lease or the instance's high-water mark in `ha.json` (`prepareTakeover`, `internal/app/ha_handover.go`; `mongorescue_ha_rollback_suspected_total`). A lease without a MAC is trusted only while the cluster has no snapshot with a header (rolling upgrade). Only an operator with the data directory forces a takeover past this (`mongorescue ha force-takeover`, audited). |
| D | Someone holding or corrupting the lease | Anyone who can write the lease prefix can stop the cluster ([design/ha.md](design/ha.md#security)), also by deleting or altering the HA objects so that every takeover is refused; alerts fire on takeover failures and suspected rollbacks. |
| E | A standby serving stale data | A standby serves no data and accepts no sign-in (`standbyGate`). |
| R | — | Step-downs and takeovers are logged, counted and notified. |

**Residual risk**: fixed in [#189](https://github.com/YigitCittan/mongorescue/issues/189): the epoch and cluster of a snapshot were read from its object key only, so a principal with write access to the HA prefix (but without `secret.key`) could copy an older snapshot under a higher epoch, or another cluster's snapshot that uses the same key, and a takeover restored it. They are now authenticated inside the encrypted content (`internal/ha/snapshotheader.go`), and the lease MAC with the last snapshot detects deleted snapshots and replaced leases (see the table and [design/ha.md](design/ha.md#rollback-protection)). What remains: that principal can still keep the cluster from having an active instance (every takeover refused until an operator forces one), and a fresh instance with no HA history that finds the HA target emptied starts from its own database (alerted: `MongoRescueHAStartedFromEmptyState`, `ha.empty_start`).

**Open items**: separate credentials for the HA prefix.

## 12. Notifications and webhooks

**Assets**: channel secrets (webhook URLs and HMAC secrets, bot tokens, SMTP and Twilio credentials); the network the server can reach.
**Trust boundary**: MongoRescue to receivers.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| S | Forged webhook deliveries to receivers | `X-MongoRescue-Signature: sha256=<HMAC-SHA256(secret, body)>` when a secret is set; payloads carry an event ID for de-duplication (`internal/notify/webhook.go`). |
| I | SSRF into metadata services and link-local networks | The destination policy (`internal/notify/destination.go`) blocks link-local, metadata (also via NAT64), unspecified, multicast and broadcast addresses and non-canonical numeric hosts, at save time and again after DNS resolution on every connection, also for the target of a proxied request; the client never follows redirects and times out (`internal/notify/http.go`); fuzzed by `FuzzDestinationPolicy`, `FuzzWebhookHostForms`, `FuzzValidateWebhook`. |
| I | SMTP credentials in clear | `AUTH PLAIN` only over TLS or to localhost (`internal/notify/email.go`). |
| I | Secrets at rest or in the API | Sealed in the database and masked in responses. |
| D | Slow receivers blocking work | Deliveries go through a durable outbox with retries (`internal/notify/outbox.go`) and a per-attempt timeout. |
| E | A non-admin creating channels to exfiltrate events | Channels and rules need admin (`routeScopes`). |

**Residual risk**: loopback and private addresses stay allowed on purpose (self-hosted receivers), so an administrator can make the server send requests to internal services.

**Open items**: an option to deny private ranges for deployments that do not need them.

## 13. Helm chart and container image

**Assets**: the pod, its volume (`mongorescue.db`, `secret.key`), the Kubernetes Secret with the key.
**Trust boundary**: the cluster and its operators.

| | Threat | Mitigations (code) |
| :--- | :--- | :--- |
| E | Container breakout, privilege escalation | Non-root UID 10001 (`Dockerfile`), `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, dropped capabilities and the `RuntimeDefault` seccomp profile, no service account token (`deploy/helm/mongorescue/values.yaml`). |
| I | `secret.key` on the data volume | `keyWrap` (key wrapping, component 6) keeps only `secret.key.wrapped` on the volume; `secretKey.existingSecret` takes the key from a Secret managed elsewhere; by default a random key is generated into a Secret that `helm uninstall` keeps (`templates/secret.yaml`). |
| I, T | Lateral movement | An optional NetworkPolicy for ingress and egress (`templates/networkpolicy.yaml`), off by default. |
| T | A tampered image | Images are built with BuildKit provenance and SBOM, attested with GitHub artifact attestations, and signed with cosign by digest ([verifying releases](security.md#verifying-releases)). |
| D | — | Resource requests and limits are configurable. |
| S, R | — | — |

**Residual risk**: the NetworkPolicy is off by default; the Secret is only as protected as etcd and the cluster's RBAC; the tools in the image come from the Alpine package repository at build time.

**Open items**: none in the chart; enable the NetworkPolicy and encryption at rest for Secrets in the cluster.

## 14. Release pipeline and parsers (cross-cutting)

**Assets**: what users download and run.

- **Signed releases**: release archives, SBOMs, `checksums.txt` and the desktop assets carry keyless cosign signatures (`<asset>.sigstore.json`); release archives and `checksums.txt` have SLSA Build Level 3 provenance from `slsa-framework/slsa-github-generator` next to the GitHub artifact attestations; container images are signed by digest ([verifying releases](security.md#verifying-releases)).
- **Fuzzing**: every parser of untrusted input has a native Go fuzz target (`FUZZ_TARGETS` in the `Makefile`): connection strings and their redaction, the tools config, sealed values, age ciphertext, the archive prelude, the oplog codec, PEM and PKCS#8 TLS keys (`internal/mongotls`), the HA storage location (`internal/config`), the HA snapshot header (`internal/ha`), notification channels, cron, storage keys, restore namespaces and the desktop updater's archives. They run nightly with Go's engine (`make fuzz`) and with ClusterFuzzLite on pull requests and in daily batch runs (`.github/workflows/cflite.yml`, [testing.md](testing.md)).
- **Dependencies**: Dependabot updates modules, actions and base images; `govulncheck`, CodeQL and the OpenSSF Scorecard run in CI.

## Open items

| Component | Item |
| :--- | :--- |
| HA | Separate credentials for the HA prefix (binding the snapshots' cluster and epoch and detecting rollbacks is done: [#189](https://github.com/YigitCittan/mongorescue/issues/189)). |
| HTTP API | HSTS when served over TLS (REST rate limits are done: [#191](https://github.com/YigitCittan/mongorescue/issues/191)). |
| Auth | MFA for local accounts (API key expiry is done: [#190](https://github.com/YigitCittan/mongorescue/issues/190)). |
| Storage | An option to require `https` S3 endpoints; the destination policy for S3 endpoints. |
| Notifications | An option to deny private ranges. |
| Release | SLSA Build Level 3 provenance for the container images (the images have Level 2 attestations and cosign signatures). |
| Process | An independent security review of auth and RBAC, the two-person rule, secretbox and key rotation, PITR restore and Object Lock (issue #144). |
