-- 0013_multi_database_jobs: jobs that back up several databases, grouped by job run.
--
-- A job gains data.database_selection (mode single, list, all or pattern, with the
-- named databases and include/exclude glob patterns). Existing jobs become single
-- selections of their database; data.database stays for single-database jobs.
--
-- Every database a run backs up keeps its own backup record and archive; the
-- records of one run share data.run_id, mirrored into an indexed "run_id" column
-- ('' for backups that are not part of a job run, such as manual ones and every
-- backup written before this migration).
--
-- job_runs records each run of a job: its status over all databases (ok, partial,
-- failed, cancelled), the outcome of every database and the databases discovered
-- since the job last ran.

UPDATE jobs
SET data = json_set(data, '$.database_selection', json_object(
        'mode', 'single',
        'databases', CASE WHEN database_name = '' THEN json_array() ELSE json_array(database_name) END,
        'auto_include_new', json('false')))
WHERE json_type(data, '$.database_selection') IS NULL;

ALTER TABLE backups ADD COLUMN run_id TEXT NOT NULL DEFAULT '';
UPDATE backups SET run_id = coalesce(json_extract(data, '$.run_id'), '');
CREATE INDEX backups_by_run ON backups (run_id, started_at, id);

CREATE TABLE job_runs (
    id         TEXT    PRIMARY KEY NOT NULL,
    job_id     TEXT    NOT NULL,
    started_at INTEGER NOT NULL,
    status     TEXT    NOT NULL,
    data       TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX job_runs_by_job ON job_runs (job_id, started_at DESC, id DESC);
