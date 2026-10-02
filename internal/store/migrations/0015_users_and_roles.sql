-- 0015_users_and_roles: jobs and backups that carry database users and roles.
--
-- A job gains data.include_users_and_roles: its dumps include the users and roles
-- defined on each database (mongodump --dumpDbUsersAndRoles). Every existing job
-- is set to false, which is what it did before. A backup records
-- data.users_and_roles when its archive contains them; earlier backups do not and
-- keep the field absent (false). Restores may only restore users and roles in place,
-- from a backup that has them.

UPDATE jobs
SET data = json_set(data, '$.include_users_and_roles', json('false'))
WHERE json_type(data, '$.include_users_and_roles') IS NULL;
