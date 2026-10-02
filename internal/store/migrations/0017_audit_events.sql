-- 0017_audit_events: the hash-chained audit log of every action.
--
-- One row per audited action (internal/auditlog): when (Unix nanoseconds, UTC), the
-- actor (kind, user ID and name, API key ID and name), the action (route pattern,
-- MCP tool or system action), its targets (path parameters, as a JSON object), the
-- HTTP status and outcome, the client address and user agent, how many identical
-- refusals the row stands for, and its hash: SHA-256 of the previous row's hash and
-- the row's canonical JSON. IDs are assigned by the application (previous ID + 1),
-- so a missing row is visible as a gap.
--
-- Rows are never updated. Retention deletes the oldest rows only after moving the
-- chain anchor (the single audit_chain_anchor row: its ID, hash and time) to the
-- newest deleted one, which the triggers enforce; verification starts from the
-- anchor. Request bodies are never stored.

CREATE TABLE audit_events (
    id             INTEGER PRIMARY KEY,
    at             INTEGER NOT NULL,
    actor_kind     TEXT    NOT NULL,
    actor_user_id  TEXT    NOT NULL DEFAULT '',
    actor_name     TEXT    NOT NULL DEFAULT '',
    actor_key_id   TEXT    NOT NULL DEFAULT '',
    actor_key_name TEXT    NOT NULL DEFAULT '',
    action         TEXT    NOT NULL,
    targets        TEXT    NOT NULL CHECK (json_valid(targets)),
    status         INTEGER NOT NULL,
    outcome        TEXT    NOT NULL,
    client_ip      TEXT    NOT NULL DEFAULT '',
    user_agent     TEXT    NOT NULL DEFAULT '',
    count          INTEGER NOT NULL DEFAULT 1,
    hash           TEXT    NOT NULL
) STRICT;

CREATE INDEX audit_events_by_time ON audit_events (at);

CREATE TABLE audit_chain_anchor (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    last_id   INTEGER NOT NULL,
    last_hash TEXT    NOT NULL,
    last_at   INTEGER NOT NULL,
    pruned_at INTEGER NOT NULL
) STRICT;

INSERT INTO audit_chain_anchor (id, last_id, last_hash, last_at, pruned_at)
VALUES (1, 0, '0000000000000000000000000000000000000000000000000000000000000000', 0, 0);

CREATE TRIGGER audit_events_append_only
BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events is append-only');
END;

CREATE TRIGGER audit_events_prune_behind_anchor
BEFORE DELETE ON audit_events
WHEN OLD.id > (SELECT last_id FROM audit_chain_anchor WHERE id = 1)
BEGIN
    SELECT RAISE(ABORT, 'audit_events rows are only deleted behind the chain anchor');
END;

CREATE TRIGGER audit_chain_anchor_forward_only
BEFORE UPDATE ON audit_chain_anchor
WHEN NEW.last_id < OLD.last_id
BEGIN
    SELECT RAISE(ABORT, 'the audit chain anchor only moves forward');
END;

CREATE TRIGGER audit_chain_anchor_kept
BEFORE DELETE ON audit_chain_anchor
BEGIN
    SELECT RAISE(ABORT, 'the audit chain anchor cannot be deleted');
END;
