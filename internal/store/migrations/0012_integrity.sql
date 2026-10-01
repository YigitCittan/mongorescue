-- 0012_integrity: backup manifests, restore tests, the retention log and integrity state.
--
-- backup_manifests keeps the manifest captured during a backup (per collection: the
-- document count range and the index specifications) apart from the backup record,
-- so record lists stay small. Restore tests compare a restored copy against it.
--
-- restore_tests records every automated restore test (outcome, duration, mismatch
-- details); the newest one per job is also copied onto the job and the backup.
--
-- retention_log records every backup deleted by a retention policy, with the rule
-- that deleted it, so the dashboard can show a job's retention history.
--
-- integrity_state holds small JSON documents keyed by name: the integrity sweep's
-- status and the latest storage scan (drift report) of every storage target.
--
-- Verification results, pins, imports and the missing status live in the backup
-- record's JSON data and need no schema change.

CREATE TABLE backup_manifests (
    backup_id   TEXT    PRIMARY KEY NOT NULL,
    captured_at INTEGER NOT NULL,
    data        TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE TABLE restore_tests (
    id         TEXT    PRIMARY KEY NOT NULL,
    job_id     TEXT    NOT NULL,
    backup_id  TEXT    NOT NULL,
    started_at INTEGER NOT NULL,
    status     TEXT    NOT NULL,
    data       TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX restore_tests_by_job ON restore_tests (job_id, started_at DESC, id DESC);

CREATE TABLE retention_log (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    at        INTEGER NOT NULL,
    job_id    TEXT    NOT NULL,
    backup_id TEXT    NOT NULL,
    data      TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX retention_log_by_job ON retention_log (job_id, id DESC);

CREATE TABLE integrity_state (
    key   TEXT PRIMARY KEY NOT NULL,
    value TEXT NOT NULL CHECK (json_valid(value))
) STRICT;
