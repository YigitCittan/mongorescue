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
-- job ID), a lowered security.delete_grace_days (kind 'delete_grace_days', subject
-- ''), a lowered metadata_backup.retention_count ('metadata_backup_retention') or the
-- two-person rule turned off without approvers ('disable_second_approver'). There is
-- at most one change per kind and subject; a newer one replaces it.
--
-- approvals holds destructive actions that wait for a second administrator
-- (security.require_second_approver): status pending until approved, rejected or
-- expired at expires_at (Unix nanoseconds, UTC). data is the whole request as JSON;
-- secret holds what must never be shown (the bcrypt hash of a password reset) and is
-- cleared once the request is decided.
--
-- users.role_changed_at is when the user got their current role (Unix nanoseconds,
-- UTC): an approver must have been an administrator before the request was made.
-- Existing users get their updated_at, which is never earlier than their last role
-- change.
--
-- users.must_change_password (0 or 1) is set when another user resets the
-- password: until the user chooses a new one, their sessions may only change it,
-- sign out and read /api/v1/auth/me.
--
-- Releases before this one refuse a database at this version.

CREATE TABLE pending_changes (
    id           TEXT    PRIMARY KEY NOT NULL,
    kind         TEXT    NOT NULL CHECK (kind IN ('retention', 'delete_grace_days', 'metadata_backup_retention', 'disable_second_approver')),
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
    data       TEXT    NOT NULL CHECK (json_valid(data)),
    secret     TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX approvals_by_created ON approvals (created_at DESC, id DESC);

ALTER TABLE users ADD COLUMN role_changed_at INTEGER NOT NULL DEFAULT 0;
UPDATE users SET role_changed_at = updated_at;

ALTER TABLE users ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0 CHECK (must_change_password IN (0, 1));
