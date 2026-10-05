-- 0024_connection_access: per-connection access of users and API keys.
--
-- connection_ids is a JSON array of connection IDs. An empty array, the default,
-- means every connection, so every user and API key stored before this migration
-- keeps the access it had. A user's list limits their sessions and caps the API keys
-- they created; a key's own list limits it further. Administrators are never
-- limited: the store clears the list when a user becomes an admin. Deleting a
-- connection does not shorten the lists, so a limit never widens to every
-- connection by itself. Releases before this one refuse a database at this version.

ALTER TABLE users ADD COLUMN connection_ids TEXT NOT NULL DEFAULT '[]';
ALTER TABLE api_keys ADD COLUMN connection_ids TEXT NOT NULL DEFAULT '[]';
