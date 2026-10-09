-- 0027_cross_region_dr: cross-region disaster recovery (see docs/production.md,
-- "Region failure").
--
-- pending_changes accepts the kind 'disable_locked_copies': turning the setting
-- security.require_locked_copies off takes effect only after the delete grace
-- period, like the other lowered protections. SQLite cannot change a CHECK
-- constraint in place, so the table is rebuilt with every row kept.
--
-- Jobs, storage targets and restore tests keep their new fields (require_locked_copies,
-- region, source_target_id) in their JSON document: no column changes.
--
-- No existing row changes, so nothing is backfilled.

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
