-- 0019_user_roles: dashboard roles of users.
--
-- Every user gains a role: viewer (read), operator (also backups, job runs and
-- safe-clone restores) or admin (everything). Users stored before this migration
-- were all administrators and keep that role. The column default only fills those
-- existing rows: the store always binds the role of a new user and refuses an empty
-- one, so the default can never grant admin to a user created later. Releases
-- before this one refuse a database at this version.

ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('viewer', 'operator', 'admin'));
