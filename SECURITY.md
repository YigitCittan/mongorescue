# Security Policy

The MongoRescue maintainers take the security and integrity of database operations with the utmost seriousness. As a disaster recovery utility handling MongoDB connection URIs, credentials, and backup streams, robust security is an architectural requirement.

---

## 1. Supported Versions

Security updates and patches are provided for the following versions:

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |
| < 0.1   | :x:                |

---

## 2. Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues, discussions, or pull requests.**

If you discover a security vulnerability in MongoRescue, report it exclusively through:

1. **GitHub Private Vulnerability Reporting**:
   Navigate to the repository's **Security** tab and click **"Report a vulnerability"** to open a confidential advisory.

### What to Include in Your Report
To help us triage and reproduce the issue rapidly, please include:
- A detailed description of the vulnerability and potential impact.
- Step-by-step reproduction steps or a minimal Proof of Concept (PoC).
- Affected MongoRescue version, OS, and MongoDB version.
- Any suggested remediation or patch (if available).

---

## 3. Vulnerability Response SLA

- **Initial Acknowledgment**: Within **48 hours** of receiving the report.
- **Triage & Assessment**: Within **5 business days**, detailing validity and severity score (CVSS).
- **Patch & Release**: Critical security fixes will be published within **7–14 days** of confirmation, coordinated under standard responsible disclosure timelines.

---

## 4. Security Model

MongoRescue is a single-tenant administration tool. Every request needs a user session or an API key; a fresh instance stays in setup mode until the first user is created with a one-time code printed to the server log. Users sign in with bcrypt-hashed passwords and get `HttpOnly`, `SameSite=Strict` session cookies with CSRF tokens; logins are throttled. Automation uses API keys, stored only as SHA-256 hashes. Every user and key has full rights, including restores, deletions, settings, storage targets and the connection and storage test endpoints (which connect to hosts named in the request); there are no roles yet (RBAC is on the roadmap). MongoDB connection strings, notification channel secrets, S3 storage credentials and backup encryption keys are encrypted at rest (AES-256-GCM, bound to their record and field) in the metadata database `mongorescue.db` (mode `0600`, as are its `-wal`/`-shm` files; older releases used `state.json`) with a key in `secret.key` (mode `0600`) or `MONGORESCUE_SECRET_KEY`; anyone holding both the database and the key can read them, so keep the key apart from database backups. All configuration is managed in the dashboard and stored this way; there is no configuration file. Backup archives can be encrypted with [age](https://age-encryption.org) before they leave the host, so storage providers only see ciphertext. MongoRescue serves plain HTTP: run it behind a TLS-terminating reverse proxy. See [docs/production.md](docs/production.md).

---

## 5. Security Architecture & Threat Model

MongoRescue incorporates defensive design principles:
- **Zero In-Memory Buffering**: Dumps stream directly to storage via piped I/O to avoid Denial of Service (DoS) memory exhaustion.
- **Strict URI Credential Masking**: All connection strings are sanitized at runtime before logging or API serialization.
- **Command Injection Defense**: System calls to `mongodump` and `mongorestore` use strict argument slices with `exec.CommandContext`, never concatenated shell invocations.
- **Path Traversal Defense**: All storage operations validate and restrict keys within designated storage boundaries.
- **Safe Clone Namespaces**: Restores default to isolated temporary databases (`<db>_rescue_<timestamp>`) to prevent inadvertent data loss on live production databases.
- **Encryption at Rest**: Optional streaming age encryption (X25519 recommended); the backup host needs only public keys, and by default restores into existing namespaces are verified before `mongorestore` runs.
- **Secret Masking**: API keys, S3 secrets, encryption identities, connection passwords and notification secrets are masked in API responses and never logged.
- **Authentication**: Sessions with CSRF tokens, login throttling with a constant-time dummy comparison for unknown users, hash-only API keys, and no unauthenticated mode.
