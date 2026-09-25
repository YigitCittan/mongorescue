-- 0002_auth_connections: users, sessions, API keys, managed MongoDB connections and a
-- key/value settings table (secret key check value).
--
-- Credentials are never stored in plaintext: passwords as bcrypt hashes, session
-- tokens and API keys as SHA-256 hashes, connection URIs encrypted (secretbox) inside
-- the JSON data column. Timestamps are Unix nanoseconds (UTC).

CREATE TABLE settings (
    key   TEXT PRIMARY KEY NOT NULL,
    value TEXT NOT NULL
) STRICT;

CREATE TABLE users (
    id            TEXT    PRIMARY KEY NOT NULL,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    last_login_at INTEGER
) STRICT;

CREATE TABLE sessions (
    token_hash   TEXT    PRIMARY KEY NOT NULL,
    user_id      TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX sessions_by_user ON sessions (user_id);
CREATE INDEX sessions_by_expiry ON sessions (expires_at);

CREATE TABLE api_keys (
    id           TEXT    PRIMARY KEY NOT NULL,
    name         TEXT    NOT NULL,
    prefix       TEXT    NOT NULL UNIQUE,
    key_hash     TEXT    NOT NULL,
    created_by   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
) STRICT;

CREATE TABLE connections (
    id   TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    data TEXT NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX connections_by_name ON connections (name, id);

ALTER TABLE jobs ADD COLUMN connection_id TEXT NOT NULL DEFAULT '';
CREATE INDEX jobs_by_connection ON jobs (connection_id);

ALTER TABLE backups ADD COLUMN connection_id TEXT NOT NULL DEFAULT '';
CREATE INDEX backups_by_connection ON backups (connection_id, database_name);
