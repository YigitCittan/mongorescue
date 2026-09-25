-- 0008_backup_trigger: record how every backup was started.
--
-- Backup records gain a "trigger" (scheduled, on_demand, manual or mcp); retention
-- only prunes the scheduled backups of the job that runs it. Records written before
-- triggers existed are backfilled: a record that belongs to a job is treated as
-- scheduled, any other as manual.

UPDATE backups
SET data = json_set(data, '$.trigger', CASE WHEN job_id != '' THEN 'scheduled' ELSE 'manual' END)
WHERE json_type(data, '$.trigger') IS NULL;
