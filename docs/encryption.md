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
- **Backup-only instances.** An instance configured with recipients but no identity can create encrypted backups but cannot restore them. This is intentional: the host that takes backups does not need the private key.
- **Turning encryption off** only affects new backups. Decryption uses the identity, the passphrase and every retired key regardless of `enabled`, so existing encrypted backups stay restorable.
- **Existing unencrypted backups** restore unchanged after encryption is enabled.
- A restore of an encrypted backup without a matching key fails with `422 Unprocessable Entity` before `mongorestore` starts.

## Verify-before-restore

Before `mongorestore` runs, MongoRescue can stream the stored artifact once end to end: it recomputes the SHA-256 against the backup record and, for encrypted backups, decrypts the whole stream. Only if that pass succeeds does the real restore begin. On a checksum mismatch, a missing checksum or a decryption error, `mongorestore` is never started and the API returns `422 Unprocessable Entity`.

Verification reads the artifact one extra time, so it is controlled by a policy:

| Source | Name | Values |
| :--- | :--- | :--- |
| Settings → General | `restore_verify_policy` | `always`, `auto` (default), `never` |
| Request | `"verify": true \| false` on `POST /api/v1/restore` | overrides the policy for that request |

`auto` verifies whenever the restore does **not** go into a safe clone, that is, whenever an existing namespace could be overwritten. Safe-clone restores write into a fresh `<db>_rescue_<timestamp>` database, so a failed stream there cannot damage live data.

Without verification, a stream that fails midway (a corrupted object, a wrong key discovered late, a network error) may leave partially restored data in the target namespace.
