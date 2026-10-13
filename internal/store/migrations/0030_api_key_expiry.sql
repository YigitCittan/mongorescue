-- 0030_api_key_expiry: optional expiry of API keys (issue #190).
--
-- expires_at is when the key stops working (Unix nanoseconds); NULL never
-- expires, which every key stored before this migration keeps until an
-- administrator sets security.api_key_max_lifetime_days: then each key with a
-- creator gets now + that lifetime (announced as api_key.deadline_set).
-- expiry_warned_for is the expiry the last api_key.expiring notice was sent for,
-- so each expiry is announced once and a changed expiry is announced again.
-- Releases before this one refuse a database at this version.

ALTER TABLE api_keys ADD COLUMN expires_at INTEGER;
ALTER TABLE api_keys ADD COLUMN expiry_warned_for INTEGER;
