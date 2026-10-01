-- 0010_list_filter_indexes: indexes for the filtered, paginated backup and restore
-- lists (GET /api/v1/backups and /api/v1/restores with filters, limit and offset).
--
-- Each list filter pairs with the default newest-first order, so a page of one job's,
-- one status's or one database's history is read straight from an index. The retry
-- index also serves the "latest retry of this backup" lookup of every listed row.
-- Indexes only: no data changes.

DROP INDEX IF EXISTS backups_by_job;
CREATE INDEX backups_by_job_started ON backups (job_id, started_at DESC, id DESC);

DROP INDEX IF EXISTS backups_by_status;
CREATE INDEX backups_by_status_started ON backups (status, started_at DESC, id DESC);

DROP INDEX IF EXISTS backups_by_retry_of;
CREATE INDEX backups_by_retry_of_started ON backups (retry_of, started_at DESC, id DESC);

DROP INDEX IF EXISTS restores_by_status;
CREATE INDEX restores_by_status_started ON restores (status, started_at DESC, id DESC);
CREATE INDEX restores_by_backup ON restores (backup_id, started_at DESC, id DESC);
CREATE INDEX restores_by_target ON restores (target_database, started_at DESC, id DESC);
