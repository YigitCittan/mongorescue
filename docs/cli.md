# Command line

The `mongorescue` binary is also a client of a running MongoRescue instance: `mongorescue backup`, `restore`, `list`, `verify` and `status` call its [REST API](api.md) with an API key, for scripts, cron and CI. They never open a data directory, so they work from any machine that can reach the server, and they do nothing the API key's scope does not allow.

```bash
export MONGORESCUE_URL=https://backup.example.com
export MONGORESCUE_CLI_API_KEY_FILE=~/.config/mongorescue/key   # a key from Settings → API keys

mongorescue status
mongorescue backup --job job_1a2b3c4d --wait
mongorescue list backups --status failed --from 2026-10-01T00:00:00Z
mongorescue restore bkp_shop_20261001_030000_3f9a1c2e --wait --verify-restore
mongorescue verify bkp_shop_20261001_030000_3f9a1c2e --wait
```

Without one of these commands `mongorescue` runs the server as before, and `mongorescue mcp` runs the [MCP stdio bridge](mcp.md). `mongorescue help` lists the commands and `mongorescue help <command>` (or `<command> -h`) its flags. Until v0.16 `mongorescue help` was an unexpected argument of the server and exited `2`; it now prints this help and exits `0` (`mongorescue -h` still lists the server's flags).

## Connecting

| Flag | Environment variable | Meaning |
| :--- | :--- | :--- |
| `--url` | `MONGORESCUE_URL` | Base URL of the instance, default `http://127.0.0.1:8080`. A path prefix is kept (`https://host/mongorescue` for a reverse proxy). It must be `http` or `https` and carry no credentials, query or fragment. |
| `--api-key-file` | `MONGORESCUE_CLI_API_KEY_FILE` | A file holding the API key (surrounding whitespace is ignored) |
| | `MONGORESCUE_CLI_API_KEY` | The API key itself |
| `--api-key` | | The API key on the command line (discouraged: other users can read it in the process list) |

The key is taken from the first of `--api-key-file`, `MONGORESCUE_CLI_API_KEY_FILE`, `MONGORESCUE_CLI_API_KEY` and `--api-key` that is set; `--api-key` prints a warning, and a warning says when it is ignored. On Linux and macOS a key file that its group or other users may read or write (`mode & 0o077`, for example `0644`) prints a warning (`readable by other users; chmod 600`) and is still used; Windows is not checked. The CLI never reads `MONGORESCUE_API_KEY` or `MONGORESCUE_API_KEY_FILE`: the server imports those as an admin key, so a shell that has them set for the server does not lend them to the CLI.

The key is sent as `Authorization: Bearer` to the URL's origin only. Redirects are never followed (the command fails and names the redirect target; set `--url` to the final address), so a redirect cannot carry the key to another host. Sending the key over plain `http` to a host that is not this machine prints a warning; use `https`. Requests identify themselves with `User-Agent: mongorescue-cli/<version>` and `X-MongoRescue-Transport: cli`.

`HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` are honoured; with an `http://` URL the request, API key included, reaches the proxy in clear text (with `https://` the proxy only tunnels it).

Use the smallest scope that does the job ([scopes](api.md#api-key-scopes)): `read` for `list` and `status`, `operator` for `backup`, `verify` and restores into a safe clone, `admin` only for in-place and cross-connection restores.

## Shared flags

Every command takes `--url`, `--api-key-file`, `--api-key` and:

| Flag | Meaning |
| :--- | :--- |
| `--json` | Print the server's JSON (the response's `data`, indented) instead of text |
| `--quiet` | Print only IDs (one per line), no progress and no notes; warnings and errors still go to stderr |

`backup`, `restore` and `verify` also take:

| Flag | Meaning |
| :--- | :--- |
| `--wait` | Poll every 2 seconds until the run finishes, with progress lines on stderr, then print the final record. The exit code tells how it ended. |
| `--timeout DURATION` | With `--wait`: stop waiting after this long (`90s`, `30m`, `2h`); `0` (the default) waits until the run finishes |

`--timeout` and Ctrl-C stop only the waiting: the backup, restore or verification goes on on the server. The command exits `6` and prints the run's ID and the command that shows it later (`mongorescue list backups --id …`). To stop a run, cancel it in the dashboard or with `POST /api/v1/backups/{id}/cancel`. `verify --wait` stops waiting after one hour unless `--timeout` is given (`--timeout 0` waits until the result is in), since a verification the server never runs would leave nothing to wait for.

While waiting, a poll that fails transiently is retried, up to 3 failures in a row (a warning each time; a successful poll resets the count): a `429` waits for the server's `Retry-After` (at most 30 seconds, at least the poll interval), and a `5xx` or a network error is retried after the poll interval (or its `Retry-After`). The fourth failure in a row ends the command with that failure's exit code (`7`, or `1` for `429`); the run goes on on the server. Any other refusal (`401`, `403`, `404`) ends it at once.

Ctrl-C at other moments:

| When | Exit | What happened |
| :--- | :--- | :--- |
| While `--wait` polls | `6` | The run started and goes on; its ID and a `mongorescue list` command are printed |
| During the request that starts the run (`backup`, `backup --job`, `restore`, `verify`), before its answer arrived | `1` | `cancelled before the request completed; the run may or may not have started, check mongorescue list …`: the server may have received the request, so look before you start it again |
| Before that request (for example during the restore preflight, or in `list` and `status`) | `1` | `interrupted before the request completed`: nothing was started |

A start request that fails with a network error after it was sent (exit `7`) may likewise have started the run; check `mongorescue list` before retrying.

Flags may come before or after the arguments: `mongorescue restore bkp_1 --dry-run` and `mongorescue restore --dry-run bkp_1` are the same. Everything after `--` is an argument.

Results go to stdout; progress, warnings and errors to stderr. Connection strings in the output are redacted.

## Commands

### backup

```bash
mongorescue backup --job ID [--wait]
mongorescue backup --connection ID --database NAME [--collections a,b | --exclude-collections a,b]
                   [--storage-target ID] [--gzip=false] [--users-and-roles] [--wait]
mongorescue backup --connection ID --database NAME --database NAME… [--parallelism N] [flags]
mongorescue backup --connection ID --databases NAME,NAME… [--parallelism N] [flags]
```

`--job` runs a scheduled job now (`POST /api/v1/jobs/{id}/run`) with its own settings; the other flags cannot be combined with it. For a job with several databases it waits for the whole run and prints each database's outcome. Otherwise it starts an on-demand backup of one database (`POST /api/v1/backups`): `--storage-target` defaults to the default target and `--gzip` to the server's setting. Without `--wait` it prints the new backup (or run) and returns at once. With `--wait` it exits `1` when the backup failed or was cancelled, or when any database of a job run did not complete.

`--database` repeated, or `--databases` with a comma-separated list, backs up several databases of the connection in one run ([details](api.md#backing-up-several-databases-now)): each into its own backup, `--parallelism` (1 to 4, default 1) at a time. `--collections` and `--exclude-collections` apply to one database only. A database another backup is running is skipped and named on stderr. Without `--wait` it prints the run's ID and its backups (`--quiet`: the run ID; `--json`: the server's answer). With `--wait` it waits for all of them, prints a summary per database and exits `0` only when every database was backed up, `1` otherwise (a skipped database counts as not backed up); `--json` then prints `{run_id, backups, busy}` with the final records. Follow a run later with `mongorescue list backups --run RUN_ID`.

```bash
mongorescue backup --connection conn_prod --databases shop,crm,billing --parallelism 2 --wait
```

### restore

```bash
mongorescue restore BACKUP_ID [--target-connection ID] [--collections a,b] [--dry-run]
                              [--verify-archive | --no-verify-archive] [--verify-restore]
                              [--skip-preflight] [--force] [--wait]
mongorescue restore BACKUP_ID --in-place --confirm [--target-database NAME] [--drop] [...]
```

A restore goes into a new safe clone database (`<db>_rescue_<timestamp>`) unless `--in-place` is given, and never touches existing data then. `--in-place` restores into the backup's own database (or `--target-database`) and needs `--confirm`: without it the command exits `2` before it sends anything. It never prompts, so a script cannot hang on a question. `--target-database` and `--drop` (drop each restored collection in the target first) need `--in-place --confirm`; `--confirm` alone is refused too.

| Flag | Request field |
| :--- | :--- |
| `--target-connection ID` | `target_connection_id`: restore into another connection (admin) |
| `--collections a,b` | `selected_collections` |
| `--dry-run` | `dry_run`: check without writing |
| `--verify-archive`, `--no-verify-archive` | `verify`: check the archive's checksum first, or not (in-place restores are always verified); neither leaves it to the server's policy |
| `--verify-restore` | `verify_restore`: compare the restored database with the backup's manifest afterwards ([details](api.md#restore-verification)) |
| `--force` | `force`: restore although a preflight check failed |

The [restore preflight](api.md#restore-preflight) runs first (`POST /api/v1/restores/preflight`). Warnings are printed to stderr and do not stop the restore. A failed check prints the checks (a table on stderr, or the preflight's JSON on stdout with `--json`) and exits `1` without starting the restore, unless `--force` is given. `--skip-preflight` skips this first call; the server still runs the checks when the restore starts and refuses it in the same way (exit `1` with the checks), unless `--force`.

With `--wait` it exits `1` when the restore failed or was cancelled, and also when `--verify-restore` found a mismatch (the data is restored, but it does not match what the backup recorded).

### list

```bash
mongorescue list backups|restores|jobs|connections|targets [filters]
```

Backups and restores are listed newest first, 50 at a time: `--limit` (1 to 200, `0` for every match) and `--offset` page through them, and a note on stderr says when there are more. The filters map to the [list parameters](api.md#listing-backups-and-restores) of the API:

| Flag | Applies to | Parameter |
| :--- | :--- | :--- |
| `--id ID,…` | backups, restores | `id` |
| `--status S` | backups, restores | `status` (deleted and purged backups are listed only when asked for) |
| `--deleted` | backups | `deleted=true`: the deleted backups that can still be undone |
| `--database NAME` | backups, restores (target), jobs | `database` |
| `--connection ID` | backups, jobs | `connection_id` |
| `--job ID` | backups | `job_id` |
| `--run ID` | backups | `run_id`: the backups of one run (a job run or a backup of several databases) |
| `--trigger T` | backups | `trigger` |
| `--backup ID` | restores | `backup_id` |
| `--from`, `--to` | backups, restores | `from`, `to` (RFC 3339) |
| `--search TEXT` | backups, restores, jobs | `q` |
| `--sort COLUMN` | backups, restores | `sort` |
| `--enabled BOOL`, `--schedule S`, `--last-status S` | jobs | `enabled`, `schedule`, `last_status` |

A filter that does not apply to the listed resource is a usage error (exit `2`). `--json` prints the server's array unchanged; `--quiet` prints the IDs. Connection strings are shown redacted.

### verify

```bash
mongorescue verify BACKUP_ID [--wait]
```

Starts a [verification](verification.md) of the backup's archive (`POST /api/v1/backups/{id}/verify`): the server re-reads it from storage and compares it with the checksum recorded at backup time. With `--wait` it polls until the backup's `verified_at` changes and exits `1` on a `mismatch` or an `error`; it stops waiting after one hour (exit `6`) unless `--timeout` is given.

### status

```bash
mongorescue status [--readiness]
```

Shows the server's health and version, the user the key belongs to, the key's effective scope, backup, restore and job counts, the last backup and the runs in progress (`GET /api/v1/health`, `/auth/me`, `/stats`, `/runs/active`). Servers before v0.17 do not report the scope; the line then says so. `--readiness` adds the [recovery readiness](api.md#recovery-readiness) of every database a job backs up and exits `1` when one of them fails it, which makes `mongorescue status --readiness --quiet` a one-line health check. `--json` prints `{url, health, me, stats, active_runs, readiness}` with the server's answers as they are.

## Exit codes

The exit codes are a stable contract:

| Code | Meaning |
| :--- | :--- |
| `0` | Success |
| `1` | The operation failed: a backup, restore or verification failed, a restore preflight failed, a readiness check failed, or the server refused the request as invalid (`400`, `422`, `429`), or Ctrl-C before a run was known to have started |
| `2` | Usage: unknown command or flag, missing or extra arguments, `--in-place` without `--confirm`, an invalid URL, no API key |
| `3` | The server refused the API key (`401`) or its scope does not allow the request (`403`) |
| `4` | Not found (`404`): the backup, restore or job does not exist |
| `5` | Conflict (`409`): for example a backup of the same database is already running |
| `6` | `--wait` stopped (`--timeout`, the one-hour default of `verify --wait`, or Ctrl-C) before the run finished; it goes on on the server |
| `7` | The server is unreachable, answered `5xx`, redirected, or is not a MongoRescue API at the URL |

## CI examples

Keep the key in your CI system's secret store and hand it to the CLI as a file, so it never appears in the command line or the job log. An operator key is enough for backups, verifications and safe-clone restores.

GitHub Actions, backing up before a deployment and failing the job when the backup fails:

```yaml
- name: Back up the shop database
  env:
    MONGORESCUE_URL: https://backup.example.com
    MONGORESCUE_KEY: ${{ secrets.MONGORESCUE_OPERATOR_KEY }}
  run: |
    umask 077
    printf '%s' "$MONGORESCUE_KEY" > "$RUNNER_TEMP/mongorescue.key"
    export MONGORESCUE_CLI_API_KEY_FILE="$RUNNER_TEMP/mongorescue.key"
    mongorescue backup --job job_shop_nightly --wait --timeout 30m
```

GitLab CI, with the key in a variable of type *File* (GitLab writes it to a file and puts the path in the variable), rehearsing a restore into a safe clone every night:

```yaml
restore-drill:
  variables:
    MONGORESCUE_URL: https://backup.example.com
  script:
    - export MONGORESCUE_CLI_API_KEY_FILE="$MONGORESCUE_OPERATOR_KEY"
    - BACKUP=$(mongorescue list backups --job job_shop_nightly --status completed --limit 1 --quiet)
    - mongorescue restore "$BACKUP" --wait --verify-restore --timeout 1h
```

A cron or monitoring check that alerts when a database misses its recovery point objective:

```bash
MONGORESCUE_CLI_API_KEY_FILE=/etc/mongorescue/read.key mongorescue status --readiness --quiet || alert "MongoRescue readiness"
```

Use `--json` with `jq` for anything else, for example the size of the newest completed backup:

```bash
mongorescue list backups --status completed --limit 1 --json | jq '.[0].size_bytes'
```
