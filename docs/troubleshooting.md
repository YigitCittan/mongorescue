# Troubleshooting

## Unreadable records

MongoRescue keeps jobs, connections, storage targets, notification channels and rules, users, API keys and the backup and restore history in `mongorescue.db` (see [production.md](production.md#data-directory)). Most rows store their record as JSON in a `data` column. A row can become unreadable when its JSON no longer fits the record format (a hand edit, a damaged disk, or a value written in another shape by a different release) or when one of its encrypted fields cannot be decrypted.

Since 0.9.1, such a row no longer breaks the whole list it belongs to:

- every list skips the row and returns the other records, so the dashboard, the REST API and the scheduler keep working; jobs that can be read keep running on schedule, and only the unreadable job does not run until it is repaired;
- the row is **never deleted or rewritten**;
- the log has one `WARN` entry per row, `skipping a stored record that cannot be read`, with `table`, `id` and a `reason` that never quotes the stored data;
- administrators (dashboard sessions and `admin` API keys) see the rows in `corrupt_records` of `GET /api/v1/stats` (`table`, `id`, `error`), and the dashboard shows a banner, for example *1 record(s) could not be read (jobs: job_x)*. The banner clears by itself once the row is repaired or deleted.

The `error` names what does not fit, for example `field retention_count: a JSON string does not fit type int`, `a timestamp field is not a valid time` or `an encrypted field cannot be decrypted`.

### 1. Back up the database first

Take a copy before you touch anything, so a mistake can be undone:

```sh
sqlite3 /data/mongorescue.db ".backup '/backup/mongorescue-before-repair.db'"
```

or stop MongoRescue and copy the whole data directory (including `mongorescue.db-wal`). Then stop MongoRescue for the repair itself, so nothing else writes to the database meanwhile. In Docker, run `sqlite3` on the host against the data volume, or copy the file out with `docker cp`, repair the copy and put it back while the container is stopped.

### 2. Inspect the row

Replace `jobs` and `job_x` with the table and ID from the banner or the log:

```sh
sqlite3 /data/mongorescue.db
```

```sql
-- Is the JSON well formed, and how large is it?
SELECT id, json_valid(data) AS valid_json, length(data) AS bytes FROM jobs WHERE id = 'job_x';

-- The top-level fields and their JSON types (look for the field named in the error).
SELECT j.key, j.type, j.value FROM jobs, json_each(jobs.data) AS j WHERE jobs.id = 'job_x';

-- The whole record (json_pretty needs SQLite 3.46 or later; use "SELECT data" otherwise).
SELECT json_pretty(data) FROM jobs WHERE id = 'job_x';
```

Rows of `connections`, `notification_channels` and `storage_targets` contain credentials. They are encrypted (values starting with `sb2:`), but do not paste these rows into an issue: report the table, the ID and the `error` text instead. Never edit an encrypted value; each one is bound to its table, ID and field and stops decrypting when it is changed or moved.

### 3. Repair, export or remove the row

Export the record before changing it:

```sh
sqlite3 /data/mongorescue.db "SELECT data FROM jobs WHERE id = 'job_x'" > job_x.json
```

Fix the field named in the error with `json_set` (here a number stored as a string):

```sql
UPDATE jobs SET data = json_set(data, '$.retention_count', 7) WHERE id = 'job_x';
```

If the record cannot be fixed, remove it after exporting it and create it again in the dashboard:

```sql
DELETE FROM jobs WHERE id = 'job_x';
```

Start MongoRescue again. The row is checked on startup and on every dashboard refresh; once it reads, it leaves the banner and the log says `a stored record that could not be read is readable again`. For a repaired job, open it in the dashboard and save it once, so the indexed columns (`name`, `enabled`, `database_name`) match the JSON again.

If you need help, open an issue at <https://github.com/YigitCittan/mongorescue/issues> with the MongoRescue version, the table, the ID and the `error` text, never the row's content.
