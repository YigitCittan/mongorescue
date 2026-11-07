# Why MongoRescue

MongoRescue is a self-hosted, single-binary tool for **logical** MongoDB backups (`mongodump` and `mongorestore`) of standalone servers and replica sets. It streams archives to local disk or S3-compatible storage, checks that they restore, optionally encrypts them with [age](https://age-encryption.org), and restores into a safe clone by default. It adds a dashboard, REST API, CLI, notifications, Prometheus metrics and, on replica sets, [point-in-time recovery](pitr.md).

## Who it is for, and who it is not for

**A good fit when** you run a handful of standalone servers or replica sets (self-hosted, or Atlas through `mongodb+srv://`), want scheduled and verified backups with evidence that they restore, want the archives encrypted and optionally immutable in your own bucket, and prefer one binary or container over a cluster of agents.

**Not a fit when**

- you run **sharded clusters**: they are not supported yet ([#58](https://github.com/YigitCittan/mongorescue/issues/58));
- your databases are so large that a logical dump and restore do not fit your recovery time objective (beyond about 1 TB, see [production.md](production.md)); use a physical or snapshot-based backup;
- you need a vendor-supported product with an SLA;
- you want a managed service with no infrastructure of your own.

## Comparison

Cells for MongoRescue describe this repository's documentation. Cells for other tools cite that tool's official documentation; where the cited page did not let us confirm a fact, the cell says **see docs** and links to where to look. Products change: check the linked pages before you decide.

| | MongoRescue | `mongodump` + cron | [Percona Backup for MongoDB][pbm] | [MongoDB Atlas Backup][atlas] | [Ops Manager / Cloud Manager][opsm] | Generic: [restic][restic], [Velero][velero] |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Sharded clusters** | No ([#58](https://github.com/YigitCittan/mongorescue/issues/58)) | Possible, but you must pause the balancer, cross-shard transactions and DDL for a consistent dump, and `--oplog` cannot be used on a sharded cluster ([mongodump][mongodump]) | **Yes**: logical, physical and incremental backups ([backup types][pbm-types]) | **Yes**; a snapshot cannot be restored after a shard is added or removed ([overview][atlas]) | **Yes**, including point-in-time restore ([restore a sharded cluster][opsm-restore]) | Not MongoDB-aware; see docs ([restic][restic], [Velero][velero]) |
| **Point-in-time recovery** | Yes, replica sets only; restores into clones, in place is an administrator opt-in ([pitr.md](pitr.md)) | No continuous oplog capture; `--oplog` gives one consistent moment ([mongodump][mongodump]) | **Yes**: oplog slices, also alongside physical backups ([PITR][pbm-pitr]) | Continuous cloud backup with a restore window; see docs ([compliance policy page][atlas-bcp-search]) | **Yes**: depends on the oplog you keep per backup ([restore a sharded cluster][opsm-restore]) | No |
| **UI** | Built-in dashboard, desktop app, REST API, CLI, MCP | None | Command line; management through PMM is listed in the docs, see docs ([PBM][pbm]) | Atlas UI; see docs ([Atlas Backup][atlas]) | Ops Manager UI; see docs ([Ops Manager][opsm]) | Command line ([restic][restic], [Velero][velero]) |
| **Encryption** | Optional client-side age encryption, so the bucket holds only ciphertext ([encryption.md](encryption.md)); stored credentials encrypted at rest | None built in as far as the cited page shows; see docs ([mongodump][mongodump]) | See docs ([PBM][pbm]) | See docs ([Atlas Backup][atlas]) | See docs ([Ops Manager][opsm]) | restic: encrypted repositories, see docs ([restic][restic-enc]); Velero: see docs ([Velero][velero]) |
| **Verification and restore tests** | Checksum after upload, at-rest sweep and automated restore tests into temporary databases ([verification.md](verification.md)) | None | See docs ([PBM][pbm]) | See docs ([Atlas Backup][atlas]) | See docs ([Ops Manager][opsm]) | restic: `check` verifies repository structure and, with `--read-data`, the data ([restic][restic-check]); no MongoDB restore test |
| **Immutability** | S3 Object Lock in compliance mode on S3 targets ([security.md](security.md#immutable-backups-s3-object-lock)) | Whatever your storage offers | See docs ([PBM][pbm]) | **Backup Compliance Policy**: no user can modify or delete snapshots before they expire ([Atlas][atlas-bcp-search]) | An "Immutable Snapshots" section exists; see docs ([Ops Manager][opsm]) | See docs ([restic][restic], [Velero][velero]) |
| **Cost** | Free; you pay for your infrastructure and storage | Free tools; your time and storage | See docs ([PBM][pbm]) | Billed per GB per month for dedicated clusters, rates vary by provider and region ([Atlas billing][atlas-cost]) | See docs ([Ops Manager][opsm]) | Free software; storage and compute are yours |
| **Licence** | AGPL-3.0 | See docs ([mongodump][mongodump]) | See docs ([PBM][pbm]) | Managed service | See docs ([Ops Manager][opsm]) | restic: BSD-2-Clause ([GitHub][restic-gh]); Velero: Apache-2.0 ([GitHub][velero-gh]) |

## Where the others are better

- **Percona Backup for MongoDB** handles sharded clusters and offers physical and incremental backups with fast restores of very large datasets ([backup types][pbm-types]). Physical backups need Percona Server for MongoDB ([physical backups][pbm-physical]). If you run a large or sharded Percona or MongoDB deployment, it is the better choice. MongoRescue's logical backups are slower to restore at that scale.
- **MongoDB Atlas Backup** needs nothing to operate, supports sharded clusters and has a Backup Compliance Policy that protects snapshots from every user ([overview][atlas], [compliance policy][atlas-bcp-search]). If your data lives in Atlas and the price is acceptable, use it; MongoRescue can still add an independent, off-platform copy of an Atlas database.
- **Ops Manager and Cloud Manager** back up sharded clusters with point-in-time restore ([restore a sharded cluster][opsm-restore]).
- **`mongodump` + cron** has no moving parts. For one small database it may be all you need; MongoRescue is for when you want scheduling, retention, verification and a restore you can rehearse without writing that yourself.
- **restic and Velero** are not MongoDB-aware. They suit file or volume backups and Kubernetes resources, and they combine well with a database-level backup ([Velero hooks][velero-hooks]); they do not replace one.

## Combining tools

These are not exclusive. A common setup is MongoRescue for logical backups and restore rehearsals plus a snapshot or physical backup for fast recovery of very large data. MongoRescue's own backup of its metadata is covered in [production.md](production.md#restore-mongorescue-from-a-snapshot).

See [features](features.md) for the full list of what MongoRescue does, and [testing](testing.md) for what is tested and the supported MongoDB versions.

[pbm]: https://docs.percona.com/percona-backup-mongodb/intro.html
[pbm-types]: https://docs.percona.com/percona-backup-mongodb/features/backup-types.html
[pbm-pitr]: https://docs.percona.com/percona-backup-mongodb/features/point-in-time-recovery.html
[pbm-physical]: https://docs.percona.com/percona-backup-mongodb/features/physical.html
[atlas]: https://www.mongodb.com/docs/atlas/backup/cloud-backup/overview/
[atlas-bcp-search]: https://www.mongodb.com/docs/atlas/backup/cloud-backup/backup-compliance-policy/
[atlas-cost]: https://www.mongodb.com/docs/atlas/billing/cluster-configuration-costs/
[opsm]: https://www.mongodb.com/docs/ops-manager/current/tutorial/nav/backup-use/
[opsm-restore]: https://www.mongodb.com/docs/ops-manager/upcoming/tutorial/restore-sharded-cluster/
[mongodump]: https://www.mongodb.com/docs/database-tools/mongodump/
[restic]: https://restic.readthedocs.io/en/stable/
[restic-enc]: https://restic.readthedocs.io/en/stable/070_encryption.html
[restic-check]: https://restic.readthedocs.io/en/stable/045_working_with_repos.html
[restic-gh]: https://github.com/restic/restic
[velero]: https://velero.io/docs/main/
[velero-gh]: https://github.com/vmware-tanzu/velero
[velero-hooks]: https://velero.io/docs/main/backup-hooks/
