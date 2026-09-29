# Privacy policy

MongoRescue is self-hosted software. It has no telemetry, analytics or crash reporting, and the project runs no service that receives data from it.

## What MongoRescue connects to

MongoRescue only makes network connections that you configure or that are listed here:

- **MongoDB servers** you add as connections, to run backups and restores.
- **Storage targets** you configure (local disk or an S3-compatible service), to store and read backup archives.
- **Notification channels** you configure (for example webhooks or email), to send backup and restore events.
- **GitHub (desktop app only).** At startup and every 6 hours the desktop app asks `api.github.com` for the latest release of `YigitCittan/mongorescue`, and when you choose to update it downloads the release files from `github.com`. These requests carry the app version in the `User-Agent` header and nothing about your data. GitHub's handling of these requests is covered by the [GitHub Privacy Statement](https://docs.github.com/site-policy/privacy-policies/github-general-privacy-statement).

## Data stored on your machine

Settings, connection details, users and backup metadata are stored in the data directory on your machine (`mongorescue.db`). Credentials in it are encrypted with a key that is also kept in the data directory (`secret.key`). Nothing in the data directory leaves your machine unless you copy it.

## Contact

Questions about this policy: open an [issue](https://github.com/YigitCittan/mongorescue/issues). For security issues, see [SECURITY.md](../SECURITY.md).
