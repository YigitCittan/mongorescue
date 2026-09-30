-- 0009_backup_retry_of: link a retried backup to the failed backup it retries.
--
-- Backup records gain an indexed "retry_of" column mirroring data.retry_of: the ID of
-- the failed backup a retry (POST /api/v1/backups/{id}/retry) was started from, or ''
-- for backups that are not retries. Retries never modify the original record, so the
-- failure history stays visible. Existing records are not retries.

ALTER TABLE backups ADD COLUMN retry_of TEXT NOT NULL DEFAULT '';
CREATE INDEX backups_by_retry_of ON backups (retry_of);
UPDATE backups SET retry_of = coalesce(json_extract(data, '$.retry_of'), '');
