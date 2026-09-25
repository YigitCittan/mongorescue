-- 0003_settings_storage_targets: dashboard-managed settings and storage targets.
--
-- Settings live in the existing key/value `settings` table (one row per setting key,
-- JSON-encoded value; secret values sealed with secretbox), next to the secret key
-- check value. Storage targets keep their S3 secret key sealed inside `data`.
--
-- Jobs and backup records reference their storage target. Existing rows keep an empty
-- storage_target_id here; at startup, once the default target exists, they are
-- assigned to it (the application knows the target, this migration does not).

CREATE TABLE storage_targets (
    id         TEXT    PRIMARY KEY NOT NULL,
    name       TEXT    NOT NULL,
    is_default INTEGER NOT NULL DEFAULT 0,
    data       TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX storage_targets_by_name ON storage_targets (name, id);
-- At most one default target.
CREATE UNIQUE INDEX storage_targets_one_default ON storage_targets (is_default) WHERE is_default = 1;

ALTER TABLE jobs ADD COLUMN storage_target_id TEXT NOT NULL DEFAULT '';
CREATE INDEX jobs_by_storage_target ON jobs (storage_target_id);

ALTER TABLE backups ADD COLUMN storage_target_id TEXT NOT NULL DEFAULT '';
CREATE INDEX backups_by_storage_target ON backups (storage_target_id, status);

-- Rows written by builds that already recorded a target in the JSON document.
UPDATE jobs SET storage_target_id = coalesce(json_extract(data, '$.storage_target_id'), '');
UPDATE backups SET storage_target_id = coalesce(json_extract(data, '$.storage_target_id'), '');
