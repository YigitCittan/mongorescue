-- 0001_init: initial MongoRescue metadata schema.
--
-- Every table keeps the complete record as JSON in `data`; the model is always read
-- from there, so fields can be added to the Go models without a migration. The other
-- columns are copies of the fields used for filtering and ordering and are rewritten
-- on every save. Timestamps used for ordering are Unix nanoseconds (UTC).

CREATE TABLE jobs (
    id            TEXT    PRIMARY KEY NOT NULL,
    name          TEXT    NOT NULL,
    database_name TEXT    NOT NULL,
    enabled       INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    data          TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX jobs_by_name ON jobs (name, id);

CREATE TABLE backups (
    id            TEXT    PRIMARY KEY NOT NULL,
    job_id        TEXT    NOT NULL,
    database_name TEXT    NOT NULL,
    status        TEXT    NOT NULL,
    started_at    INTEGER NOT NULL,
    data          TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX backups_by_started ON backups (started_at DESC, id DESC);
CREATE INDEX backups_by_database ON backups (database_name, started_at DESC, id DESC);
CREATE INDEX backups_by_job ON backups (job_id);
CREATE INDEX backups_by_status ON backups (status);

CREATE TABLE restores (
    id              TEXT    PRIMARY KEY NOT NULL,
    backup_id       TEXT    NOT NULL,
    source_database TEXT    NOT NULL,
    target_database TEXT    NOT NULL,
    status          TEXT    NOT NULL,
    started_at      INTEGER NOT NULL,
    data            TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX restores_by_started ON restores (started_at DESC, id DESC);
CREATE INDEX restores_by_status ON restores (status);

-- Channel data includes delivery secrets (webhook secrets, bot tokens, SMTP and Twilio
-- credentials) in plain text; the database file is created with mode 0600.
CREATE TABLE notification_channels (
    id      TEXT    PRIMARY KEY NOT NULL,
    name    TEXT    NOT NULL,
    type    TEXT    NOT NULL,
    enabled INTEGER NOT NULL,
    data    TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX notification_channels_by_name ON notification_channels (name, id);

CREATE TABLE notification_rules (
    id      TEXT    PRIMARY KEY NOT NULL,
    name    TEXT    NOT NULL,
    enabled INTEGER NOT NULL,
    data    TEXT    NOT NULL CHECK (json_valid(data))
) STRICT;

CREATE INDEX notification_rules_by_name ON notification_rules (name, id);
