-- 0005_audit_log: the audit log of API/MCP activity.
--
-- One row per MCP tool call: when (Unix nanoseconds, UTC), which API key (ID and a
-- snapshot of its name), over which transport (http or stdio), the tool, its
-- arguments as JSON with secrets redacted, the result (ok, error, denied,
-- rate_limited), the error shown to the client and the duration. The application
-- keeps the newest entries and prunes older ones on insert.

CREATE TABLE audit_log (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    at           INTEGER NOT NULL,
    api_key_id   TEXT    NOT NULL,
    api_key_name TEXT    NOT NULL,
    transport    TEXT    NOT NULL,
    tool         TEXT    NOT NULL,
    arguments    TEXT    NOT NULL CHECK (json_valid(arguments)),
    result       TEXT    NOT NULL,
    error        TEXT    NOT NULL DEFAULT '',
    duration_ms  INTEGER NOT NULL
) STRICT;

CREATE INDEX audit_log_by_time ON audit_log (at);
