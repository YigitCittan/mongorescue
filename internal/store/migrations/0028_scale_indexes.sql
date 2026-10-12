-- 0028_scale_indexes: indexes for the jobs list and the overview's history at
-- tens of thousands of jobs (EXPLAIN QUERY PLAN at 10,000 jobs, issue #156).
--
-- The jobs list pages in name and ID order (keyset paging). Filtered by
-- connection or by state, it read every job of the filter and sorted them in a
-- temporary B-tree, and counting the enabled jobs scanned the table: the two
-- indexes below serve both filters in page order, and their counts from the index
-- alone. jobs_by_connection is a prefix of jobs_by_connection_name and goes.
--
-- The history's per-day outcomes and the size stored before its window read the
-- status and size of every backup of the range from the table; the two covering
-- indexes answer them from the index. The newest backup per job and database
-- (multi-database jobs) groups a job's backups by database from the index.
--
-- Indexes only: no row changes.

CREATE INDEX jobs_by_connection_name ON jobs (connection_id, name, id);
CREATE INDEX jobs_by_enabled_name ON jobs (enabled, name, id);
DROP INDEX jobs_by_connection;

CREATE INDEX backups_by_started_status_size ON backups (started_at, status, size_bytes);
CREATE INDEX backups_by_status_started_size ON backups (status, started_at, size_bytes);
CREATE INDEX backups_by_job_database ON backups (job_id, database_name, status, started_at);
