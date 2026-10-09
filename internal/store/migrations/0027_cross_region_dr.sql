-- 0027_cross_region_dr: cross-region disaster recovery (see docs/production.md,
-- "Region failure").
--
-- pending_changes accepts the kind 'disable_locked_copies': turning the setting
-- security.require_locked_copies off takes effect only after the delete grace
-- period, like the other lowered protections. SQLite cannot change a CHECK
-- constraint in place, so the table is rebuilt with every row kept.
--
-- oplog_chunk_copies holds the copies of oplog chunks on the copy targets of their
-- PITR stream (copy_targets in the stream's data): one row per chunk and target,
-- with the copy's state in data (a models.BackupCopy document: storage key, S3
-- version, Object Lock retention, attempts, error). The row keeps the key and
-- version itself, so a copy is purged after its chunk row is gone. The copy queue
-- plans the rows of every live chunk, copies the due ones with their checksum
-- checked, and purges the copies of pruned chunks once their lock ends.
--
-- Jobs, storage targets, restore tests and PITR streams keep their new fields
-- (require_locked_copies, region, source_target_id, copy_targets) in their JSON
-- document: no column changes.
--
-- No existing row changes, so nothing is backfilled.

CREATE TABLE oplog_chunk_copies (
    chunk_id        TEXT    NOT NULL,
    stream_id       TEXT    NOT NULL,
    target_id       TEXT    NOT NULL,
    status          TEXT    NOT NULL CHECK (status IN ('pending', 'done', 'failed', 'purged')),
    next_attempt_at INTEGER,
    data            TEXT    NOT NULL CHECK (json_valid(data)),
    PRIMARY KEY (chunk_id, target_id)
) STRICT;

CREATE INDEX oplog_chunk_copies_by_status ON oplog_chunk_copies (status, next_attempt_at);
CREATE INDEX oplog_chunk_copies_by_target ON oplog_chunk_copies (target_id, status);

CREATE TABLE pending_changes_0027 (
    id           TEXT    PRIMARY KEY NOT NULL,
    kind         TEXT    NOT NULL CHECK (kind IN ('retention', 'delete_grace_days', 'metadata_backup_retention', 'disable_second_approver', 'object_lock', 'disable_locked_copies')),
    subject      TEXT    NOT NULL,
    effective_at INTEGER NOT NULL,
    data         TEXT    NOT NULL CHECK (json_valid(data)),
    UNIQUE (kind, subject)
) STRICT;

INSERT INTO pending_changes_0027 (id, kind, subject, effective_at, data)
    SELECT id, kind, subject, effective_at, data FROM pending_changes;

DROP TABLE pending_changes;

ALTER TABLE pending_changes_0027 RENAME TO pending_changes;
