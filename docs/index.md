# MongoRescue

<p align="center">
  <img src="assets/logo.svg" width="360" alt="mongorescue">
</p>

<p align="center">
  <strong>MongoDB backups and restores that stream, verify and encrypt.</strong><br>
  A single Go binary with a built-in web dashboard, Point-in-Time Recovery (PITR), and disaster recovery.
</p>

<p align="center">
  <a href="https://github.com/YigitCittan/mongorescue/actions/workflows/ci.yml"><img src="https://github.com/YigitCittan/mongorescue/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/YigitCittan/mongorescue/releases"><img src="https://img.shields.io/github/v/release/YigitCittan/mongorescue?sort=semver" alt="Release"></a>
  <a href="https://github.com/YigitCittan/mongorescue/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="AGPL-3.0 license"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/YigitCittan/mongorescue"><img src="https://api.scorecard.dev/projects/github.com/YigitCittan/mongorescue/badge" alt="OpenSSF Scorecard"></a>
</p>

---

![The MongoRescue dashboard listing recent backups](assets/screenshots/dashboard-dark.png#only-dark)
![The MongoRescue dashboard listing recent backups](assets/screenshots/dashboard-light.png#only-light)

??? info "More screenshots"
    | | |
    | --- | --- |
    | ![First-run setup](assets/screenshots/setup.png) | ![MongoDB connections](assets/screenshots/connections.png) |
    | First-run setup | MongoDB connections |
    | ![Scheduled jobs](assets/screenshots/jobs.png) | ![Restore dialog with safe clone and verification](assets/screenshots/restore-modal.png) |
    | Scheduled jobs | Restore into a safe clone |
    | ![Storage targets](assets/screenshots/storage.png) | ![Notification channels and rules](assets/screenshots/notifications.png) |
    | Storage targets | Notifications |
    | ![Restore history](assets/screenshots/restores.png) | ![Backups](assets/screenshots/backups.png) |
    | Restore history | Backups |

MongoRescue runs `mongodump` on a schedule, streams the archive to local disk or any S3-compatible bucket, and brings it back with `mongorestore` when you need it. The dump is piped straight through, so memory use stays flat no matter how large the database is.

Restores go into a separate copy of the database (`<db>_rescue_<timestamp>_<id>`) unless you explicitly ask to overwrite the original. Nothing touches production by accident.

---

## Key Features

MongoRescue streams verified, optionally age-encrypted logical backups of standalone servers and replica sets (MongoDB 5.0 to 8.0) to local disk or S3, restores into a safe clone by default and adds point-in-time recovery, S3 Object Lock, SSO, an audit log and an MCP server.

See the [full feature catalogue](features.md), and [how MongoRescue compares](comparison.md) with mongodump + cron, Percona Backup for MongoDB, Atlas Backup, Ops Manager and restic/Velero, including who it is not for (sharded clusters are not supported yet).

---

## Quick Start with Docker

The fastest way to get started is using the official Docker container:

```bash
docker run -d --name mongorescue -p 8080:8080 \
  -v mongorescue-data:/data -v mongorescue-backups:/backups \
  ghcr.io/yigitcittan/mongorescue:latest
```

1. Open `http://localhost:8080` in your browser.
2. Enter the one-time setup code printed in the logs (`docker logs mongorescue`).
3. Create your administrator account, connect your MongoDB database, and schedule your first backup!

<p align="center">
  <img src="assets/demo.gif" alt="Taking a backup and restoring it into a safe clone from the dashboard" width="800">
</p>

For Docker Compose, Kubernetes Helm charts, or standalone binary setups, check the [Configuration Guide](configuration.md) and [Kubernetes Deployment](kubernetes.md).
