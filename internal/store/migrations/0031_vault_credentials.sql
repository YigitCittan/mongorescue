-- 0031_vault_credentials: credentials read from HashiCorp Vault (issue #62).
--
-- A connection ($.credential_source) or an S3 storage target
-- ($.s3.credential_source) whose source is 'vault' reads its credentials from
-- Vault when it is used, through the reference in $.vault or $.s3.vault (a
-- mount, a path or role and field names, never a secret). Nothing read from
-- Vault is ever stored. The generated columns expose the source of each row so
-- the startup check counts the rows that need Vault without opening any sealed
-- value; rows stored before this migration have none and are 'stored'.
-- Releases before this one refuse a database at this version: they would use a
-- connection whose URI holds no credentials.

ALTER TABLE connections ADD COLUMN credential_source TEXT
    GENERATED ALWAYS AS (coalesce(nullif(json_extract(data, '$.credential_source'), ''), 'stored')) VIRTUAL;
ALTER TABLE storage_targets ADD COLUMN credential_source TEXT
    GENERATED ALWAYS AS (coalesce(nullif(json_extract(data, '$.s3.credential_source'), ''), 'stored')) VIRTUAL;
