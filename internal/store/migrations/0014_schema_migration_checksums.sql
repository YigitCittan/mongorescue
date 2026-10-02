-- 0014_schema_migration_checksums: record what every applied migration contained.
--
-- schema_migrations gains a checksum column: the hex SHA-256 of the migration's SQL
-- as embedded in the binary that applied it. Rows written before this migration
-- start empty and are backfilled from the running binary's embedded migrations
-- right after the upgrade. From then on, opening the store fails when an applied
-- migration's embedded SQL no longer matches its recorded checksum.

ALTER TABLE schema_migrations ADD COLUMN checksum TEXT NOT NULL DEFAULT '';
