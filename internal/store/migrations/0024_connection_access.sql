-- 0024_connection_access: per-connection access of users and API keys.
--
-- all_connections = 1 allows every connection; otherwise connection_ids, a JSON
-- array of connection IDs, lists exactly the allowed connections, and an empty
-- array allows none: an empty list never means every connection. The column
-- defaults only fill the rows stored before this migration, which keep every
-- connection; the store always binds both columns of a new row. A user's access
-- limits their sessions and caps the API keys they created; a key's own access
-- limits it further. Administrators always have every connection: the store sets
-- it when a user becomes an admin. Releases before this one refuse a database at
-- this version.

ALTER TABLE users ADD COLUMN all_connections INTEGER NOT NULL DEFAULT 1 CHECK (all_connections IN (0, 1));
ALTER TABLE users ADD COLUMN connection_ids TEXT NOT NULL DEFAULT '[]';
ALTER TABLE api_keys ADD COLUMN all_connections INTEGER NOT NULL DEFAULT 1 CHECK (all_connections IN (0, 1));
ALTER TABLE api_keys ADD COLUMN connection_ids TEXT NOT NULL DEFAULT '[]';
