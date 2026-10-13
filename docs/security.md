# Security: delete protection and key wrapping

MongoRescue holds the means to destroy your backups: an administrator, or anyone with a stolen administrator session or API key, can delete backups, shorten retention and delete or repoint storage targets. Ransomware operators look for exactly this before they encrypt production. Delete protection makes sure that **nothing done through MongoRescue makes a backup unrecoverable faster than the delete grace period**, and optionally that **no single administrator can finish a destructive action alone**.

This page is the threat model. Reporting vulnerabilities is described in [SECURITY.md](../SECURITY.md); the API is in [api.md](api.md#delete-protection) and the settings in [configuration.md](configuration.md#security).

`secret.key`, the root key of every stored credential, can be [wrapped by an external KMS](#wrapping-secretkey-with-a-kms) so that the data directory never holds it in plain form.

Delete protection keeps data longer on purpose, which works against erasure requests: [privacy.md](privacy.md#backups-and-erasure-requests) shows what each artefact holds, how long the grace period and retention keep it, and how to re-apply erasures after a restore.

## Delete protection

### Soft deletes and the grace period

Deleting a backup (`DELETE /api/v1/backups/{id}`, a bulk delete or the dashboard) does not touch storage. The record moves to the status `deleted` with `deleted_at`, `deleted_by`, an optional `delete_reason` and `purge_after`, the end of the delete grace period (`security.delete_grace_days`, default 7, 1 to 90 days). The archive stays where it is.

- Deleted backups are hidden from the backup lists (`?status=deleted` or `?deleted=true` shows them; the dashboard's **Deleted** view), cannot be restored, verified, pinned or deleted again, and keep their run log.
- `POST /api/v1/backups/{id}/undelete` (admin) restores the status the backup had before (completed, failed, missing, …). The dashboard's **Undo delete** does the same.
- The scheduler purges every ten minutes: a deleted backup whose grace period has ended, and that is not pinned, loses its archive (unless another record still holds the same archive) and becomes `purged`. A purge that cannot delete the archive leaves the backup `deleted` and retries. Each purge is audited (`SYSTEM backup.purge`) and published as a `security.destructive_action` event.
- Raising the grace period protects deletions made before it: a backup is purged only when both its recorded `purge_after` and `deleted_at` plus the grace period in force have passed.
- Retention deletes the same way: backups its policy selects become `deleted` (`deleted_by: retention`) and are purged after the grace period, so a retention run never frees storage at once either.
- Storage scans treat the archive of a deleted backup as owned: it is never reported as an orphan, never marked missing and cannot be imported as a new backup. The archive of a purged record that is still there (deleted by hand, or the purge failed) is an orphan again.
- Pins still block deletion (`409`): unpin first (with the two-person rule, that needs an approval).

### Lowering a protection takes the grace period too

A lower protection is stored as a pending change with an `effective_at` time, the end of the grace period in force when it was requested, and the scheduler applies it then. Until it applies, the stronger protection stays in force:

- **Shorter retention** of a job: fewer days, fewer backups, or a rule where there was none (`0` is "keep forever"). `PUT /api/v1/jobs/{id}` (and `POST /api/v1/jobs`) store the job with its current retention and answer with `pending_retention`. A longer retention applies at once and cancels a pending shortening. A job created under the ID of a deleted job that still names backups counts as keeping them forever.
- **A shorter grace period** (`security.delete_grace_days`). A longer one applies at once and cancels a pending lowering.
- **Locked copies no longer required** (`security.require_locked_copies` turned off, kind `disable_locked_copies`, or a job's `require_locked_copies` turned off while the setting is off, kind `job_locked_copies`; with the two-person rule, an approval first). Turning it on applies at once and cancels a pending change. See [locked copies](production.md#locked-copies).
- **A lower S3 Object Lock** of a storage target: governance instead of compliance, no lock, a shorter `retention_days` or `legal_hold_on_pin` turned off. `PUT /api/v1/storage-targets/{id}` stores the target with its current lock and answers with `pending_object_lock` (with the two-person rule, `approval` first). Raising the lock applies at once and cancels a pending lowering; a mixed change raises the stronger parts at once and holds the weaker ones.

`GET /api/v1/pending-changes` lists them, and so do `GET /api/v1/settings` (`pending_changes`), `GET /api/v1/jobs/{id}` (`pending_retention`) and the dashboard (Settings → Security). An administrator can cancel a pending change (`DELETE /api/v1/pending-changes/{id}`), which keeps the current protection and needs no approval.

### Storage targets

A storage target that a backup record whose object may exist references (any record but purged ones, deleted backups waiting for their purge included) cannot be deleted, and its type, local path, S3 endpoint, bucket and prefix cannot change (`409`). Failed and cancelled backups only count while their object may exist: one that never named an object does not count, and neither does one whose object the target reports missing (a target that cannot be reached keeps counting them). New credentials, region or path style of a target in use must pass a connection test, or the change is refused (`409`), so wrong credentials can never cut off stored backups. A target nothing references is deleted at once.

No two targets may store archives in the same place: creating a target, or moving one, is refused (`409`) when its location equals or overlaps another target's (the same or a nested local directory, symbolic links resolved, or the same S3 endpoint and bucket with an equal or nested prefix). S3 endpoints are compared normalised: the scheme, trailing dots and slashes and the default ports 80 and 443 are ignored and the host is compared in lower case, so `https://minio:9000` and `http://minio:9000`, or `https://x` and `https://x:443`, are one place; no endpoint, `aws` and every `*.amazonaws.com` host (regional, global, dual-stack or virtual-host style) are all Amazon S3, where the bucket and prefix alone decide. Local paths are compared with their symbolic links resolved (of the path itself when it exists, else of its nearest existing parent), and case-insensitively on macOS and Windows, whose default file systems ignore case. Bind mounts cannot be detected: two container paths (or two mount points) that mount the same host directory look like different places to MongoRescue, so do not mount one host directory twice. Before the purge deletes an object, it also checks every live record on every target that resolves to the same physical object, so an alias made before this check existed cannot make one target's purge delete another target's archive.

### Metadata snapshots

The [metadata backups](production.md#metadata-backups) follow the same rules: retention never deletes a snapshot younger than the grace period, whatever `metadata_backup.retention_count` says, and lowering that count is a pending change (with the two-person rule, an approval request first) that applies after the current grace period.

### The two-person rule

With `security.require_second_approver` on, these actions do not run; they become approval requests (`202` with `{approval_required: true, approval}`):

- deleting backups (single or bulk), unpinning backups (single or bulk);
- deleting a storage target;
- shortening a job's retention, lowering the grace period or the metadata snapshot count (each then still waits for the grace period once approved); an approved retention shortening is bound to the job's creation time and refused if the job was deleted and created again;
- an in-place restore that drops the target database first (`drop_target`);
- an [in-place point-in-time restore](pitr.md#replacing-the-original-databases-in-place) (`pitr_in_place`), which replaces the databases it names after a safety backup; the approval starts the same request, and starting it publishes `security.destructive_action` (`action: "pitr_in_place"`);
- removing or changing a connection's [post-restore commands](api.md#post-restore-commands) (`reduce_post_restore_commands`): they re-apply erasures after restores, so quietly removing one could bring erased data back. The rest of the connection update applies at once, the new list waits sealed until the request is decided, and adding commands needs no approval;
- turning the two-person rule off;
- every change that makes or unmakes an administrator: creating a user with the admin role (the user is created as a viewer and the admin role waits), promoting a user to admin, demoting or deleting an administrator, creating an admin-scope API key (the key works with the operator scope until the admin scope is approved; its plaintext is shown once, as usual), and single sign-on changes that can grant admin (a new admin role mapping, or turning single sign-on on, or another issuer, client or groups claim, while admin mappings exist; such a change cannot carry a new client secret, which requests never store), and single sign-on changes that can take admin away (an admin mapping removed or lowered, another issuer, client or groups claim while admin mappings exist, or the first role mappings while single sign-on users hold the admin role);
- a single sign-on that would demote an administrator: the user keeps the admin role, Settings shows the `oidc_role_kept` warning, and an approval request (`sso_demote_admin`) asks a second administrator to apply the role the groups give (one open request per user and role, not one per sign-in);
- resetting another user's password: it needs a dashboard session (API keys get `403`), and the new password's bcrypt hash waits apart from the request, never shown, until the request is decided;
- downloading a recovery kit with the plaintext `secret.key` of a [KMS-wrapped](#wrapping-secretkey-with-a-kms) installation (`plaintext_recovery_kit`): approving runs nothing; the requester then downloads the kit once, within an hour, with `approval_id` (the approver never gets it).

Changing which connections a user or an API key may touch is not on this list: it never makes anyone an administrator, so it applies at once, also when it lifts a limit (by hand, or because a sign-in matched a group mapping without connections).

A password reset by another user (with or without the rule) also forces the user to choose a new password: until they do, their sessions can only change their own password (`PUT /api/v1/users/{id}/password`), sign out and read `GET /api/v1/auth/me` (`must_change_password: true`); every other request answers `403`. So whoever chose the password cannot keep using it unnoticed.

A different administrator must approve (`POST /api/v1/approvals/{id}/approve`) or reject (`.../reject`) the request within 72 hours; afterwards it expires. The rules live in `internal/auth`:

- The requester cannot approve their own request, also not through a session when they requested it with one of their API keys.
- API keys can request and reject, but never approve: approving needs an administrator signed in to the dashboard. MCP has no tool that approves, rejects, deletes or unpins.
- The approver must have been an administrator before the request was made. Every user records when they got their current role (`role_changed_at`: their creation, a role change, or a password reset by someone else), and a newer administrator can never approve an older request. So a stolen administrator credential cannot create, promote or reset the password of an account and approve with it: the account needs an approval first, and even then it is too new for the requests made before.
- An API key without a creator (imported from `MONGORESCUE_API_KEY`, or created before users existed) cannot request anything while the rule is on (`403`): nobody could tell its approver apart from its requester. Use a key created by a user.
- The rule can only be turned on while at least two administrators exist. If fewer than two remain anyway (data edits, single sign-on demotions), turning it off needs no approval: it becomes a pending change that applies after the grace period, and is dropped if two administrators can approve again by then. It records the administrators of the moment and is dropped as well when one of them is no longer an administrator when it is due, unless an approved request demoted or deleted them, so a credential that makes itself the last administrator cannot use the lockout path.

The approved action runs with the approver's rights and every protection checked again (pins, running backups, the newest good and verified backups, targets in use). Requests, approvals, rejections, undeletes and purges are recorded in the [audit log](audit.md) (`approval_id`, `protection`, `purge_after`), and every destructive action and request is published as `security.destructive_action` or `security.approval_requested`, which [notification rules](notifications.md) can select.

## Per-connection access

In a shared instance, limit the people and the automation of each team to their connections ([API](api.md#connection-access), [design](design/roles.md#per-connection-access)). A user or an API key limited to some connections cannot list, read, back up, restore from or restore into another connection, read its backups, restores and run logs, or see its jobs and readiness, by any route, bulk action, MCP tool or CLI command; such requests answer `404`, exactly like a connection that does not exist, so a team does not even learn which other servers exist. Their API keys are capped by their own connections on every request, and they cannot scrape `/metrics`. What it does not cover:

- **Administrators.** They always reach every connection, and so do admin-scope keys; a limit on them is refused. Keep administrators few.
- **The storage.** Teams that share a storage target share its bucket or directory: anyone with the target's credentials reads every team's archives. Give each team its own target (and encrypt with their own recipients) where that matters; a limited user sees only the targets their connections use.
- **Global settings and names.** Settings, notification channels and the names of users stay readable to every role, as before.
- **Deleted connections.** Deleting a connection keeps it in the lists of the users and keys limited to it (it simply no longer exists), and an empty list is no connection, never every connection, so a limit never widens by itself. Saving a user's list drops the deleted IDs.

## API key expiry

API keys are long-lived credentials for automation; a key copied into a CI system, a script or an MCP client works until it expires or is revoked. Give keys an expiry, and let MongoRescue enforce one:

- **An expiry per key.** `POST /api/v1/api-keys` takes `expires_at` (RFC 3339) or `expires_in_days`; the dashboard offers 30, 90, 180 or 365 days or a date. From that moment the key is refused with `401` and the code `api_key_expired` on every route, `/mcp` and `/metrics` included, and each refusal is recorded in the [audit log](audit.md) with the key (identical refusals are coalesced). Only a request carrying the full, correct key learns that it expired; any other key is just unknown. Expired keys stay listed until they are revoked.
- **A maximum lifetime.** `security.api_key_max_lifetime_days` (default `0`, unlimited; at most 3650) caps every key: a new key needs an expiry within it (`400` without one or beyond it). Keys stored before it was set have no expiry, so turning it on (or lowering it) gives every key that has no expiry, or a later one, the deadline *now + the maximum lifetime*, keys created by an API key without a user included. Each key records the maximum it was checked against, so the same maximum never caps it twice (also not when the clock steps back). This is announced, not applied silently: the `api_key.deadline_set` event names the keys and the deadline, an audit entry (`api_key.deadline_set`) and a log line record it, and the keys show the deadline in Settings → API keys. Choose a lifetime that leaves time to rotate the automation that uses them.
- **Warnings before expiry.** A key that comes within `security.api_key_expiry_warning_days` (default 14) of its expiry is announced once (the `api_key.expiring` event, for notification rules), listed in the `api_keys_expiring` settings warning (the dashboard banner; each user sees their own keys, administrators every key) and counted by `mongorescue_api_keys_expiring` ([metrics](metrics.md)); `mongorescue_api_keys_expired` counts expired keys that were not revoked yet. The check runs on the active instance every hour and whenever these settings change.
- **The imported key.** The key imported from the deprecated `MONGORESCUE_API_KEY` is flagged `imported` (`GET /api/v1/api-keys`) and keeps no expiry, also under a maximum lifetime; while one is set, the `api_key_never_expires` warning asks to replace it with a key created by a user.
- **Clients.** The [command line](cli.md) exits `3` with a hint to create a new key, `apiclient` reports `ErrAPIKeyExpired` (also matching `ErrUnauthorized`), and `GET /api/v1/auth/me` shows a key's `key_expires_at`, so automation can check its own deadline.

## REST API rate limits

A leaked or runaway API key, or a session left polling in a loop, cannot hammer the REST API: every request is charged to a token bucket, and a request over its budget is refused with `429 Too Many Requests`, a `Retry-After` header and the code `rate_limited`, before it runs.

- **Per API key and per session.** `security.api_rate_limit_per_minute` (default 1200) and `security.api_rate_limit_burst` (default 300) are the budget of each API key and each session, whatever address it comes from. That is far above what the dashboard needs (a tab polls a few requests every 10 seconds, every 2 seconds while runs are active; several tabs of one session share the budget) and what the CLI's `--wait` polling sends. `0` per minute turns every REST limit off.
- **Routes that start work.** Backups, restores and their preflight, job runs, verifications, restore tests, PITR base backups and chain tests, sweeps, storage scans and imports, metadata snapshots, bulk actions and the connection, storage and notification tests also spend `security.api_work_rate_limit_per_minute` (default 60) with `security.api_work_rate_limit_burst` (default 20). `0` leaves them to the general budget. The list is the `workRoutes` table in `internal/server/ratelimit.go`, which a test keeps in step with the routes.
- **Without credentials.** Two fixed budgets per client address, 300 per minute with bursts of 100 each, kept apart so that one cannot exhaust the other:
  - the sign-in routes (setup, login, `GET /api/v1/auth/methods`, `GET /api/v1/auth/me` and single sign-on; budget `auth`);
  - every other request without valid credentials, a missing, wrong or [expired](#api-key-expiry) API key or session on any route, `/mcp` and `/metrics` included (budget `general`). An expired key is charged before its `401 api_key_expired`.
  An IPv6 address counts by its /64. Failed sign-ins are throttled further, as before.
- **Behind a reverse proxy, turn on `security.trust_proxy_headers`.** Without it every client has the proxy's address, so all of them share these two budgets (and the login throttle). MongoRescue logs a warning once when requests carry `X-Forwarded-For` or `X-Real-IP` while it is off. See [production.md](production.md#tls-and-the-reverse-proxy).
- **Not limited.** `GET /api/v1/health` (probes and load balancers), `/metrics` with a valid key (or when public) and the dashboard's own files. `/mcp` has its own limit per key ([mcp.md](mcp.md#audit-log-rate-limit-and-metrics)), so a key's MCP calls and REST requests are counted apart.
- **Observability.**
  - Every refusal counts in `mongorescue_api_rate_limited_total{caller, budget}` ([metrics](metrics.md)).
  - A refused caller is logged at most once a minute, with its API key ID, user ID or address, never a secret.
  - A refused change is recorded in the [audit log](audit.md) like any other change: outcome `rate_limited`, with identical refusals coalesced. A caller's first refusal in a minute is recorded there even when it is a read.
- **Clients back off.**
  - The dashboard waits for the `Retry-After` (at most a minute) before its next request and sends the refused request once more.
  - `apiclient` and the [command line](cli.md) send a refused request again after `Retry-After`, twice, each wait at most 30 seconds.
- **Memory.** The buckets live in memory, one per active key, session or address, and at most 100,000 are kept, so a flood of new addresses cannot exhaust memory.
  - At the cap, buckets that have refilled go first; they are as good as new.
  - Only then are the least recently used ones evicted, an eighth of the cap at a time.
  - A caller that keeps being refused stays among the most recently used, so a flood of new keys cannot reset its bucket.
  - A restart or an HA takeover starts every budget afresh.

## Immutable backups (S3 Object Lock)

Delete protection guards the paths through MongoRescue; an S3 target with an object lock mode also guards the storage itself. Every object MongoRescue uploads to it (backup archives, PITR base backups, oplog chunks, metadata snapshots) is locked for the target's `retention_days` ([configuration](configuration.md#immutable-backups-s3-object-lock)):

- **Compliance mode**: nobody can delete or overwrite a locked version, or shorten its lock, before it ends: not MongoRescue, not an attacker with the bucket's credentials or with MongoRescue's database and `secret.key`, not the bucket owner, not the AWS root account. Only closing the whole cloud account removes the data.
- **Governance mode**: the same, except for principals with `s3:BypassGovernanceRetention`, who can delete locked versions or shorten their lock. Never give that permission to MongoRescue's credentials; the dashboard warns about governance-mode targets.
- **Legal hold on pin** keeps a pinned backup's archive beyond its retention until it is unpinned. Unpinning needs the admin role and, with the two-person rule, a second administrator.

MongoRescue itself respects the lock: a deleted backup whose archive is still locked stays `deleted` (and can be undeleted) until the lock ends, and then the purge deletes the recorded object version, which frees the space; deleting only the key would leave the data behind a delete marker. A target with an object lock mode is refused unless its bucket has Object Lock and versioning enabled; MongoRescue never enables them. Lowering a target's lock (a shorter retention, governance instead of compliance, or none) takes the grace period like any lowered protection ([above](#lowering-a-protection-takes-the-grace-period-too)), and then only applies to later uploads: objects already written keep their lock.

## Wrapping secret.key with a KMS

`secret.key` seals every credential MongoRescue stores (connection strings, storage keys, notification secrets, the age identity) in `mongorescue.db` and its metadata snapshots: it is the single root of trust of an installation. By default it is a plaintext file in the data directory (mode `0600`), or `MONGORESCUE_SECRET_KEY`. With **key wrapping**, an external key management service protects it instead:

```bash
MONGORESCUE_KEY_WRAP=aws-kms:arn:aws:kms:eu-central-1:123456789012:alias/mongorescue
MONGORESCUE_KEY_WRAP=gcp-kms:projects/my-project/locations/europe-west1/keyRings/mongorescue/cryptoKeys/secret-key
MONGORESCUE_KEY_WRAP=azure-keyvault:https://my-vault.vault.azure.net/keys/mongorescue
MONGORESCUE_KEY_WRAP=vault-transit:https://vault.internal:8200/transit/keys/mongorescue
```

(or the `-key-wrap` flag). The data directory then holds `secret.key.wrapped` only: a JSON file with the provider, the key URI, the KMS key ID, the key's public fingerprint and the ciphertext the KMS returned. At every start MongoRescue asks the KMS to unwrap it, with the instance's own identity, and keeps the key in memory only. The key URI never holds a credential (a URL with user information, a query or a fragment is refused); credentials come from the provider's standard environment:

| Provider | Key URI | Credentials | Permissions on the key |
| :--- | :--- | :--- | :--- |
| `aws-kms` | key ARN, alias ARN, key ID or `alias/<name>` (the region comes from the ARN, else `AWS_REGION`) | AWS default chain: `AWS_*` variables, shared profile, IRSA or EKS Pod Identity, ECS task role, EC2 instance profile. `AWS_ENDPOINT_URL_KMS` points at another endpoint | `kms:Encrypt`, `kms:Decrypt`. The ciphertext is bound to the encryption context `purpose=mongorescue/secret.key` |
| `gcp-kms` | `projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>` (no key version) | Application Default Credentials: `GOOGLE_APPLICATION_CREDENTIALS`, GKE Workload Identity, the attached service account | `roles/cloudkms.cryptoKeyEncrypterDecrypter`. Requests and responses are checked with CRC32C; the additional authenticated data is `mongorescue/secret.key` |
| `azure-keyvault` | `https://<vault>.vault.azure.net/keys/<name>[/<version>]` (an RSA key of a vault or Managed HSM) | `DefaultAzureCredential`: `AZURE_*` variables, Azure Workload Identity, a managed identity, the Azure CLI | `wrapKey` and `unwrapKey` (role *Key Vault Crypto User*). Wrapped with `RSA-OAEP-256`; the ciphertext records the key version |
| `vault-transit` | `https://<vault>:8200/<mount>/keys/<name>` (the Transit engine's mount path; `VAULT_ADDR` is not read) | `VAULT_TOKEN`, else AppRole (`VAULT_ROLE_ID`, `VAULT_SECRET_ID`, mount `VAULT_APPROLE_MOUNT`, default `approle`), else Kubernetes (`VAULT_K8S_ROLE`, mount `VAULT_K8S_MOUNT`, default `kubernetes`, token file `VAULT_K8S_TOKEN_PATH`); `VAULT_NAMESPACE`, `VAULT_CACERT` | `update` on `<mount>/encrypt/<name>` and `<mount>/decrypt/<name>`. A refused token of AppRole or Kubernetes auth is renewed by logging in again |

Google Cloud KMS and Vault are reached over their REST APIs (no client library), AWS and Azure through their SDKs. **No KMS client follows a redirect**: every request carries credentials (a token, a signed request) and often the key itself, so a `3xx` answer is an error. A `vault-transit` key URI must use `https://`; plain `http://` is refused unless `MONGORESCUE_KEY_WRAP_ALLOW_INSECURE=true`, which sends the Vault token and the key unencrypted and is meant for a **development Vault only** (every start that uses it logs a warning).

**Turning it on.** Set `MONGORESCUE_KEY_WRAP` and restart. On that start MongoRescue wraps the existing `secret.key` (and the `secret.key.next` and `secret.key.previous` of a [key rotation](encryption.md#key-rotation-runbook)), writes the wrapped files atomically (temporary file, fsync, rename), unwraps them again to check that they hold the same keys, and only then removes the plaintext files: each is renamed to a temporary name first, then overwritten with zeros, synced and deleted. An interrupted migration finishes at the next start, which also removes (with a warning) a key file left under a temporary name, or a half-overwritten plaintext file next to a wrapped key that unwraps and matches its fingerprint; a plaintext file that holds *another* key than its wrapped counterpart stops the start instead of being dropped. A first start without any key generates one and stores it wrapped right away. The change is published as `security.key_wrapping_changed` (`action: wrapped`) and recorded in the [audit log](audit.md) (`keywrap.wrapped`). Download a new [recovery kit](production.md#recovery-kit) afterwards: the dashboard asks for one. On copy-on-write file systems and SSDs the overwritten blocks of the old plaintext file may survive on the device; if that matters, rotate `secret.key` once wrapping is on (the new key never touches the disk in plain form).

**Startup.** The unwrap is retried (5 attempts over about 15 seconds, 20 seconds at most each) while the KMS is unreachable; a refusal (a tampered ciphertext, another KMS key, a missing permission) fails at once. Either way the start fails with an error naming the provider, the key URI and what to check; **MongoRescue never falls back to a plaintext key**. With wrapping on, `MONGORESCUE_SECRET_KEY` is refused (there is no file to wrap), and with wrapping off a data directory holding `secret.key.wrapped` refuses to start instead of generating a new key. The readiness report (`GET /api/v1/readiness`, `key_wrapping`) shows that wrapping is active, and `mongorescue_key_wrapping_active`, `mongorescue_key_wrap_duration_seconds` and `mongorescue_key_wrap_errors_total` ([metrics](metrics.md)) watch the KMS calls.

**Rotation.** A [secret key rotation](encryption.md#key-rotation-runbook) works as before: the new key is wrapped (and checked) before it is written to `secret.key.next.wrapped`, the old one is kept as `secret.key.previous.wrapped`, so no key of a rotation reaches the disk in plain form; a KMS that is down stops the rotation before anything changed. A new version of the KMS key (automatic rotation in AWS, Google Cloud or Vault, a new key version in Azure) needs nothing: old ciphertexts keep unwrapping. To move to **another KMS key**, stop MongoRescue and run

```bash
mongorescue key rewrap --data-dir /data --to vault-transit:https://vault.internal:8200/transit/keys/mongorescue-2
```

with `MONGORESCUE_KEY_WRAP` still naming the current key: it unwraps every wrapped key file with that key, wraps it with the new one, checks it and replaces the file; then set `MONGORESCUE_KEY_WRAP` to the new value. Keep the old KMS key until no backup of the data directory and no recovery kit holds a key wrapped with it. `mongorescue key status` shows the provider, key URI and fingerprint of the stored key without calling the KMS.

**Leaving the KMS.** `mongorescue key unwrap --data-dir /data --confirm-plaintext` writes the keys back as plaintext files (`0600`) and removes the wrapped ones; unset `MONGORESCUE_KEY_WRAP` before the next start. Both commands take the KMS key from `MONGORESCUE_KEY_WRAP`, never from the provider and key URI recorded in the data directory: a file wrapped with another key is refused, so a tampered data directory cannot send the instance's credentials elsewhere. To use another key, name it with `--from <provider>:<key URI>` and confirm it with `--confirm-from`. Both refuse to run while MongoRescue holds the data directory, need the operating system permissions of the data directory and an identity allowed to use the KMS keys, and are journaled in the data directory: the next start publishes them as `security.key_wrapping_changed` (`rewrapped`, `unwrapped_to_file`) with the operating system user as the actor, and records them in the audit log.

**Recovery kit.** With wrapping on, the [recovery kit](production.md#recovery-kit) carries `secret.key.wrapped` and the KMS key URI instead of the key: **recovering with it needs access to the KMS key**. If the KMS key is deleted or access to it is lost, neither the kit nor the database can be opened. An administrator can still ask for a kit with the plaintext key, through the API only (`"plaintext_secret_key": true` in `POST /api/v1/recovery-kit`; the dashboard always downloads the wrapped kit); the download is logged as a warning, its audit entry records `plaintext: true`, and under the [two-person rule](#the-two-person-rule) it needs a second administrator's approval first.

**High availability.** Every instance of a cluster needs the same key after unwrapping: copy `secret.key.wrapped` of the first instance into the data directory of the others (each may also wrap its own copy of the same plaintext key; the ciphertexts then differ, the key does not), and give every instance an identity allowed to use the KMS key. The shared-key check of the lease (a key ID derived from the unwrapped key) refuses an instance whose key differs, as before. An instance unwraps once per process: changing roles (a takeover or a step-down) does not need the KMS.

### What key wrapping changes in the threat model

- **A copy of the data directory, a volume snapshot or a stolen disk** no longer holds the root key: the credentials in `mongorescue.db` stay sealed unless the attacker can also use the KMS key with an identity allowed to decrypt.
- **Losing the data directory** no longer loses the root of trust, as long as the KMS key exists: a recovery kit or a backup of `secret.key.wrapped` restores it on a new host with the right identity.
- **It does not protect a running instance**: the key is in its memory, and whoever controls the host, the process or its identity can unwrap the key (or read the credentials) too. Restrict who can assume the instance's role, and alert on KMS decrypt calls from elsewhere (CloudTrail, Cloud Audit Logs, Key Vault and Vault audit logs).
- **It adds a dependency**: the instance cannot start while the KMS is unreachable or the key is disabled, and deleting the KMS key destroys every credential sealed with `secret.key`. Use the provider's deletion protection (AWS's waiting period, Google Cloud's scheduled destruction, Azure's soft delete and purge protection, Vault's `deletion_allowed=false`), and decide whether to keep a plaintext recovery kit offline as a last resort.

## What this protects against

- A stolen administrator API key or session, or a malicious or careless administrator, using MongoRescue (the dashboard, the REST API, the CLI or MCP) to delete backups, run retention with a short policy, lower the protections, or delete or repoint storage targets: every backup stays recoverable for at least the grace period, which is the time you have to notice (configure a `security.destructive_action` notification rule) and undo.
- With the two-person rule, one compromised administrator cannot finish any of those actions at all.
- With an S3 target in compliance mode, **deletion on the storage side** too: nobody, whatever credentials they hold, can delete a backup's archive before its lock ends.

## What this does not protect against

- **Deletion on the storage side, without Object Lock.** On a target without an object lock mode (or in governance mode, by a principal with `s3:BypassGovernanceRetention`), anyone with the bucket's credentials (or the storage target's credentials taken from MongoRescue's database together with `secret.key`), or with write access to a local target's directory, can delete the archives directly, without MongoRescue noticing until a [storage scan](verification.md#storage-scans) reports them missing. Local targets have no immutability option at all: a delete there is a plain file removal. Protect the storage itself:
  - **S3 Object Lock in compliance mode** on the target ([above](#immutable-backups-s3-object-lock)) makes every archive undeletable until its lock ends. Archives are deletable again afterwards, so choose `retention_days` at least as long as the time you need to notice an attack.
  - **Least-privilege credentials**: give MongoRescue's access key `s3:PutObject`, `s3:GetObject`, `s3:ListBucket` and `s3:AbortMultipartUpload`, but not `s3:DeleteObject`, and let a bucket lifecycle rule expire objects instead. Set the lifecycle expiry to at least the longest retention plus the grace period. The purge then cannot delete archives: it logs a warning and keeps the backups `deleted` until the lifecycle rule removed the objects, after which it marks them purged. Partial archives of failed or cancelled backups and the probe objects of target tests (`.mongorescue-probe-*`) stay too until the lifecycle expires them; storage scans report the partial ones as orphans.
  - Keep a copy outside MongoRescue's reach (another account, offline media).
- **Access to the host or the data directory.** Whoever can write `mongorescue.db` or read `secret.key` can change any record, any setting and any credential. Protect the host, the volume and the recovery kit. [Key wrapping](#wrapping-secretkey-with-a-kms) keeps the key off the volume, but not out of a running instance's memory or out of reach of its cloud identity.
- **Two administrators acting together**, or one who controls two administrator accounts that existed before the request (the rule stops a single credential from making a new one). Single sign-on users get their roles from the identity provider: whoever controls the provider's groups controls who is an administrator. Watch the [audit log](audit.md) and notifications and keep administrators few.
- **Waiting out the grace period unnoticed.** The protection buys time; it does not raise an alarm by itself unless a notification rule does.
- **The encryption settings.** Delete protection does not cover them. A replaced or removed age identity or passphrase is kept as a retired key, so older backups stay restorable; keep the recovery kit offline anyway.
- **The source data.** An in-place restore (admin, with confirmation) overwrites a database; delete protection covers backups, not MongoDB. With the two-person rule, an in-place restore that drops the target first waits for a second administrator; one that only overwrites does not.
- **Metadata snapshots** are not soft-deleted: they are kept for at least the grace period and then removed by their own retention.
