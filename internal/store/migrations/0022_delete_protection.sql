-- 0022_delete_protection: soft deletes, delayed lowering of protections and second
-- approvals (see docs/security.md).
--
-- Backups gain two states in the existing status column: 'deleted' (the archive is
-- kept until data.purge_after, the end of the delete grace period, and the deletion
-- can be undone) and 'purged' (the purge removed the archive; the record stays as
-- history). The deletion is described by data.deleted_at, deleted_by,
-- delete_approved_by, delete_reason, purge_after, status_before_delete and
-- purged_at. No existing row changes, so nothing is backfilled.
--
-- pending_changes holds lowered protections that take effect only at effective_at
-- (Unix nanoseconds, UTC): a shortened job retention (kind 'retention', subject the
-- job ID) or a lowered security.delete_grace_days (kind 'delete_grace_days', subject
-- ''). There is at most one change per kind and subject; a newer one replaces it.
--
-- approvals holds destructive actions that wait for a second administrator
-- (security.require_second_approver): status pending until approved, rejected or
-- expired at expires_at (Unix nanoseconds, UTC). data is the whole request as JSON.
--
-- Releases before this one refuse a database at this version.

CREATE TABLE pending_changes (
    id           TEXT    PRIMARY KEY NOT NULL,
    kind         TEXT    NOT NULL CHECK (kind IN ('retention', 'delete_grace_days')),
    subject      TEXT    NOT NULL,
    effective_at INTEGER NOT NULL,
    data         TEXT    NOT NULL CHECK (json_valid(data)),
    UNIQUE (kind, subject)
) STRICT;

CREATE TABLE approvals (
    id         TEXT    PRIMARY KEY NOT NULL,
    status     TEXT    NOT NULL CHECK (status IN ('pending', 'approved', 'failed', 'rejected', 'expired')),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    data       TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX approvals_by_created ON approvals (created_at DESC, id DESC);
