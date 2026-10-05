-- 0023_oplog_chunk_deletes: soft deletes of oplog chunks (see docs/pitr.md).
--
-- PITR retention deletes chunks through the delete protection like backups: a
-- deleted chunk keeps its status (committed or superseded) and its object until
-- purge_after (Unix nanoseconds, UTC), the end of the delete grace period, and is
-- no longer part of any point-in-time window. The purge then removes the object and
-- marks the chunk pruned. deleted_at is when it was deleted. Both are NULL for a
-- chunk that is not deleted, so no existing row changes.

ALTER TABLE oplog_chunks ADD COLUMN deleted_at INTEGER;
ALTER TABLE oplog_chunks ADD COLUMN purge_after INTEGER;

CREATE INDEX oplog_chunks_purge ON oplog_chunks (purge_after) WHERE purge_after IS NOT NULL;
