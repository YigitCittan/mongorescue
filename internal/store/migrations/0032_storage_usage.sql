-- 0032_storage_usage: cost tracking (issue #63, see docs/cost.md).
--
-- storage_usage_daily is the daily snapshot of what MongoRescue stores: one row
-- per UTC day, storage target, kind (backup, copy, retained, pitr), job, database
-- and PITR stream, with the number of objects and their total stored bytes. The
-- cost collector rewrites the rows of today in one transaction (so a snapshot is
-- idempotent per day) and deletes the days older than 400. Job and database are ''
-- where they do not apply (manual backups, PITR chunks); stream_id is '' for
-- archives.
--
-- A target's price (storage_targets.data $.pricing) lives in the target's JSON
-- document: no column changes. No existing row changes, so nothing is backfilled;
-- the first snapshot is taken when the new release starts.

CREATE TABLE storage_usage_daily (
    day           TEXT    NOT NULL,
    target_id     TEXT    NOT NULL,
    kind          TEXT    NOT NULL CHECK (kind IN ('backup', 'copy', 'retained', 'pitr')),
    job_id        TEXT    NOT NULL DEFAULT '',
    database_name TEXT    NOT NULL DEFAULT '',
    stream_id     TEXT    NOT NULL DEFAULT '',
    objects       INTEGER NOT NULL CHECK (objects >= 0),
    bytes         INTEGER NOT NULL CHECK (bytes >= 0),
    PRIMARY KEY (day, target_id, kind, job_id, database_name, stream_id)
) STRICT, WITHOUT ROWID;
