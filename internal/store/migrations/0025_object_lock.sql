-- 0025_object_lock: the S3 version and Object Lock retention of oplog chunks (see
-- docs/configuration.md, "Immutable backups").
--
-- version_id is the S3 version of the chunk's object on a versioned bucket ('' on
-- other targets): reads and the purge address that version, since deleting only the
-- key adds a delete marker and frees nothing. retain_until (Unix nanoseconds, UTC)
-- is the end of the object's Object Lock retention, NULL without a lock: storage
-- refuses to delete the object before, so a deleted chunk is purged only once both
-- purge_after and retain_until have passed. Backup records keep the same values in
-- their JSON document.
--
-- No existing row changes, so nothing is backfilled.

ALTER TABLE oplog_chunks ADD COLUMN version_id TEXT NOT NULL DEFAULT '';
ALTER TABLE oplog_chunks ADD COLUMN retain_until INTEGER;
