-- 0029_job_search_status: indexed search, filters and last status of the jobs list,
-- and a change counter for the cached overview and readiness report (issue #169).
--
-- Search and filters. The jobs list's search (q) and its database and schedule
-- filters read and decoded every job of the indexed filters at 10,000 jobs. Jobs
-- gain columns the application fills on every job write (internal/store
-- jobIndexColumns), so the filters are SQL on the jobs rows:
--   search_text  the lower-cased ID, name, database, selection and known databases,
--                separated by U+001F, matched as a substring (instr);
--   schedule     the frequency of the cron expression (hourly, daily, weekly,
--                monthly, other);
--   databases    U+001F, then every database the job names or knows, each followed
--                by U+001F, for an exact database match;
--   discovers    1 for an all or pattern selection, whose other databases the
--                application matches against its patterns;
--   index_version  the version of the code that filled them: rows of an older one
--                (every row after this migration) are filled again when the
--                database opens.
-- jobs_filter holds every filter column with the sort keys, so a filtered count
-- and the IDs of a filtered page are read from it without a document;
-- jobs_discovering lists the all and pattern jobs.
--
-- Last status. A job's last status is the outcome of its newest backup (a
-- multi-database job: of its newest run that was not skipped), which the jobs list
-- filters and sorts by. Triggers on backups, job_runs and jobs keep it on the job:
--   last_status             the rank of the outcome: failed 0, partial 1,
--                           cancelled 2, in progress 3, completed 4; NULL when the
--                           job never ran;
--   last_status_at          when that backup or run started (Unix nanoseconds);
--   last_backup_connection  the connection of that backup (single-database jobs),
--                           so a caller limited to some connections can tell
--                           whether the job's newest backup is one of theirs.
-- The view job_last_outcomes computes them; the statement after the triggers
-- fills every existing job. The indexes serve the last-status order (failures
-- first, the newest first within an outcome, then ID) with and without the
-- connection and state filters.
--
-- Change counter. store_changes holds one number that triggers increment on every
-- insert, update and delete of the tables the overview (GET /api/v1/stats) and the
-- readiness report read: jobs, backups, job_runs, restores, restore_tests,
-- rpo_breaches and job_database_joins. Their caches compare it with the number they
-- were computed at, so every write path, the scheduler's and deletions included,
-- drops them.

ALTER TABLE jobs ADD COLUMN search_text TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN schedule TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN databases TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN discovers INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN index_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN last_status INTEGER;
ALTER TABLE jobs ADD COLUMN last_status_at INTEGER;
ALTER TABLE jobs ADD COLUMN last_backup_connection TEXT;

CREATE INDEX jobs_by_last_status ON jobs (last_status, last_status_at DESC, id);
CREATE INDEX jobs_by_connection_last_status ON jobs (connection_id, last_status, last_status_at DESC, id);
CREATE INDEX jobs_by_enabled_last_status ON jobs (enabled, last_status, last_status_at DESC, id);
CREATE INDEX jobs_filter ON jobs (schedule, last_status, enabled, connection_id, databases, search_text, name, created_at, last_status_at, id);
CREATE INDEX jobs_discovering ON jobs (id) WHERE discovers = 1;

CREATE VIEW job_last_outcomes AS
SELECT
    j.id AS job_id,
    CASE WHEN coalesce(json_extract(j.data, '$.database_selection.mode'), '') IN ('', 'single') THEN
        (SELECT CASE b.status WHEN 'failed' THEN 0 WHEN 'cancelled' THEN 2 WHEN 'pending' THEN 3 WHEN 'in_progress' THEN 3 ELSE 4 END
         FROM backups b WHERE b.job_id = j.id ORDER BY b.started_at DESC, b.id DESC LIMIT 1)
    ELSE
        (SELECT CASE r.status WHEN 'ok' THEN 4 WHEN 'partial' THEN 1 WHEN 'failed' THEN 0 WHEN 'cancelled' THEN 2 ELSE 3 END
         FROM job_runs r WHERE r.job_id = j.id AND r.status != 'skipped' ORDER BY r.started_at DESC, r.id DESC LIMIT 1)
    END AS rank,
    CASE WHEN coalesce(json_extract(j.data, '$.database_selection.mode'), '') IN ('', 'single') THEN
        (SELECT b.started_at FROM backups b WHERE b.job_id = j.id ORDER BY b.started_at DESC, b.id DESC LIMIT 1)
    ELSE
        (SELECT r.started_at FROM job_runs r WHERE r.job_id = j.id AND r.status != 'skipped' ORDER BY r.started_at DESC, r.id DESC LIMIT 1)
    END AS at,
    CASE WHEN coalesce(json_extract(j.data, '$.database_selection.mode'), '') IN ('', 'single') THEN
        (SELECT b.connection_id FROM backups b WHERE b.job_id = j.id ORDER BY b.started_at DESC, b.id DESC LIMIT 1)
    END AS connection_id
FROM jobs j;

CREATE TRIGGER backups_last_status_insert AFTER INSERT ON backups
WHEN NEW.job_id != ''
BEGIN
    UPDATE jobs SET (last_status, last_status_at, last_backup_connection) =
        (SELECT rank, at, connection_id FROM job_last_outcomes WHERE job_id = jobs.id)
        WHERE id = NEW.job_id;
