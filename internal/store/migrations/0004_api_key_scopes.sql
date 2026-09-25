-- 0004_api_key_scopes: every API key gets a scope (read < operator < admin).
--
-- Keys created before scopes existed had full rights, so they become admin keys; the
-- application creates new keys with the read scope unless another one is chosen.

ALTER TABLE api_keys ADD COLUMN scope TEXT NOT NULL DEFAULT 'admin'
    CHECK (scope IN ('read', 'operator', 'admin'));
