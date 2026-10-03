-- 0021_pitr: point-in-time recovery streams, oplog chains, chunks and collector state.
--
-- See docs/design/pitr.md. Positions in the oplog are BSON timestamps stored as
-- integer pairs: *_t the seconds and *_i the ordinal. Wall-clock times are Unix
-- nanoseconds (UTC), like every other table.
--
-- pitr_streams holds one row per replica set connection that collects its oplog
-- (connection_id is unique). oplog_max_days 0 means unset. data holds the remaining
-- options as JSON (base_on_gap, read_preference).
--
-- pitr_chains holds the unbroken runs of chunks of a stream. end_t, end_i, end_reason and
-- ended_at are NULL while a chain is open; a stream has at most one open chain.
--
-- oplog_chunks holds one row per stored range (from, to] of the oplog. A chunk is
-- committed when stored, superseded when it follows a divergence point, and pruned
-- once retention deleted its object. (target_id, storage_key) locates the object.
--
-- pitr_state holds the collector's position per stream: the end of its last
-- committed chunk on chain_id and the term of the entry there. It is written in the
-- same transaction as the chunk. No existing row changes, so nothing is backfilled.

CREATE TABLE pitr_streams (
    id              TEXT    PRIMARY KEY NOT NULL,
    connection_id   TEXT    NOT NULL UNIQUE,
    replica_set     TEXT    NOT NULL,
    target_id       TEXT    NOT NULL,
    enabled         INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    base_cron       TEXT    NOT NULL,
    base_keep_count INTEGER NOT NULL CHECK (base_keep_count >= 0),
    base_keep_days  INTEGER NOT NULL CHECK (base_keep_days >= 0),
    oplog_max_days  INTEGER NOT NULL CHECK (oplog_max_days >= 0),
    chunk_seconds   INTEGER NOT NULL CHECK (chunk_seconds > 0),
    data            TEXT    NOT NULL CHECK (json_valid(data)),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT;

CREATE TABLE pitr_chains (
    stream_id  TEXT    NOT NULL,
    chain_id   TEXT    NOT NULL,
    start_t    INTEGER NOT NULL,
    start_i    INTEGER NOT NULL,
    end_t      INTEGER,
    end_i      INTEGER,
    end_reason TEXT,
    ended_at   INTEGER,
    PRIMARY KEY (stream_id, chain_id),
    CHECK ((end_t IS NULL) = (ended_at IS NULL) AND (end_i IS NULL) = (ended_at IS NULL) AND (end_reason IS NULL) = (ended_at IS NULL))
) STRICT;

CREATE UNIQUE INDEX pitr_chains_open ON pitr_chains (stream_id) WHERE ended_at IS NULL;

CREATE TABLE oplog_chunks (
    id              TEXT    PRIMARY KEY NOT NULL,
    stream_id       TEXT    NOT NULL,
    chain_id        TEXT    NOT NULL,
    target_id       TEXT    NOT NULL,
    storage_key     TEXT    NOT NULL,
    from_t          INTEGER NOT NULL,
    from_i          INTEGER NOT NULL,
    to_t            INTEGER NOT NULL,
    to_i            INTEGER NOT NULL,
    first_term      INTEGER NOT NULL,
    last_term       INTEGER NOT NULL,
    entries         INTEGER NOT NULL CHECK (entries >= 0),
    size_bytes      INTEGER NOT NULL CHECK (size_bytes >= 0),
    sha256          TEXT    NOT NULL,
    encrypted       INTEGER NOT NULL CHECK (encrypted IN (0, 1)),
    encryption_mode TEXT    NOT NULL,
    status          TEXT    NOT NULL CHECK (status IN ('committed', 'superseded', 'pruned')),
    created_at      INTEGER NOT NULL,
    verified_at     INTEGER,
    verify_error    TEXT    NOT NULL DEFAULT '',
    UNIQUE (target_id, storage_key)
) STRICT;

CREATE INDEX oplog_chunks_by_chain ON oplog_chunks (stream_id, chain_id, from_t, from_i);

CREATE TABLE pitr_state (
    stream_id  TEXT    PRIMARY KEY NOT NULL,
    chain_id   TEXT    NOT NULL,
    last_t     INTEGER NOT NULL,
    last_i     INTEGER NOT NULL,
    last_term  INTEGER NOT NULL,
    status     TEXT    NOT NULL,
    last_error TEXT    NOT NULL DEFAULT '',
    lag_since  INTEGER,
    updated_at INTEGER NOT NULL
) STRICT;
