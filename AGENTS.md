# AGENTS.md

MongoRescue is a MongoDB backup and restore tool written in Go: a single binary that streams `mongodump`/`mongorestore` archives to local or S3-compatible storage, with an embedded web dashboard, age encryption, notifications and Prometheus metrics.

## Commands

```bash
make build                     # binary in bin/
make test                      # unit tests
make test-race                 # unit tests with the race detector (run before every commit)
make test-coverage && make coverage-check   # coverage gate (60%)
make test-integration          # integration-tagged tests; needs MongoDB Database Tools and MONGORESCUE_TEST_* services
make test-integration-docker   # integration tests against disposable MongoDB, MinIO, LocalStack (Docker)
golangci-lint run              # lint (config: .golangci.yml)
go vet ./... && go vet -tags integration ./...
go mod tidy                    # must leave go.mod/go.sum unchanged
```

## Invariants

- Never buffer a dump: backup and restore data flows through `io.Reader`/`io.Writer` pipes; no `[]byte` archives, no full temp copies.
- Redact credentials: MongoDB URIs go through `internal/redact` before logs, errors or API output; mask every new secret in API responses; never log secrets.
- Run external tools with `exec.CommandContext` and argument slices, never a shell; pass the URI via `mongotools.WriteURIConfig`, not `--uri`.
- Safe-clone restores (`<db>_rescue_<timestamp>`) stay the default in user-facing flows; destructive options require explicit opt-in.
- Business packages (`backup`, `restore`, `scheduler`, `notify`) must not depend on `server` or `cmd`.
- Use `log/slog` for logging, pass `context.Context` to all I/O, wrap errors with `%w`, and expose sentinel errors.
- Every exported identifier has a Godoc comment; every package has a package comment.
- No orphan goroutines: bind lifecycles to a context and a WaitGroup or errgroup.
- The MongoDB Go driver is allowed only in `internal/mongoconn` and `_test.go` files.
- Credentials at rest (connection URIs, notification secrets) go through `internal/secretbox` in the store; passwords are bcrypt hashes, session tokens and API keys SHA-256 hashes. Auth rules live in `internal/auth`, not in HTTP handlers.
- Unit tests are hermetic (no network, no external binaries).

## Where things live

| Path | Contents |
| :--- | :--- |
| `cmd/mongorescue` | Entry point and flags |
| `internal/app` | Dependency wiring and lifecycle |
| `internal/config` | Bootstrap flags/env and the one-time import of deprecated env vars / config.json |
| `internal/settings`, `internal/targets` | Dashboard-managed settings (live, in the database) and storage targets |
| `internal/backup`, `internal/restore` | Backup and restore engines |
| `internal/scheduler` | Cron jobs and retention |
| `internal/storage` | `Storage` interface, local and S3 drivers |
| `internal/store` | SQLite metadata store (`mongorescue.db`), migrations, data directory lock |
| `internal/auth` | Setup mode, users, sessions, CSRF, login throttling, API keys |
| `internal/connections`, `internal/mongoconn` | Managed MongoDB connections and the driver adapter |
| `internal/secretbox` | AES-256-GCM encryption of stored credentials, `secret.key` |
| `internal/encryption` | age encryption |
| `internal/events`, `internal/notify`, `internal/metrics` | Event bus, notifications, Prometheus |
| `internal/server` | REST API, auth, dashboard serving |
| `internal/redact`, `internal/mongouri`, `internal/mongotools` | Credential scrubbing, URI validation, tool helpers |
| `internal/integration` | Integration tests (`integration` build tag) |
| `web/static` | Embedded dashboard |
| `docs/` | User and design documentation |

## Workflow

- Conventional Commits (`feat(scope): summary`), signed off with `git commit -s`.
- Update `README.md`, `docs/` and `CHANGELOG.md` when behaviour changes.
- Discuss large or architectural changes with the maintainer before implementing them.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full standards and [docs/architecture.md](docs/architecture.md) for the design.
