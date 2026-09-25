# Using MongoRescue from AI assistants (MCP)

MongoRescue includes a [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server, so AI assistants such as Claude Desktop, Claude Code, VS Code (GitHub Copilot), Cursor or your own agents can inspect your backups and run backups and restores for you. Ask "did last night's backups succeed?", "why did the shop backup fail?" or "restore yesterday's backup of crm into a clone so I can check an order" and the assistant calls MongoRescue's tools.

It is built with the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk). For an introduction to MCP concepts (tools, resources, prompts, transports), see [MCP for Beginners](https://github.com/microsoft/mcp-for-beginners).

- [Safety model](#safety-model)
- [API key scopes](#api-key-scopes)
- [Transports](#transports)
- [Client configuration](#client-configuration): [Claude Desktop](#claude-desktop), [Claude Code](#claude-code), [VS Code](#vs-code), [Cursor](#cursor), [MCP Inspector](#mcp-inspector)
- [Tools](#tools), [resources](#resources), [prompts](#prompts)
- [Example conversations](#example-conversations)
- [Audit log, rate limit and metrics](#audit-log-rate-limit-and-metrics)
- [Troubleshooting](#troubleshooting)

## Safety model

An assistant is an automated client that acts on text it reads, so the MCP server is deliberately narrower than the REST API:

- **API keys only.** The MCP endpoint accepts an API key (`Authorization: Bearer <key>` or `X-API-Key`) and never a browser session cookie, so a web page cannot make your browser call it (no CSRF surface). Requests with a foreign `Origin` header are refused, and a server listening on a loopback address refuses requests that name a non-local host (DNS rebinding), unless `trust_proxy_headers` says a reverse proxy is in front.
- **Least privilege.** Every key has a [scope](#api-key-scopes); give an assistant a `read` key unless it must start operations. The tool list is filtered by scope, so a read key does not even see the action tools, and every call is checked again on the server.
- **Nothing destructive.** There is no tool that deletes anything, restores in place, drops collections or changes settings, users, API keys, connections, storage targets or encryption keys. Restores through MCP always go into a new database named `<db>_rescue_<timestamp>`; the tool has no parameter that could name another target. Those operations stay with a person in the dashboard.
- **No secrets.** Connection strings, passwords and storage credentials are never returned: connections are shown with their hosts only, storage targets without credentials, and error messages are redacted.
- **Accountable.** Every tool call is recorded in the audit log with the key, tool, arguments (secrets redacted), result and duration, and each key is rate limited.
- **Kill switch.** Turn the endpoint off under **Settings → Security → MCP server for AI assistants** (`security.mcp_enabled`). Revoking a key under **Settings → API keys** cuts off that assistant immediately.

Assistants may still be wrong. Review what they propose before you let them start a backup or a restore, and keep in-place recovery a human decision.

## API key scopes

Create a key under **Settings → API keys** and pick its scope (read is the default). Keys created before scopes existed are admin keys; consider replacing them with narrower ones.

| Scope | REST API | MCP tools |
| :--- | :--- | :--- |
| `read` | Every `GET` except the audit log | All read tools, resources and prompts |
| `operator` | `read` plus `POST /api/v1/backups`, `POST /api/v1/jobs/{id}/run` and safe-clone `POST /api/v1/restore` | `read` tools plus `start_backup`, `run_job`, `restore_to_safe_clone` |
| `admin` | Everything, including deletions, in-place restores, settings, users and keys | Same as `operator` (MCP has no admin-only tools) |

Browser sessions always have admin rights. See [api.md](api.md#api-key-scopes) for the route table.

## Transports

**Streamable HTTP** at `http(s)://<host>:8080/mcp`, on the same listener as the dashboard. The endpoint is stateless (every request is self-contained) and answers with JSON. Use it when the assistant can send an HTTP header, and always through HTTPS when it leaves the machine ([production.md](production.md)).

**stdio** with `mongorescue mcp`, for clients that launch a local command. The subcommand is a thin bridge: it speaks MCP on stdin/stdout and forwards every request to the `/mcp` endpoint of a running MongoRescue instance. It does not open the database or need the data directory, so it can run on a laptop against a remote instance.

```text
mongorescue mcp [--url http://127.0.0.1:8080] [--log-level warn]
```

| Setting | Flag | Environment variable | Default |
| :--- | :--- | :--- | :--- |
| Instance URL | `--url` | `MONGORESCUE_MCP_URL` | `http://127.0.0.1:8080` |
| API key | `--api-key` (discouraged: visible in the process list) | `MONGORESCUE_MCP_API_KEY` | required |
| Log level | `--log-level` | | `warn` |

The bridge sends the API key only to the configured scheme and host and never follows HTTP redirects: if the instance redirects (for example from `http` to `https`), set `--url` to the final address. Logs go to stderr only, because stdout carries the protocol. The bridge lists the tools once at start, so it shows exactly what the key's scope allows; restart the assistant after changing a key.

With the container image, run the bridge inside the container: `docker exec -i -e MONGORESCUE_MCP_API_KEY mongorescue mongorescue mcp` (the `-e NAME` form passes the variable from the environment of the `docker` command without putting the key on the command line).

## Client configuration

Replace `mr_...` with your key and `http://127.0.0.1:8080` with your instance. Prefer the client's secret storage or environment variables over pasting keys into files that end up in Git.

### Claude Desktop

Edit `claude_desktop_config.json` (**Settings → Developer → Edit Config**):

```json
{
  "mcpServers": {
    "mongorescue": {
      "command": "/usr/local/bin/mongorescue",
      "args": ["mcp", "--url", "http://127.0.0.1:8080"],
      "env": { "MONGORESCUE_MCP_API_KEY": "mr_..." }
    }
  }
}
```

With the container image instead of a local binary:

```json
{
  "mcpServers": {
    "mongorescue": {
      "command": "docker",
      "args": ["exec", "-i", "-e", "MONGORESCUE_MCP_API_KEY", "mongorescue", "mongorescue", "mcp"],
      "env": { "MONGORESCUE_MCP_API_KEY": "mr_..." }
    }
  }
}
```

### Claude Code

Streamable HTTP:

```bash
claude mcp add --transport http mongorescue http://127.0.0.1:8080/mcp \
  --header "Authorization: Bearer $MONGORESCUE_MCP_API_KEY"
```

stdio:

```bash
claude mcp add mongorescue --env MONGORESCUE_MCP_API_KEY="$MONGORESCUE_MCP_API_KEY" \
  -- mongorescue mcp --url http://127.0.0.1:8080
```

Add `--scope project` to share the server with your team through `.mcp.json` (without the key: each person supplies their own).

### VS Code

`.vscode/mcp.json` (GitHub Copilot agent mode); VS Code prompts for the key once and stores it securely:

```json
{
  "inputs": [
    { "type": "promptString", "id": "mongorescue-key", "description": "MongoRescue API key", "password": true }
  ],
  "servers": {
    "mongorescue": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer ${input:mongorescue-key}" }
    }
  }
}
```

stdio variant:

```json
{
  "servers": {
    "mongorescue": {
      "type": "stdio",
      "command": "mongorescue",
      "args": ["mcp", "--url", "http://127.0.0.1:8080"],
      "env": { "MONGORESCUE_MCP_API_KEY": "${input:mongorescue-key}" }
    }
  }
}
```

### Cursor

`~/.cursor/mcp.json` (or `.cursor/mcp.json` in a project), reading the key from your environment:

```json
{
  "mcpServers": {
    "mongorescue": {
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer ${env:MONGORESCUE_MCP_API_KEY}" }
    }
  }
}
```

stdio variant:

```json
{
  "mcpServers": {
    "mongorescue": {
      "command": "mongorescue",
      "args": ["mcp", "--url", "http://127.0.0.1:8080"],
      "env": { "MONGORESCUE_MCP_API_KEY": "${env:MONGORESCUE_MCP_API_KEY}" }
    }
  }
}
```

### MCP Inspector

The official [MCP Inspector](https://github.com/modelcontextprotocol/inspector) is handy to explore the server or script checks:

```bash
# Streamable HTTP
npx @modelcontextprotocol/inspector --cli http://127.0.0.1:8080/mcp --transport http \
  --header "Authorization: Bearer $MONGORESCUE_MCP_API_KEY" --method tools/list

# stdio bridge
npx @modelcontextprotocol/inspector --cli mongorescue mcp --method tools/call --tool-name get_status \
  -e MONGORESCUE_MCP_API_KEY="$MONGORESCUE_MCP_API_KEY" -e MONGORESCUE_MCP_URL=http://127.0.0.1:8080
```

Drop `--cli` for the interactive web UI.

## Tools

Every tool returns a one-line summary for the model and the full result as structured JSON (also as JSON text for clients without structured content). Annotations tell clients how careful to be: all tools have `destructiveHint: false` and `openWorldHint: false`; read tools are `readOnlyHint: true` and `idempotentHint: true`.

| Tool | Scope | Arguments | Result |
| :--- | :--- | :--- | :--- |
| `list_connections` | read | | Connections with hosts, description, last test (never the URI) |
| `list_databases` | read | `connection_id` | Databases of a connection (admin, config and local hidden) |
| `list_collections` | read | `connection_id`, `database` | Collections and views |
| `list_jobs` | read | `limit` (≤ 100), `cursor` | Scheduled jobs with last and next run |
| `get_job` | read | `id` | One job |
| `list_backups` | read | `database`, `connection_id`, `status`, `limit` (≤ 100), `cursor` | Backups, newest first |
| `get_backup` | read | `id` | One backup record (status, size, SHA-256, target, error) |
| `list_restores` | read | `limit` (≤ 100), `cursor` | Restores, newest first |
| `get_restore` | read | `id` | One restore record |
| `list_storage_targets` | read | | Storage targets without credentials |
| `get_status` | read | | Health, version, counts, running operations, last successful backup per job, failures in the last 24 hours |
| `start_backup` | operator | `connection_id`, `database`, optional `storage_target_id`, `collections`, `exclude_collections`, `gzip` | The new backup record (`in_progress`) |
| `run_job` | operator | `job_id` | The new backup record (`in_progress`) |
| `restore_to_safe_clone` | operator | `backup_id`, optional `target_connection_id`, `collections`, `verify` | The new restore record (`in_progress`) into `<db>_rescue_<timestamp>` |

Backups and restores run in the background: the action tools return at once and tell the model to poll `get_backup` or `get_restore` until the status is `completed` or `failed`. The same rules as in the REST API apply, because both call the same services: one backup per database at a time, one restore per target database, validation of names and filters, and a missing decryption key reported before anything starts. List tools return at most 100 items per call with a `next_cursor` for the next page.

## Resources

| URI | Contents |
| :--- | :--- |
| `mongorescue://status` | The `get_status` overview |
| `mongorescue://backups/{id}` | One backup record (template) |
| `mongorescue://jobs/{id}` | One scheduled job (template) |
| `mongorescue://restores/{id}` | One restore record (template) |

All resources are JSON and need the `read` scope.

## Prompts

| Prompt | Arguments | What it guides the assistant through |
| :--- | :--- | :--- |
| `diagnose_failed_backup` | `backup_id` | Read the record, the job and earlier runs, check the source and the storage target, classify the cause and propose next steps |
| `disaster_recovery_plan` | `database` | Pick a recovery point, estimate data loss, rehearse with `restore_to_safe_clone` (verify on), poll and report, explain the manual switch-over |
| `verify_recent_backups` | `hours` (default 24) | Check every job's last success in the window, list gaps, failures and suspicious size changes |

## Example conversations

> **You:** Did all backups succeed last night?
>
> **Assistant** *(calls `get_status`)*: 3 of 4 jobs have a successful backup in the last 24 hours. `crm-nightly` failed at 03:00 with "authentication failed"; its last good backup is two days old. Shall I diagnose it?

> **You:** Why did the crm backup fail?
>
> **Assistant** *(uses `diagnose_failed_backup`: `get_backup`, `get_job`, `list_backups`, `list_databases`)*: The connection "crm primary" answers, but `mongodump` was refused: the backup user's password changed on the 23rd, when the failures started. Update the connection's password in the dashboard, then I can run the job again.

> **You:** Restore yesterday's shop backup somewhere I can look at order 1042.
>
> **Assistant** *(calls `list_backups`, then `restore_to_safe_clone` with `verify: true`, then polls `get_restore`)*: Restored `bkp_shop_20260924_030000_3f9a1c2e` into the new database `shop_rescue_20260925_101500` (archive verified first). Your live `shop` database was not touched.

## Audit log, rate limit and metrics

- **Audit log.** Every tool call is stored with time, API key (ID and name), transport (`http`, or `stdio` for the bridge), tool, arguments (values of secret-looking keys and credentials in strings are masked), result (`ok`, `error`, `denied`, `rate_limited`), the error shown to the assistant, the duration and a `count`. The newest 10,000 entries are kept; repeated `denied` calls of one key and tool, and repeated `rate_limited` calls of one key, are merged into one entry per 10 seconds (`count` says how many calls it stands for), so refused calls cannot flush the log. See the last 200 under **Settings → Security → Recent API/MCP activity**, or call `GET /api/v1/audit?limit=200` with an admin key ([api.md](api.md#audit-log)).
- **Rate limit.** Each API key may make 60 tool calls, resource reads or prompt requests per minute with bursts of 20. Beyond that the server answers with JSON-RPC error `-32029` and a retry hint. The limit is checked before the scope, so calls refused for their scope count against it too.
- **Metrics.** `mongorescue_mcp_calls_total{tool, result}` counts calls ([metrics.md](metrics.md)); unknown tool names are counted as `tool="unknown"`.

## Troubleshooting

| Symptom | Cause and fix |
| :--- | :--- |
| `401 Unauthorized` | Missing or revoked key. Browser sessions do not work on `/mcp`; send `Authorization: Bearer <key>` |
| `403` "the MCP endpoint is disabled" | Turn on **Settings → Security → MCP server for AI assistants** |
| `403` "cross-origin request" | The client sent an `Origin` header of another site. Add it to `cors_origins` only if you trust that site |
| `403` "invalid Host header" | MongoRescue listens on loopback behind a reverse proxy that sets another `Host`: enable `trust_proxy_headers` |
| A tool is missing | The key's scope hides it (for example `start_backup` for a read key); restart the stdio client after changing keys |
| Tool error "forbidden: ... needs an API key with the "operator" scope" | Use an operator key for actions |
| JSON-RPC error `-32029` | Rate limit; wait the indicated seconds |
| The bridge exits with "cannot connect" | Check `--url` and that the instance is running; the bridge logs to stderr (`--log-level debug`) |
