<p align="center">
  <img src="docs/assets/logo.svg" width="360" alt="mongorescue">
</p>

<p align="center">
  <a href="https://github.com/YigitCittan/mongorescue/actions/workflows/ci.yml"><img src="https://github.com/YigitCittan/mongorescue/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/YigitCittan/mongorescue/releases"><img src="https://img.shields.io/github/v/release/YigitCittan/mongorescue?sort=semver" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="AGPL-3.0 license"></a>
  <a href="https://yigitcittan.github.io/mongorescue/"><img src="https://img.shields.io/badge/docs-online-teal" alt="Documentation"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/YigitCittan/mongorescue"><img src="https://api.scorecard.dev/projects/github.com/YigitCittan/mongorescue/badge" alt="OpenSSF Scorecard"></a>
</p>

**MongoRescue is a self-hosted, single-binary MongoDB backup and restore tool: it streams `mongodump` archives to local disk or S3, proves they restore, and brings them back into a safe clone, with a dashboard, point-in-time recovery and age encryption.**

- **For:** teams running one or a few MongoDB replica sets or standalone servers who want scheduled, verified, encrypted logical backups without writing glue scripts, and a restore they can rehearse.
- **Not for:** sharded clusters (not supported yet, [#58](https://github.com/YigitCittan/mongorescue/issues/58)); multi-terabyte deployments that need physical or snapshot backups; point-in-time recovery of anything but replica sets. See [how it compares](docs/comparison.md) with mongodump + cron, Percona Backup for MongoDB, Atlas Backup, Ops Manager and restic/Velero.

**Try it in one command** (with a throwaway MongoDB, from a clone of this repository):

```bash
docker compose -f docker-compose.yml -f examples/with-mongodb.yml up -d
```

Open http://localhost:8080, enter the setup code from `docker compose logs mongorescue`, create the admin account and add a connection with `mongodb://root:demo-password@mongo:27017/?authSource=admin`. For evaluation only; see [examples/with-mongodb.yml](examples/with-mongodb.yml).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/screenshots/dashboard-dark.png">
  <img src="docs/assets/screenshots/dashboard-light.png" alt="The MongoRescue dashboard listing recent backups" width="100%">
</picture>

## Support matrix

| | Supported | Tested in CI |
| :--- | :--- | :--- |
| MongoDB | 5.0, 6.0, 7.0, 8.0, standalone and replica set (point-in-time recovery: replica sets only) | All four versions, each standalone and as a single-node replica set |
| Database Tools | Included in the image and the desktop app; the server binary needs them on `PATH` ([details](docs/configuration.md#mongodb-database-tools)) | 100.12.2 and 100.19.1 |
| MongoDB Atlas | `mongodb+srv://` connections ([guide](docs/connections.md#mongodb-atlas)) | Not tested in CI; a [manual checklist](docs/connections.md#atlas-manual-test-checklist) is provided |
| Sharded clusters | Not supported ([#58](https://github.com/YigitCittan/mongorescue/issues/58)) | |
| Storage | Local disk, AWS S3, MinIO, Cloudflare R2, Backblaze B2, DigitalOcean Spaces, Wasabi | MinIO and LocalStack; real providers on pushes to `main` |

What each suite guarantees and its known limits: [docs/testing.md](docs/testing.md).

## What it does

MongoRescue runs `mongodump` on a schedule and streams the archive to local disk or any S3-compatible bucket, without buffering it, so memory stays flat however large the database is. Every archive is checksum-verified after upload, and automated restore tests prove the latest backup restores. Restores go into a separate copy of the database (`<db>_rescue_<timestamp>_<id>`) unless you explicitly ask to overwrite the original.

The full catalogue (retention and delete protection, S3 Object Lock, age encryption, point-in-time recovery, SSO, audit log, notifications, Prometheus, high availability, MCP, CLI and more) is in [docs/features.md](docs/features.md).

## Quick start

```bash
docker run -d --name mongorescue -p 8080:8080 \
  -v mongorescue-data:/data -v mongorescue-backups:/backups \
  ghcr.io/yigitcittan/mongorescue:latest
```

1. Open http://localhost:8080.
2. Enter the one-time setup code printed in the logs (`docker logs mongorescue`) and create the admin account.
3. Add a MongoDB connection, test it, and create your first backup job.

The image includes the [MongoDB Database Tools](https://www.mongodb.com/docs/database-tools/). MongoRescue serves plain HTTP: put a TLS-terminating reverse proxy in front of it before you expose the port ([production guide](docs/production.md)).

Other ways to run it: the **desktop app** from [Releases](https://github.com/YigitCittan/mongorescue/releases) ([docs/desktop.md](docs/desktop.md)), a **Helm chart** in [deploy/helm/mongorescue](deploy/helm/mongorescue) ([docs/kubernetes.md](docs/kubernetes.md)), or the headless **server binary** (REST API, MCP, metrics; pass `-dashboard` to serve the dashboard).

Everything is configured in the dashboard and stored in the data volume (`/data`), together with the key that encrypts stored credentials: back that volume up. See [docs/configuration.md](docs/configuration.md).

## Command line

```bash
export MONGORESCUE_URL=https://backup.example.com
export MONGORESCUE_CLI_API_KEY_FILE=~/.config/mongorescue/key

mongorescue backup --job job_shop_nightly --wait --timeout 30m
mongorescue restore bkp_shop_20260924_030000_3f9a1c2e --wait --verify-restore
```

Restores go into a safe clone unless you pass both `--in-place` and `--confirm`. Everything the dashboard does is also in the [REST API](docs/api.md), and assistants can use the [MCP server](docs/mcp.md). See [docs/cli.md](docs/cli.md).

## Documentation

- [Why MongoRescue: comparison with other tools](docs/comparison.md) and the [feature catalogue](docs/features.md)
- [Configuration reference](docs/configuration.md)
- [Connections: TLS, x509, LDAP, Kerberos, Atlas and AWS](docs/connections.md)
- [Desktop app](docs/desktop.md), [Kubernetes (Helm chart)](docs/kubernetes.md)
- [REST API](docs/api.md), [command line](docs/cli.md), [AI assistants (MCP)](docs/mcp.md)
- [Encryption and verified restores](docs/encryption.md), [verification, restore tests and retention safety](docs/verification.md)
- [Delete protection and its threat model](docs/security.md), [threat model (STRIDE)](docs/threat-model.md), [verifying releases](docs/security.md#verifying-releases)
- [Privacy, retention and erasure requests](docs/privacy.md)
- [Notifications](docs/notifications.md), [metrics and alerting](docs/metrics.md), [monitoring](docs/monitoring.md)
- [Running in production](docs/production.md) and the [production checklist](docs/production.md#production-checklist)
- [Troubleshooting](docs/troubleshooting.md), [architecture](docs/architecture.md), [testing](docs/testing.md), [roadmap](docs/roadmap.md)

## Development

You need Go 1.26 or newer (releases are built with the toolchain in `.go-version`).

```bash
make build        # build ./bin/mongorescue
make test-race    # unit tests
make test-integration-docker   # integration tests against MongoDB, MinIO and LocalStack in Docker
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full testing setup and how to send a pull request.

## Security model

MongoRescue holds the credentials of your databases and your storage, so it is built to limit what one stolen credential can do: soft deletes with a grace period, an optional two-person rule, [S3 Object Lock](docs/security.md#immutable-backups-s3-object-lock), roles with [per-connection access](docs/security.md#per-connection-access), a hash-chained [audit log](docs/audit.md) and credentials encrypted at rest. The [threat model](docs/threat-model.md) covers every component; release archives and images are signed with Sigstore cosign and carry SLSA provenance ([how to verify](docs/security.md#verifying-releases)). Report security issues privately, as described in [SECURITY.md](SECURITY.md).

## Known limitations

As of v0.28.0: one working instance at a time (active-passive HA available), `secret.key` is the root of trust, sharded clusters are not supported ([#58](https://github.com/YigitCittan/mongorescue/issues/58); backups and restores through `mongos` are refused unless the connection sets `allow_mongos`), point-in-time recovery covers replica sets only, and there has been no independent security audit. Details and the rest are in [docs/features.md](docs/features.md#known-limitations); run through the [production checklist](docs/production.md#production-checklist) before you rely on MongoRescue.

## Code signing policy

Windows releases are signed with free code signing provided by [SignPath.io](https://signpath.io), certificate by [SignPath Foundation](https://signpath.org). Committers and reviewers: [members of this repository](https://github.com/YigitCittan/mongorescue/graphs/contributors); approvers: [owners](https://github.com/YigitCittan). Only artifacts built by the [release workflow](.github/workflows/release.yml) from a tagged commit are signed.

## Privacy

MongoRescue does not collect telemetry or send data anywhere except to the MongoDB servers, storage targets and notification channels you configure. See [docs/privacy.md](docs/privacy.md).

## License

GNU Affero General Public License v3.0 (AGPL-3.0). See [LICENSE](LICENSE) for details. Need help? See [SUPPORT.md](SUPPORT.md). Security issues: please report them privately, as described in [SECURITY.md](SECURITY.md).
