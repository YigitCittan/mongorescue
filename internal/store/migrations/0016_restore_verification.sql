-- 0016_restore_verification: post-restore verification and restore preflights.
--
-- Restore records gain a "verification" JSON column mirroring data.verification:
-- the comparison of the restored database with the backup's manifest (status
-- passed, failed or skipped, the mismatches and checked_at; see
-- models.RestoreVerification). It is NULL for restores that did not ask for one,
-- which includes every restore written before this migration, so nothing is
-- backfilled.
--
-- The preflight summary of a restore (data.preflight), whether it was forced past a
-- failed preflight (data.forced) and the server version a backup was taken from
-- (data.server_version) live in the records' JSON data and need no schema change.

ALTER TABLE restores ADD COLUMN verification TEXT CHECK (verification IS NULL OR json_valid(verification));
