# Security: delete protection

MongoRescue holds the means to destroy your backups: an administrator, or anyone with a stolen administrator session or API key, can delete backups, shorten retention and delete or repoint storage targets. Ransomware operators look for exactly this before they encrypt production. Delete protection makes sure that **nothing done through MongoRescue makes a backup unrecoverable faster than the delete grace period**, and optionally that **no single administrator can finish a destructive action alone**.

This page is the threat model. Reporting vulnerabilities is described in [SECURITY.md](../SECURITY.md); the API is in [api.md](api.md#delete-protection) and the settings in [configuration.md](configuration.md#security).

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
- removing or changing a connection's [post-restore commands](api.md#post-restore-commands) (`reduce_post_restore_commands`): they re-apply erasures after restores, so quietly removing one could bring erased data back. The rest of the connection update applies at once, the new list waits sealed until the request is decided, and adding commands needs no approval;
- turning the two-person rule off;
- every change that makes or unmakes an administrator: creating a user with the admin role (the user is created as a viewer and the admin role waits), promoting a user to admin, demoting or deleting an administrator, creating an admin-scope API key (the key works with the operator scope until the admin scope is approved; its plaintext is shown once, as usual), and single sign-on changes that can grant admin (a new admin role mapping, or turning single sign-on on, or another issuer, client or groups claim, while admin mappings exist; such a change cannot carry a new client secret, which requests never store), and single sign-on changes that can take admin away (an admin mapping removed or lowered, another issuer, client or groups claim while admin mappings exist, or the first role mappings while single sign-on users hold the admin role);
- a single sign-on that would demote an administrator: the user keeps the admin role, Settings shows the `oidc_role_kept` warning, and an approval request (`sso_demote_admin`) asks a second administrator to apply the role the groups give (one open request per user and role, not one per sign-in);
- resetting another user's password: it needs a dashboard session (API keys get `403`), and the new password's bcrypt hash waits apart from the request, never shown, until the request is decided.

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

## What this protects against

- A stolen administrator API key or session, or a malicious or careless administrator, using MongoRescue (the dashboard, the REST API, the CLI or MCP) to delete backups, run retention with a short policy, lower the protections, or delete or repoint storage targets: every backup stays recoverable for at least the grace period, which is the time you have to notice (configure a `security.destructive_action` notification rule) and undo.
- With the two-person rule, one compromised administrator cannot finish any of those actions at all.

## What this does not protect against

- **Deletion on the storage side.** Anyone with the bucket's credentials (or the storage target's credentials taken from MongoRescue's database together with `secret.key`), or with write access to a local target's directory, can delete the archives directly, without MongoRescue noticing until a [storage scan](verification.md#storage-scans) reports them missing. Protect the storage itself:
  - **S3 Object Lock** (compliance or governance mode retention on the bucket) makes objects undeletable for a fixed time; MongoRescue support for it is planned in [#59](https://github.com/YigitCittan/mongorescue/issues/59).
  - **Least-privilege credentials**: give MongoRescue's access key `s3:PutObject`, `s3:GetObject`, `s3:ListBucket` and `s3:AbortMultipartUpload`, but not `s3:DeleteObject`, and let a bucket lifecycle rule expire objects instead. Set the lifecycle expiry to at least the longest retention plus the grace period. The purge then cannot delete archives: it logs a warning and keeps the backups `deleted` until the lifecycle rule removed the objects, after which it marks them purged. Partial archives of failed or cancelled backups and the probe objects of target tests (`.mongorescue-probe-*`) stay too until the lifecycle expires them; storage scans report the partial ones as orphans.
  - Keep a copy outside MongoRescue's reach (another account, offline media).
- **Access to the host or the data directory.** Whoever can write `mongorescue.db` or read `secret.key` can change any record, any setting and any credential. Protect the host, the volume and the recovery kit.
- **Two administrators acting together**, or one who controls two administrator accounts that existed before the request (the rule stops a single credential from making a new one). Single sign-on users get their roles from the identity provider: whoever controls the provider's groups controls who is an administrator. Watch the [audit log](audit.md) and notifications and keep administrators few.
- **Waiting out the grace period unnoticed.** The protection buys time; it does not raise an alarm by itself unless a notification rule does.
- **The encryption settings.** Delete protection does not cover them. A replaced or removed age identity or passphrase is kept as a retired key, so older backups stay restorable; keep the recovery kit offline anyway.
- **The source data.** An in-place restore (admin, with confirmation) overwrites a database; delete protection covers backups, not MongoDB. With the two-person rule, an in-place restore that drops the target first waits for a second administrator; one that only overwrites does not.
- **Metadata snapshots** are not soft-deleted: they are kept for at least the grace period and then removed by their own retention.
