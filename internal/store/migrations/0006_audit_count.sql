-- 0006_audit_count: coalesced audit entries.
--
-- Repeated refused calls (denied, rate limited) of one API key are merged into one
-- entry per short window; count is the number of calls an entry stands for.

ALTER TABLE audit_log ADD COLUMN count INTEGER NOT NULL DEFAULT 1 CHECK (count >= 1);
