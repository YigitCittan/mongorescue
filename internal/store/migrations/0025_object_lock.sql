-- 0025_object_lock: the S3 version and Object Lock retention of oplog chunks (see
-- docs/configuration.md, "Immutable backups").
--
-- version_id is the S3 version of the chunk's object on a versioned bucket ('' on
-- other targets): reads and the purge address that version, since deleting only the
-- key adds a delete marker and frees nothing. retain_until (Unix nanoseconds, UTC)
-- is the end of the object's Object Lock retention, NULL without a lock: storage
-- refuses to delete the object before, so a deleted chunk is purged only once both
-- purge_after and retain_until have passed. Backup records keep the same values in
-- their JSON document.
--
-- pending_changes accepts the kind 'object_lock' (subject the storage target ID):
-- lowering a target's lock (a weaker mode, no lock, a shorter retention or the
-- legal hold on pin turned off) takes effect only after the delete grace period,
-- like a shorter job retention. SQLite cannot change a CHECK constraint in place,
-- so the table is rebuilt with every row kept.
--
-- No existing row changes, so nothing is backfilled.

ALTER TABLE oplog_chunks ADD COLUMN version_id TEXT NOT NULL DEFAULT '';
ALTER TABLE oplog_chunks ADD COLUMN retain_until INTEGER;

CREATE TABLE pending_changes_0025 (
    id           TEXT    PRIMARY KEY NOT NULL,
    kind         TEXT    NOT NULL CHECK (kind IN ('retention', 'delete_grace_days', 'metadata_backup_retention', 'disable_second_approver', 'object_lock')),
    subject      TEXT    NOT NULL,
    effective_at INTEGER NOT NULL,
    data         TEXT    NOT NULL CHECK (json_valid(data)),
    UNIQUE (kind, subject)
) STRICT;

INSERT INTO pending_changes_0025 (id, kind, subject, effective_at, data)
    SELECT id, kind, subject, effective_at, data FROM pending_changes;

DROP TABLE pending_changes;

ALTER TABLE pending_changes_0025 RENAME TO pending_changes;
