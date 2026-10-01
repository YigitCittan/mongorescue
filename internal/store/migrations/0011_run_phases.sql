-- 0011_run_phases: phase timestamps of backup and restore runs, and cancelled runs.
--
-- Backup and restore records gain a "phases" JSON column mirroring data.phases: the
-- times a run was queued, started, finished dumping, uploading, verifying or
-- restoring, and finished (see models.RunPhases). Records written before phases
-- existed are backfilled from their start and completion times. Runs can now end as
-- "cancelled" (the status column is free text, so no constraint changes); a cancelled
-- record names who cancelled it in data.cancelled_by and data.cancelled_at.

ALTER TABLE backups ADD COLUMN phases TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(phases));
ALTER TABLE restores ADD COLUMN phases TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(phases));

UPDATE backups
SET data = json_set(data, '$.phases', json_patch(
        json_object('queued', json_extract(data, '$.started_at')),
        CASE WHEN json_type(data, '$.completed_at') = 'text'
             THEN json_object('finished', json_extract(data, '$.completed_at'))
             ELSE '{}' END))
WHERE json_type(data, '$.phases') IS NULL AND json_type(data, '$.started_at') = 'text';

UPDATE restores
SET data = json_set(data, '$.phases', json_patch(
        json_object('queued', json_extract(data, '$.started_at')),
        CASE WHEN json_type(data, '$.completed_at') = 'text'
             THEN json_object('finished', json_extract(data, '$.completed_at'))
             ELSE '{}' END))
WHERE json_type(data, '$.phases') IS NULL AND json_type(data, '$.started_at') = 'text';

UPDATE backups SET phases = coalesce(json_extract(data, '$.phases'), '{}');
UPDATE restores SET phases = coalesce(json_extract(data, '$.phases'), '{}');
