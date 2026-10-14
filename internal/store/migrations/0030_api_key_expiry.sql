-- 0030_api_key_expiry: optional expiry of API keys (issue #190).
--
-- expires_at is when the key stops working (Unix nanoseconds); NULL never
-- expires, which every key stored before this migration keeps until an
-- administrator sets security.api_key_max_lifetime_days: then each key except the
-- imported one gets now + that lifetime (announced as api_key.deadline_set).
-- expiry_max_days is the maximum lifetime (days) the expiry was checked against
-- when the key was created or capped (NULL: none), so the same maximum never caps
-- a key twice, also not after the clock stepped back.
-- expiry_warned_for is the expiry the last api_key.expiring notice was sent for,
-- so each expiry is announced once and a changed expiry is announced again.
-- imported marks the key imported from MONGORESCUE_API_KEY, the only key that keeps
-- no expiry under a maximum lifetime: a key without a creator whose hash is a MAC
-- or whose lookup prefix is upper case ("K", "L"), which only imports have.
-- created_by_key is the API key that created a key ('' when a session did), so
-- keys minted by a key without a user are attributed.
-- Releases before this one refuse a database at this version.

ALTER TABLE api_keys ADD COLUMN expires_at INTEGER;
ALTER TABLE api_keys ADD COLUMN expiry_max_days INTEGER;
ALTER TABLE api_keys ADD COLUMN expiry_warned_for INTEGER;
ALTER TABLE api_keys ADD COLUMN imported INTEGER NOT NULL DEFAULT 0 CHECK (imported IN (0, 1));
ALTER TABLE api_keys ADD COLUMN created_by_key TEXT NOT NULL DEFAULT '';

UPDATE api_keys SET imported = 1
 WHERE created_by = ''
   AND (key_hash LIKE 'hmac-sha256:%' OR substr(prefix, 1, 1) IN ('K', 'L'));