END;

CREATE TRIGGER backups_last_status_update AFTER UPDATE OF job_id, status, started_at, connection_id ON backups
WHEN NEW.job_id IS NOT OLD.job_id OR NEW.status IS NOT OLD.status OR NEW.started_at IS NOT OLD.started_at OR NEW.connection_id IS NOT OLD.connection_id
BEGIN
    UPDATE jobs SET (last_status, last_status_at, last_backup_connection) =
        (SELECT rank, at, connection_id FROM job_last_outcomes WHERE job_id = jobs.id)
        WHERE id IN (OLD.job_id, NEW.job_id);
END;

CREATE TRIGGER backups_last_status_delete AFTER DELETE ON backups
WHEN OLD.job_id != ''
BEGIN
    UPDATE jobs SET (last_status, last_status_at, last_backup_connection) =
        (SELECT rank, at, connection_id FROM job_last_outcomes WHERE job_id = jobs.id)
        WHERE id = OLD.job_id;
END;

CREATE TRIGGER job_runs_last_status_insert AFTER INSERT ON job_runs
WHEN NEW.job_id != ''
BEGIN
    UPDATE jobs SET (last_status, last_status_at, last_backup_connection) =
        (SELECT rank, at, connection_id FROM job_last_outcomes WHERE job_id = jobs.id)
        WHERE id = NEW.job_id;
END;

CREATE TRIGGER job_runs_last_status_update AFTER UPDATE OF job_id, status, started_at ON job_runs
WHEN NEW.job_id IS NOT OLD.job_id OR NEW.status IS NOT OLD.status OR NEW.started_at IS NOT OLD.started_at
BEGIN
    UPDATE jobs SET (last_status, last_status_at, last_backup_connection) =
        (SELECT rank, at, connection_id FROM job_last_outcomes WHERE job_id = jobs.id)
        WHERE id IN (OLD.job_id, NEW.job_id);
END;

CREATE TRIGGER job_runs_last_status_delete AFTER DELETE ON job_runs
WHEN OLD.job_id != ''
BEGIN
    UPDATE jobs SET (last_status, last_status_at, last_backup_connection) =
        (SELECT rank, at, connection_id FROM job_last_outcomes WHERE job_id = jobs.id)
        WHERE id = OLD.job_id;
END;

CREATE TRIGGER jobs_last_status_insert AFTER INSERT ON jobs
BEGIN
    UPDATE jobs SET (last_status, last_status_at, last_backup_connection) =
        (SELECT rank, at, connection_id FROM job_last_outcomes WHERE job_id = jobs.id)
        WHERE id = NEW.id;
END;

CREATE TRIGGER jobs_last_status_update AFTER UPDATE OF data ON jobs
WHEN (coalesce(json_extract(NEW.data, '$.database_selection.mode'), '') IN ('', 'single')) != (coalesce(json_extract(OLD.data, '$.database_selection.mode'), '') IN ('', 'single'))
BEGIN
    UPDATE jobs SET (last_status, last_status_at, last_backup_connection) =
        (SELECT rank, at, connection_id FROM job_last_outcomes WHERE job_id = jobs.id)
        WHERE id = NEW.id;
END;

UPDATE jobs SET (last_status, last_status_at, last_backup_connection) =
    (SELECT rank, at, connection_id FROM job_last_outcomes WHERE job_id = jobs.id);

CREATE TABLE store_changes (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    n  INTEGER NOT NULL
) STRICT;
INSERT INTO store_changes (id, n) VALUES (1, 0);

CREATE TRIGGER jobs_changed_insert AFTER INSERT ON jobs BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER jobs_changed_update AFTER UPDATE ON jobs BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER jobs_changed_delete AFTER DELETE ON jobs BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER backups_changed_insert AFTER INSERT ON backups BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER backups_changed_update AFTER UPDATE ON backups BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER backups_changed_delete AFTER DELETE ON backups BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER job_runs_changed_insert AFTER INSERT ON job_runs BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER job_runs_changed_update AFTER UPDATE ON job_runs BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER job_runs_changed_delete AFTER DELETE ON job_runs BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER restores_changed_insert AFTER INSERT ON restores BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER restores_changed_update AFTER UPDATE ON restores BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER restores_changed_delete AFTER DELETE ON restores BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER restore_tests_changed_insert AFTER INSERT ON restore_tests BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER restore_tests_changed_update AFTER UPDATE ON restore_tests BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER restore_tests_changed_delete AFTER DELETE ON restore_tests BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER rpo_breaches_changed_insert AFTER INSERT ON rpo_breaches BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER rpo_breaches_changed_update AFTER UPDATE ON rpo_breaches BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER rpo_breaches_changed_delete AFTER DELETE ON rpo_breaches BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER job_database_joins_changed_insert AFTER INSERT ON job_database_joins BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER job_database_joins_changed_update AFTER UPDATE ON job_database_joins BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
CREATE TRIGGER job_database_joins_changed_delete AFTER DELETE ON job_database_joins BEGIN UPDATE store_changes SET n = n + 1 WHERE id = 1; END;
