# Encryption at rest

MongoRescue can encrypt every backup with [age](https://age-encryption.org) before it reaches storage. Encryption is streamed like the rest of the pipeline: `mongodump` output passes through the age writer on its way to the storage driver, so memory stays constant and the storage backend only ever receives ciphertext.

## Modes

| Mode | Configure | Encrypts with | Decrypts with |
| :--- | :--- | :--- | :--- |
| **X25519** (recommended) | `recipients` | One or more public keys (`age1…`) | Matching private key(s) in `identity`, or a retired identity |
| **Passphrase** (scrypt) | `passphrase` | The passphrase | The same passphrase, or a retired one |

Encryption is configured under **Settings → Encryption** (or `PUT /api/v1/settings`, see [api.md](api.md#settings)). Enabling it in `x25519` mode requires at least one recipient, in `passphrase` mode a passphrase of at least 16 characters. Changes apply to the next backup.

Prefer X25519. Passphrase mode runs scrypt on every backup and restore, which costs about **256 MiB of RAM and roughly one second of CPU per operation**. It also means the backup host holds the secret that decrypts every backup.

## Generating a key

**Generate key pair** in the dashboard (`POST /api/v1/settings/encryption/generate-key`) creates a new X25519 key pair without storing it. The private key (`AGE-SECRET-KEY-1…`) is shown once, with *Copy* and *Download* (a standard age identity file):

```
# created: 2026-09-24T10:00:00Z
# public key: age1...
AGE-SECRET-KEY-1...
```

**Keep a copy of the private key somewhere safe and separate from the backups: without it, backups encrypted to its public key cannot be restored.** *Use this key* stores the private key in MongoRescue's database (encrypted with the secret key) and adds the public key to the recipients. `age-keygen` works as well: paste the public key into *Recipients* and, where restores run, the private key into *Identity*.

## Settings

| Setting | Description |
| :--- | :--- |
| `enabled` | Encrypt newly created backups |
| `mode` | `x25519` or `passphrase` |
| `recipients` | `age1…` public keys new backups are encrypted to |
| `identity` | Private key(s), one per line, used by restores |
| `passphrase` | Passphrase for scrypt mode (encryption and decryption) |
| `retired_keys` | Read-only list of replaced or removed identities and passphrases |

`identity` and `passphrase` are stored encrypted, returned as `******` and never logged. Keys are validated when they are saved, so a malformed key is rejected with `400`.

**Retired keys.** Replacing or removing the identity or the passphrase does not forget it: the old one is kept (encrypted) under `retired_keys` and still tried when restoring, so rotating keys never makes older backups unrestorable. Each retired passphrase that does not match costs one scrypt derivation during a restore.

## Behaviour

- **Storage keys** of encrypted backups end in `.age`. The recorded `size_bytes` and `sha256` describe the stored ciphertext, not the plaintext dump. Backup records carry `encrypted` and `encryption_mode` (`x25519` or `scrypt`).
- **Layer detection.** A stored backup is `mongodump --archive` output, optionally gzip-compressed (`--gzip`), optionally wrapped in age. Restores treat a backup as encrypted when its record says so, its key ends in `.age` or its content starts with the age header, so ciphertext is never handed to `mongorestore` as a dump. Compression is taken from the archive's own signature (falling back to a `.gz` key suffix), so backups stored by earlier releases under a custom `target_key` (no longer accepted) restore with the right `--gzip` flag. The first encrypted chunk is authenticated before `mongorestore` starts.
- **Backup-only instances.** An instance configured with recipients but no identity can create encrypted backups but cannot restore them. This is intentional: the host that takes backups does not need the private key.
- **Turning encryption off** only affects new backups. Decryption uses the identity, the passphrase and every retired key regardless of `enabled`, so existing encrypted backups stay restorable.
- **Existing unencrypted backups** restore unchanged after encryption is enabled.
- A restore of an encrypted backup without a matching key fails with `422 Unprocessable Entity` before `mongorestore` starts; the message says which key to add under **Settings → Encryption**.

## Encryption turned off by an upgrade

Releases up to v0.7.1 imported the deprecated encryption settings (`MONGORESCUE_ENCRYPTION_*`, `encryption.*` in `config.json`) without the on/off switch, so an installation that encrypted its backups before the upgrade has been writing **unencrypted** backups since. At startup MongoRescue detects this when the deprecated switch is still set (`MONGORESCUE_ENCRYPTION_ENABLED=true` or `"enabled": true` in `config.json`), the recipients or passphrase were imported, and encryption is off. Import records alone are not enough, because earlier releases wrote them for `false` as well. Keep the old setting in place until you have checked. MongoRescue then

- logs a warning on every start,
- shows a banner in the dashboard (*Encryption was enabled in your previous configuration but is currently off; backups since the upgrade are NOT encrypted*) with a button that opens **Settings → Encryption**, and
- sends one alert to every enabled notification channel, whatever the rules. The alert is sent again on the next start if the process stopped before it was handed to the notification service.

Encryption is **not** turned on automatically: check the recipients (or passphrase) and enable it yourself. The banner disappears once encryption is on, or when you dismiss it (`POST /api/v1/settings/warnings/encryption_off_after_upgrade/dismiss`). If you turn encryption off yourself in the settings, the warning is not raised. Backups taken while encryption was off stay unencrypted; take a new backup after enabling it, and delete the unencrypted ones if they must not stay in storage.

## Key management and loss

**A lost identity (or passphrase) means lost backups.** age has no recovery mechanism, and MongoRescue keeps no copy of your private key outside its own database. If the identity an encrypted backup was made for is gone, that backup cannot be restored by anyone, including you: a restore fails with `encryption: key material required` (no key configured) or `encryption: decryption failed` (a different key), and `mongorestore` is never started.

- **Escrow the private key offline**, separately from the backups and from the MongoRescue host: a password manager, a hardware-backed vault, or a printed copy in a safe. Keep one copy per key you ever used, not just the current one. With X25519 you can also encrypt to a second recipient whose private key never leaves offline storage, so either key can restore.
- **The database copy is not a backup of the key.** The identity stored in MongoRescue's database is sealed with the secret key (`secret.key` or `MONGORESCUE_SECRET_KEY`, see [production.md](production.md#data-directory)); losing the secret key or the data directory loses that copy too.
- **Rotating keys is safe.** Replacing the identity or the passphrase retires the old one; restores keep trying retired keys, so backups made before a rotation still restore. Deleting the data directory, or starting a new instance without importing the old keys, drops the retired keys as well: paste every escrowed identity into *Identity* (one per line) on the new instance before restoring old backups.
- **Test restores regularly** on an instance that holds only the escrowed key, so a missing key is discovered before it is needed.
- **Passphrase mode** has the same property: a forgotten passphrase cannot be recovered or reset.

## Key rotation runbook

Rotate a key when it may have leaked, when someone who held it leaves, or on a schedule. Every rotation below is audited (the request's audit entry carries `key_rotated` and the old and new fingerprints) and publishes a `security.key_rotated` event (`action` is `secret_key`, `encryption` or `storage_credentials`, `detail` the old and new fingerprints, `approval_id` the approval if any). No key material is ever logged, audited or sent. The dashboard has the three actions under **Settings → Security → Key rotation**.

### secret.key

`secret.key` seals every credential in `mongorescue.db`: connection strings, storage credentials, notification secrets, job heartbeat URLs, the OIDC client secret, the audit webhook secret, the backup encryption identities and passphrases. `POST /api/v1/security/rotate-secret-key` (body `{"current_password": "..."}`) replaces it. It needs an administrator signed in to the dashboard who confirms their password, like the recovery kit; API keys are refused. With the two-person rule on it answers `202` and waits for a second administrator.

What happens:

1. A marker (the old and new key fingerprints, never the keys) is written to the database, then the new key to `secret.key.next` (temporary file, fsync, rename).
2. Every sealed value is re-sealed under the new key in **one** transaction, which also removes every dashboard session. It runs with `synchronous=FULL`, so a power loss after it cannot bring the old key back.
3. The old key is written to `secret.key.previous`, `secret.key.next` is renamed to `secret.key` and the marker is removed.

A crash at any point is settled at the next start: when `secret.key` opens the database the rotation either completed or never committed (a leftover `secret.key.next` is discarded); when it does not and the marker names it as the old key, the commit happened and `secret.key.next` is installed. Any other key is refused as before.

Effects:

- **Everyone signs in again.** Sessions, and single sign-on flows in progress (their cookie is sealed with a subkey of `secret.key`), end.
- **The old key no longer opens the database**; the new one does.
- **API keys keep working.** Generated keys are SHA-256 digests and are not affected. Keys imported from the deprecated `MONGORESCUE_API_KEY` are MACed with a subkey of `secret.key`: the old subkey is kept, sealed under the new key, so they keep verifying, and their first use after the rotation re-hashes them under the new subkey.
- **The recovery kit reminder fires again**, because the kit's fingerprint changed. Download a new kit.
- **Metadata snapshots taken before the rotation are sealed with the old key** and stay below the old install ID's prefix (`_mongorescue/metadata/<old id>/`); new snapshots go below the new key's prefix. They hold the credentials of their time sealed with the old, possibly leaked key, so they do not stay: the rotation records the old install ID in the same transaction, and once the delete grace period (Settings → Security) has passed since the rotation, the metadata backup service deletes the snapshots below that prefix on every storage target. Until then, restore an old snapshot with the old key: the previous recovery kit, or a kit downloaded with **Include the previous key** (`"include_previous_key": true` in `POST /api/v1/recovery-kit`), which adds `secret.key.previous`.
- **secret.key.previous goes after the old snapshots.** Once they are pruned, MongoRescue deletes `secret.key.previous` itself when a recovery kit was downloaded after the rotation; otherwise the dashboard warns (`previous_secret_key`) until you download a kit or delete the file.

#### Keys from MONGORESCUE_SECRET_KEY

A key set by `MONGORESCUE_SECRET_KEY` cannot be rotated by MongoRescue (it does not own the environment); the endpoint answers `409`. Rotate it by hand:

1. Download a recovery kit, then stop MongoRescue.
2. Unset `MONGORESCUE_SECRET_KEY` and put the current key into `<data dir>/secret.key` (mode `0600`).
3. Start MongoRescue, sign in and rotate from the dashboard (or the endpoint).
4. Either keep the file key, or stop again, set `MONGORESCUE_SECRET_KEY` to the content of the new `secret.key`, remove the file and start.
5. Download a new recovery kit.

### Backup encryption key

`POST /api/v1/encryption/rotate` (admin) needs encryption to be on.

- **X25519 mode**: a new identity is generated; its public key replaces the old identity's public key among the recipients (other recipients, such as an offline escrow key, stay). The old identity is retired: restores keep trying it, so every older backup still restores. In recipient-only mode (no identity stored) MongoRescue holds no private key and refuses: generate a new pair yourself and replace the recipient.
- **Passphrase mode**: send the new passphrase, `{"passphrase": "..."}`. The old one is retired.
- **Re-encrypting existing backups** (optional, `{"reencrypt": true}`): a background job streams every encrypted backup, decrypts it with the current or a retired key, encrypts it under the new key into a new object, reads that object back (its SHA-256, and a decryption whose plaintext hash must match the original's), then points the backup at it in one transaction. The old archive is not deleted at once: the same transaction stores a deleted copy of the record that keeps it until the delete grace period ends, when the purge removes it (and it can be undeleted until then). The job survives restarts: an interrupted backup's half-written object is removed and the backup processed again; finished ones are skipped. `GET /api/v1/encryption/reencryption` reports its progress. A backup whose archive does not match its recorded SHA-256 is left alone and reported.

After rotating, escrow the new identity (download a new recovery kit) and keep the escrow of the old ones until no backup needs them.

### Storage credentials

`POST /api/v1/storage-targets/{id}/rotate-credentials` (admin, body `{"access_key_id": "...", "secret_access_key": "..."}`) tests the new S3 credentials on a temporary object named `.mongorescue-probe-rotate-<random>` (below the target's prefix): write, read back, list and delete (the delete is checked). Only when every probe passes are they swapped in, in one write; otherwise it answers `422` with the steps and nothing changes. Revoke the old key at the provider afterwards. See [production.md](production.md#least-privilege-storage-credentials) for the policy the new key needs.

## Verify-before-restore

Before `mongorestore` runs, MongoRescue can stream the stored artifact once end to end: it recomputes the SHA-256 against the backup record and, for encrypted backups, decrypts the whole stream. Only if that pass succeeds does the real restore begin. On a checksum mismatch, a missing checksum or a decryption error, `mongorestore` is never started and the API returns `422 Unprocessable Entity`.

Verification reads the artifact one extra time, so it is controlled by a policy:

| Source | Name | Values |
| :--- | :--- | :--- |
| Settings → General | `restore_verify_policy` | `always`, `auto` (default), `never` |
| Request | `"verify": true \| false` on `POST /api/v1/restore` | overrides the policy for that request |

**In-place restores are always verified**, whatever the policy or the request says: a damaged artifact or a wrong key is found before `mongorestore` touches an existing database. The artifact is read twice (verification, then the restore), never buffered. Only a backup record without a checksum (from an older release) skips the pass; the restore record then carries a warning. An explicit `verify: true` or the `always` policy still refuses such a record.

The policy therefore decides for safe clones only: `auto` and `never` stream straight into the new `<db>_rescue_<timestamp>_<id>` database, `always` (or `verify: true`) verifies first. A clone restore is refused before anything is written if that database already exists.

A clone restore that fails once `mongorestore` has started (a corrupted object, a wrong key discovered late, a checksum mismatch, a `mongorestore` error, a cancellation) drops the partially restored clone, and the record says so. The exception is a restore with documents that failed to insert: that clone is kept for inspection.

Every restore also checks the checksum while it streams, with or without verification: the stored bytes are hashed on their way into `mongorestore`, and once it exits the result is compared with the record. A mismatch fails the restore with *backup checksum mismatch*; a safe clone is then dropped, and the message says so. This catches damage that `mongorestore` itself accepts, such as a changed byte in the archive's collection or index metadata. Records from releases that did not store a checksum are restored without this check.

A restore also fails when `mongorestore` reports documents it could not insert (*N document(s) failed to restore*), for example duplicate keys in an in-place target that already held data without `drop_target`; `mongorestore` itself exits successfully in that case. The target, a clone too, is kept so the documents that arrived can be inspected. If `mongorestore` prints no document summary at all, the restore succeeds with the warning *document counts unavailable*. When the connection's MongoDB user holds the `bypassDocumentValidation` privilege on the target database (the built-in `restore`, `dbAdmin`, `dbOwner` and `root` roles grant it), restores run with `--bypassDocumentValidation`, so documents that predate a collection's validator are restored like the rest. A user with only `readWrite` restores with validation on: a document the validator rejects fails the restore with that hint, and granting the `restore` role fixes it.
