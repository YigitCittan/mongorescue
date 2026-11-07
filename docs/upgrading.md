# Upgrading and rolling back

MongoRescue upgrades by replacing the binary or image: the next start migrates the metadata database (`mongorescue.db`) to the new schema. This page is the one place for the steps, the order for [high availability](#high-availability) and the way back. [versioning.md](versioning.md) says which releases can contain a migration.

- [Before you upgrade](#before-you-upgrade)
- [What a start does to the database](#what-a-start-does-to-the-database)
- [Upgrade steps](#upgrade-steps): [Docker](#docker-and-docker-compose) · [Kubernetes](#kubernetes-helm) · [binary and systemd](#binary-and-systemd) · [desktop app](#desktop-app)
- [High availability](#high-availability)
- [Rolling back](#rolling-back)
- [The database is newer than this binary](#the-database-is-newer-than-this-binary)
- [Upgrade notes of every release](#upgrade-notes-of-every-release)

## Before you upgrade

1. **Read the upgrade notes** of every release between yours and the target: the `### Upgrade notes` of its [CHANGELOG.md](https://github.com/YigitCittan/mongorescue/blob/main/CHANGELOG.md) section, repeated at the top of the release page. They list what you must do (a new opt-in, a changed default, a refused request).
2. **Have a recovery path that does not depend on the upgrade:**
    - the [recovery kit](production.md#recovery-kit) downloaded and stored offline, in particular `secret.key` (a rollback and a restore both need the key the database was written with);
    - a fresh [metadata backup](production.md#metadata-backups) on a target outside the instance (Settings → Recovery → *Back up now*, or `POST /api/v1/metadata-backup/run`), or a copy of the stopped data directory or a volume snapshot.
3. **Let running backups and restores finish**, or accept that the stop cancels them ([graceful shutdown](kubernetes.md#graceful-shutdown), `MONGORESCUE_SHUTDOWN_GRACE`).
4. **Pin the version** (`ghcr.io/yigitcittan/mongorescue:<version>`, not `latest`) so that a rollback names the release you came from.
5. Upgrade one release line at a time when you are several minor versions behind, and never skip a release whose upgrade notes say so.

## What a start does to the database

A release with a new schema migration (a *feature* release, see [versioning.md](versioning.md#which-number-changes); a patch release never has one) applies it on its first start. Migrations are applied in order, each in its own transaction, and are not undone by installing the old release: the old binary refuses a database at a newer schema ([below](#the-database-is-newer-than-this-binary)).

So that a rollback has something to go back to, **MongoRescue copies the database before it applies pending migrations**:

- The copy is `mongorescue.db.pre-v<from>-to-v<to>` in the data directory, next to `mongorescue.db` (for example `mongorescue.db.pre-v30-to-v31`; the numbers are schema versions, not release numbers). It is written with SQLite's `VACUUM INTO`, which makes a consistent, compact copy of the live database including its write-ahead log, flushed to disk before the first migration runs, mode `0600`.
- It is taken only when migrations are pending on an existing database: never on a fresh data directory and never when the schema is already current, so restarts do not create copies.
- The newest **3** copies are kept (`-pre-migration-copies` / `MONGORESCUE_PRE_MIGRATION_COPIES`, `0` turns the copies off, at most 50; see [configuration.md](configuration.md#bootstrap-options)). Older ones are deleted after a new copy succeeded.
- **If the copy cannot be written** (disk full, directory not writable) **the start fails** with `store: cannot copy the database before migrating it: …` and nothing is migrated. Free space or fix the permissions and start again; only set `-pre-migration-copies 0` when you have your own copy.
- The log says `copied the metadata database before migrating it` (with the file name), then `applied metadata schema migration` for every step.
- **`secret.key` is not copied.** The credentials in the copy are still sealed with the key the database had, which the upgrade never changes, so the key you have is the key the copy needs. Keep it in the recovery kit.
- The copy is local to the instance's data directory and the disk it is on. It is a rollback aid for the minutes after an upgrade, not a backup: it dies with the volume, and a [metadata backup](production.md#metadata-backups) is the copy that survives. In [high availability](#high-availability) each instance has its own.
- Each copy is about the size of `mongorescue.db`, which holds metadata only. The free-space check of the data directory does not count it, so keep room for the copies (by default 3 times the database) next to it.

## Upgrade steps

Whatever the deployment: take the [steps above](#before-you-upgrade), replace the binary or image, start it, then check that the start finished the migration: the dashboard loads, `GET /api/v1/health` answers `200`, and the log shows no `ErrSchemaTooNew`, `ErrMigrationChanged` or secret key error. Run one backup and, if you use them, the restore test.

### Docker and Docker Compose

```bash
# 1. Set the new version (MONGORESCUE_VERSION in .env, or the tag in your compose file).
# 2. Replace the container; the volume with /data stays.
docker compose pull mongorescue
docker compose up -d mongorescue
docker compose logs --tail 50 mongorescue
```

For `docker run`, stop and remove the container and start the new tag on the same `-v …:/data` volume. A `latest` tag follows the newest stable release; pin a version in production (see [container images](production.md#container-images)).

### Kubernetes (Helm)

```bash
helm upgrade mongorescue ./deploy/helm/mongorescue -n mongorescue --reuse-values --set image.tag=<new version>
kubectl -n mongorescue rollout status statefulset/mongorescue
kubectl -n mongorescue logs statefulset/mongorescue | grep -i "migrat"
```

The pod is replaced (see [graceful shutdown](kubernetes.md#graceful-shutdown)); the new pod migrates the database on its first start, within the startup probe's budget (`probes` values). Read the chart's own upgrade notes in the release's CHANGELOG section as well: a value that was renamed is listed there. Do not use `helm rollback` to go back after a migration without restoring the copy ([rolling back](#rolling-back)).

### Binary and systemd

```bash
sudo systemctl stop mongorescue
sudo install -m 0755 mongorescue /usr/local/bin/mongorescue    # the new, verified binary
sudo systemctl start mongorescue
sudo journalctl -u mongorescue -n 50 --no-pager
```

Verify the download first ([verifying releases](security.md#verifying-releases)). Keep the old binary (`mongorescue.<old version>`) next to it: rolling back needs it.

### Desktop app

Install the update from the app (see [desktop.md](desktop.md#updates)) or run the new installer or archive over the old one. The data directory is `<user config dir>/MongoRescue/data` and survives. A *major* release is a mandatory update from 1.0. Before a feature release, use Settings → Recovery → *Back up now* or copy the data directory while the app is closed; the pre-migration copy appears in the same directory.

## High availability

In [active-passive mode](production.md#high-availability) every instance has its own data directory, and the active instance's metadata snapshots on the HA target are what a standby restores on takeover. An older binary cannot restore a snapshot written by a newer one, so the order is:

1. **Upgrade the standby (or standbys) first.** Each one migrates its own database on its first start; it stays standby, so nothing changes for users.
2. **Upgrade the active instance last.** Its shutdown hands over to the already upgraded standby, which restores the active's snapshot (migrating it if the schema is older) and takes over; then the old active starts as a standby on the new release.
3. Wait for `MongoRescueHATakeoverFailed`-free metrics and a successful backup before declaring the upgrade done. Never run an older release on an instance while a newer one is active.

With the Helm chart this means upgrading the standby release first, then the active one ([kubernetes.md](kubernetes.md#high-availability)); do not `--wait` on a standby, it is never *Ready*. Going back is different: see [rolling back in HA](#high-availability-rollback).

## Rolling back

A release that applied no migration (a patch release, or a feature release that had none for your database) can be rolled back by installing the previous version again: the schema did not change. When the log of the upgrade shows `applied metadata schema migration`, the old binary will refuse to start; go back with the pre-migration copy:

1. **Stop MongoRescue** so that nothing writes to the database.
2. **Find the copy** in the data directory: `mongorescue.db.pre-v<from>-to-v<to>`, where `<from>` is the schema the release you want to run again wrote. After several upgrades take the copy of the upgrade you are undoing (the newest `to`); the copies of earlier upgrades take you further back, at the price of more lost records.
3. **Replace the database with it.** Delete the `-wal` and `-shm` files of the migrated database (they belong to it, not to the copy), keep the migrated database for reference and put the copy in its place, with the owner and mode the old file had.
4. **Install the old release** and start it. It opens the copy at its own schema.

Everything written after the upgrade is gone: jobs, settings, users, API keys, run history and backup records created since. Backups taken after the copy exist as archives on the target but have no record; a storage scan (Settings → Storage → *Scan now*) imports them again.

**Keep `secret.key`.** The copy is sealed with the secret key the database had when it was copied. Do not rotate or replace the key between the upgrade and a rollback; if you did, put the key from the [recovery kit](production.md#recovery-kit) back (`secret.key.previous` holds the key a rotation replaced).

### Docker and Docker Compose

```bash
docker compose stop mongorescue
# List the copies, then swap one in. The helper container uses the service's volume.
docker compose run --rm --no-deps --entrypoint sh mongorescue -c '
  set -e; cd /data
  ls -l mongorescue.db*
  cp -p mongorescue.db mongorescue.db.migrated
  rm -f mongorescue.db-wal mongorescue.db-shm
  cp -p mongorescue.db.pre-v30-to-v31 mongorescue.db'
# Set MONGORESCUE_VERSION (or the tag) back to the previous release, then:
docker compose up -d mongorescue
```

Replace `pre-v30-to-v31` with the name `ls` showed. The helper container runs as the image's user (UID `10001`), so the restored file has the right owner.

### Kubernetes (Helm)

`helm rollback` alone restores the old image but not the database, and the old pod then fails with `ErrSchemaTooNew`. Restore the copy first, with the StatefulSet scaled down (the claim of a release named `mongorescue` is `data-mongorescue-0`):

```bash
kubectl -n mongorescue scale statefulset/mongorescue --replicas=0
kubectl -n mongorescue apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: mongorescue-rollback
spec:
  restartPolicy: Never
  securityContext:
    runAsUser: 10001
    runAsGroup: 10001
    fsGroup: 10001
  containers:
    - name: shell
      image: alpine:3.24
      command: ["sleep", "3600"]
      volumeMounts:
        - { name: data, mountPath: /data }
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data-mongorescue-0
EOF
kubectl -n mongorescue wait --for=condition=Ready pod/mongorescue-rollback
kubectl -n mongorescue exec mongorescue-rollback -- sh -c '
  set -e; cd /data
  ls -l mongorescue.db*
  cp -p mongorescue.db mongorescue.db.migrated
  rm -f mongorescue.db-wal mongorescue.db-shm
  cp -p mongorescue.db.pre-v30-to-v31 mongorescue.db'
kubectl -n mongorescue delete pod mongorescue-rollback
helm rollback mongorescue -n mongorescue    # or helm upgrade with the old image.tag
kubectl -n mongorescue scale statefulset/mongorescue --replicas=1
```

Free space on the claim is needed for the extra `mongorescue.db.migrated` file; delete it when you are done.

### Binary and systemd

```bash
sudo systemctl stop mongorescue
cd /var/lib/mongorescue/data
ls -l mongorescue.db*
sudo cp -p mongorescue.db mongorescue.db.migrated
sudo rm -f mongorescue.db-wal mongorescue.db-shm
sudo cp -p mongorescue.db.pre-v30-to-v31 mongorescue.db
sudo install -m 0755 /usr/local/bin/mongorescue.0.28.0 /usr/local/bin/mongorescue
sudo systemctl start mongorescue
```

### Desktop app

Quit the app completely (not just the window, which hides to the tray), open the data directory (`<user config dir>/MongoRescue/data`), move `mongorescue.db`, `mongorescue.db-wal` and `mongorescue.db-shm` aside, copy the `mongorescue.db.pre-v…` file to `mongorescue.db` and install the previous version from the [releases page](https://github.com/YigitCittan/mongorescue/releases). The app's in-app update never offers to go back.

### High availability rollback

Stop every instance. On the instance that should be active again, restore its pre-migration copy as above and start the old release with `mongorescue ha force-takeover --keep-local-database` ([forcing a takeover](design/ha.md#forcing-a-takeover)): the HA target holds snapshots at the newer schema that the old release cannot read, and the lease would otherwise suspect a rollback. It takes over with the restored database and writes a snapshot at once. Roll the standbys back afterwards, without their data (they restore the snapshot), or with their own copies.

### Without a copy

If the copy is gone (copies turned off, the volume lost), restore the newest [metadata snapshot](production.md#restore-mongorescue-from-a-snapshot) taken before the upgrade, with the recovery kit's `secret.key`, and start the old release on it.

## The database is newer than this binary

```
store: database schema is newer than this binary supports: database is at version 32, this binary knows up to 31
```

(`ErrSchemaTooNew`.) The database was migrated by a newer release than the one you started. MongoRescue refuses to start and writes nothing, not even the secret key check. This happens after an intentional or accidental downgrade, when two releases share a data directory, or when an image tag moved (before the tag change of [#225](https://github.com/YigitCittan/mongorescue/pull/225), `latest` followed unreleased `main`). Either start the release that migrated the database, or [roll back with the pre-migration copy](#rolling-back). Editing `schema_migrations` by hand is never a fix.

A different message, `ErrMigrationChanged`, means the binary embeds a migration whose checksum differs from the one recorded in the database (a build from a modified source tree, or a version that is not a release): run the release that applied it.

## Upgrade notes of every release

Every release section of [CHANGELOG.md](https://github.com/YigitCittan/mongorescue/blob/main/CHANGELOG.md) has an `### Upgrade notes` subsection when an upgrader must act (a new opt-in, a changed default, a removal, a migration that blocks downgrades), and the [release page](https://github.com/YigitCittan/mongorescue/releases) starts with *Highlights* and *Upgrade notes* before the full list of changes. A release with nothing to do says so.

### Removal of the legacy imports before 1.0

The one-time import of deprecated environment variables, `config.json` and `state.json` stays through 0.x and is removed before 1.0.0. Upgrade through the last 0.x release (start it once so that it imports) before moving to 1.0. See [configuration.md](configuration.md#upgrading-deprecated-environment-variables-and-configjson).
