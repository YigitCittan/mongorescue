-- 0023_pitr_retention: soft deletes of oplog chunks and the persisted alert state
-- of the PITR collector (see docs/pitr.md).
--
-- PITR retention deletes chunks through the delete protection like backups: a
-- deleted chunk keeps its status (committed or superseded) and its object until
-- purge_after (Unix nanoseconds, UTC), the end of the delete grace period, and is
-- no longer part of any point-in-time window. The purge then removes the object and
-- marks the chunk pruned. deleted_at is when it was deleted. Both are NULL for a
-- chunk that is not deleted.
--
-- pitr_state.replica_set_id is the replica set ID (hex ObjectID) the collector
-- first saw; '' until then or when the user may not read the configuration. A
-- different ID after a restart is a gap. window_low_since is when the oplog
-- headroom dropped below its threshold (NULL while it does not), so a restart
-- does not raise pitr.window_low again, like lag_since for pitr.lag_high.
--
-- No existing row changes, so nothing is backfilled.

ALTER TABLE oplog_chunks ADD COLUMN deleted_at INTEGER;
ALTER TABLE oplog_chunks ADD COLUMN purge_after INTEGER;

CREATE INDEX oplog_chunks_purge ON oplog_chunks (purge_after) WHERE purge_after IS NOT NULL;

ALTER TABLE pitr_state ADD COLUMN replica_set_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pitr_state ADD COLUMN window_low_since INTEGER;
