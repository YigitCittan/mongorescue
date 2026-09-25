-- 0007_audit_http_status: REST requests in the audit log.
--
-- REST requests authenticated by an API key are audited too (transport "rest", the
-- route pattern as the tool); http_status is their response status, 0 for MCP calls.

ALTER TABLE audit_log ADD COLUMN http_status INTEGER NOT NULL DEFAULT 0;
