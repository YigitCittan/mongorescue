-- 0018_rpo: recovery point objectives of jobs and their breaches.
--
-- A job gains data.rpo_minutes, its recovery point objective: how old the newest
-- successful backup of each of its databases may be. It is absent (0) on every job
-- stored before this migration, which keeps the default from the schedule (two
-- schedule intervals plus an hour, at least six hours), so nothing is backfilled.
--
-- rpo_breaches holds one row per job and database whose objective is currently
-- missed: since is when the checker (internal/readiness) first saw the breach (Unix
-- nanoseconds, UTC), and the row exists exactly while job.rpo_missed has been
-- published and job.rpo_recovered has not, so a restart never alerts again for a
-- breach it already reported. Rows of deleted or paused jobs are removed by the
-- checker without an event.

CREATE TABLE rpo_breaches (
    job_id        TEXT    NOT NULL,
    database_name TEXT    NOT NULL,
    since         INTEGER NOT NULL,
    PRIMARY KEY (job_id, database_name)
) STRICT;
