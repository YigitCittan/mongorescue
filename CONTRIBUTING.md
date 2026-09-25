# Contributing to MongoRescue

Thanks for your interest in MongoRescue. Bug fixes, documentation, tests and new storage drivers are all welcome. For larger changes (new subsystems, API changes, new dependencies), please open an issue or discussion first so the design can be agreed before you write the code.

- [Code of Conduct](#code-of-conduct)
- [Workflow](#workflow)
- [Building and testing](#building-and-testing)
- [Engineering standards](#engineering-standards)
- [Security rules](#security-rules)
- [Commits, pull requests and releases](#commits-pull-requests-and-releases)

For how the code is organized, read [docs/architecture.md](docs/architecture.md).

## Code of Conduct

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md). Please treat fellow contributors with respect.

## Workflow

External contributions use the fork-and-pull model; direct pushes to `YigitCittan/mongorescue` are limited to maintainers.

```bash
# 1. Fork on GitHub, then clone your fork and add the upstream remote
git clone https://github.com/<your-username>/mongorescue.git
cd mongorescue
git remote add upstream https://github.com/YigitCittan/mongorescue.git

# 2. Branch from upstream/main (never open a PR from your fork's main)
git fetch upstream
git checkout -b feat/my-change upstream/main

# 3. Keep the branch current by rebasing, not merging
git fetch upstream
git rebase upstream/main
git push origin feat/my-change --force-with-lease
```

Branch prefixes: `feat/`, `fix/`, `perf/`, `refactor/`, `docs/`, `test/`, `ci/`, `chore/`, e.g. `feat/gcs-storage-driver`, `fix/s3-path-style-url`.

When the branch is ready, open a pull request against `main` and fill in the template. `main` must always be green and releasable.

## Building and testing

Requirements: Go 1.26+, `make`, and [MongoDB Database Tools](https://www.mongodb.com/docs/database-tools/) 100.3.0+ for integration tests. Linting uses [golangci-lint](https://golangci-lint.run) with the repository's `.golangci.yml`.

```bash
make build                       # local binary in bin/
make test-race                   # unit tests with the race detector
make test-coverage && make coverage-check   # fails below 60% coverage
golangci-lint run                # lint
go vet ./... && go vet -tags integration ./...
make cross-compile               # Linux, macOS, Windows
```

**Unit tests** need only Go and must stay hermetic: no network, no external binaries, no live databases or buckets. Use fakes of `storage.Storage` and in-memory readers. Prefer table-driven tests.

**Integration tests** live in `internal/integration` behind the `integration` build tag. They drive the real `mongodump`/`mongorestore` and any services named in `MONGORESCUE_TEST_*` variables, and skip what is not configured (see [Test commands and variables](#test-commands-and-variables)):

```bash
make test-integration-docker                 # MongoDB 7 + MinIO + LocalStack in Docker
IT_PROVIDERS=minio make test-integration-docker
make test-integration                        # against services you configured yourself
make docker-smoke                            # build the image and smoke-test it
```

The MongoDB Go driver (`go.mongodb.org/mongo-driver/v2`) may be imported **only from `_test.go` files**; CI fails if it reaches the binary. New storage drivers must pass the shared conformance suite (`runStorageConformance`).

### What CI checks

On every push and pull request (`.github/workflows/ci.yml`):

- **Lint**: `go vet` (with and without the `integration` tag), `golangci-lint`, the driver-not-in-binary check, and `go mod tidy` with no resulting diff.
- **Test**: unit tests with `-race` on `ubuntu-latest` and `macos-latest` with Go 1.26.x (the `go.mod` minimum and release toolchain) and 1.27.x; coverage gate and report upload on Ubuntu.
- **Cross-compilation**: `make cross-compile` for Linux, macOS and Windows.
- **Integration**: the integration suite against MongoDB 7 with MinIO and with LocalStack.
- **Docker smoke test**: builds the image and checks health, auth, dashboard and bundled tools.

The cloud provider suite runs only on pushes to `main` and on `v*` tags in `YigitCittan/mongorescue`, never on pull requests. You do not need cloud credentials to contribute.

#### Maintainers: cloud provider secrets

Each provider is enabled by adding repository secrets; providers with missing secrets are skipped:

| Provider | Secrets (`<P>` = `AWS`, `R2`, `B2`, `SPACES`, `WASABI`) |
|---|---|
| All | `MR_TEST_<P>_BUCKET`, `MR_TEST_<P>_ACCESS_KEY`, `MR_TEST_<P>_SECRET_KEY` (required), `MR_TEST_<P>_ENDPOINT`, `MR_TEST_<P>_REGION`, `MR_TEST_<P>_PATH_STYLE` (optional) |

Examples: `MR_TEST_R2_ENDPOINT=https://<account>.r2.cloudflarestorage.com` with `MR_TEST_R2_REGION=auto`; `MR_TEST_B2_ENDPOINT=https://s3.<region>.backblazeb2.com`; `MR_TEST_SPACES_ENDPOINT=https://<region>.digitaloceanspaces.com`; `MR_TEST_WASABI_ENDPOINT=https://s3.<region>.wasabisys.com`; leave `MR_TEST_AWS_ENDPOINT` empty for AWS. Use a dedicated, empty test bucket with credentials scoped to it: the tests write under unique prefixes (`it-conformance/…`, `it_*/…`) and delete them afterwards.

### Automated checks

Beyond `ci.yml`, every pull request is checked by CodeQL (Go and the web UI), dependency review (fails on new high-severity advisories), a Conventional Commits title check and path-based area labels. The CI `security` job runs `govulncheck`, `actionlint` and `shellcheck` (`make vulncheck lint-ci` locally), and the Docker smoke job fails on fixable HIGH/CRITICAL findings from Trivy. OpenSSF Scorecard runs weekly. Workflow actions are pinned to commit SHAs and Dependabot keeps Go modules, actions and base images current. Releases ship SPDX SBOMs and GitHub build provenance attestations for archives and container images (`gh attestation verify <file> --repo YigitCittan/mongorescue`).

## Engineering standards

### Architecture

- **Keep the boundaries.** `backup`, `restore`, `scheduler` and `notify` contain business logic and must not import `server` or `cmd`, or depend on HTTP types, CLI flags or cloud SDK specifics. They depend on small interfaces such as `storage.Storage`.
- **Inject dependencies.** No package-level mutable singletons. Pass dependencies through constructors or functional options, and accept interfaces so components can be tested with fakes.
- **Stream, never buffer.** Backup and restore data moves through `io.Reader`/`io.Writer` between the tool process and storage. Never read an archive into `[]byte` or stage a full copy on disk. Memory use must not grow with dump size.
- **Single binary.** Dashboard assets live in `web/static` and are embedded with `//go:embed`. Builds must work with `CGO_ENABLED=0` and must not require Node.js or npm.
- **Standard library first.** `net/http`, `log/slog`, `context`, `crypto`, `os/exec`. Adding a module needs a clear reason in the PR description.

### Go conventions

- **Routing**: `net/http` `ServeMux` with method patterns, e.g. `mux.HandleFunc("GET /api/v1/backups", h)`. No third-party routers.
- **Logging**: `log/slog` only, with structured attributes. No `fmt.Println` or `log.Print` outside `cmd/`.
- **Context**: every function that does I/O takes a `context.Context` as its first argument and honours cancellation.
- **Errors**: wrap with `%w` and a short description (`fmt.Errorf("save backup record: %w", err)`); expose sentinel errors (`ErrNotFound`, `ErrChecksumMismatch`, ...) for expected conditions; inspect with `errors.Is` / `errors.As`. Do not discard errors silently; if ignoring one is intentional, say why in a comment.
- **Concurrency**: no orphan goroutines. Every goroutine has an owner and a lifecycle bound to a context, `sync.WaitGroup` or `errgroup`, and shuts down gracefully. Shared state is protected and tests run with `-race`.
- **Resources**: `defer` closes of files, response bodies and pipes immediately after the error check. Capture tool stderr for diagnostics, and make sure cancelled contexts terminate child processes.
- **Documentation**: every exported identifier has a Godoc comment starting with its name, and every package has a package comment.
- **API responses**: handlers use the shared helpers, which produce `{"success": true, "data": ...}` or `{"success": false, "error": "<message>"}` with a matching HTTP status.

## Security rules

- **Redact credentials.** MongoDB URIs pass through `internal/redact` before they reach logs, error messages, API responses or the UI. New secrets (tokens, passwords, keys) must be masked in API output and never logged. Tests should assert that the password does not leak.
- **No shell.** Start external tools with `exec.CommandContext(ctx, name, args...)` and separate argument slices. Never build a command string. Pass the connection URI through `mongotools.WriteURIConfig`, not `--uri`, so it is not visible in the process list.
- **Safe restores.** Keep safe-clone restores (`<db>_rescue_<timestamp>`) as the default in user-facing flows. Destructive options such as `drop_target` must require an explicit opt-in, and in-place restores must go through verify-before-restore unless the user disables it.
- **Contain paths.** Storage keys are validated against the storage root to prevent traversal. Stored backups are write-once: never modify an artifact in place.
- **No secrets in Git.** Connection strings, passwords and cloud credentials never go into commits, fixtures or test logs.

Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md).

## Commits, pull requests and releases

### Conventional Commits

```
<type>(<scope>): <imperative summary>

[optional body: what and why]

[optional footers, e.g. Closes #12]
Signed-off-by: Your Name <you@example.com>
```

- Types: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `chore`, `ci`.
- Imperative mood ("add", not "added"), no trailing period, subject under 72 characters.
- One logical change per commit; do not mix reformatting with behaviour changes.

### DCO sign-off

Every commit needs a Developer Certificate of Origin sign-off, certifying that you have the right to submit the change under the [MIT License](LICENSE):

```bash
git commit -s -m "feat(storage): add Google Cloud Storage backend"
```

### Pull request checklist

A pull request is merged when:

1. CI is green (lint, race-enabled tests on both OSes, coverage gate, integration, Docker smoke).
2. New I/O paths stream and do not buffer whole payloads.
3. New exported identifiers are documented.
4. Credentials are redacted and no secrets are committed.
5. User-visible changes are reflected in `README.md`, `docs/` and `CHANGELOG.md` (in an `[Unreleased]` section, created if missing).

Maintainers merge with **Squash and merge** or **Rebase and merge** to keep `main` linear. Never force-push `main`.

### Versioning and releases

MongoRescue follows [Semantic Versioning](https://semver.org): `MAJOR` for incompatible API, storage-format or flag changes; `MINOR` for backwards-compatible features; `PATCH` for fixes. While the version is `0.x`, minor releases may still contain breaking changes, which the changelog calls out.

Releases are cut by pushing an **annotated** tag:

```bash
git tag -a v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0
```

The tag triggers:

- **GoReleaser** (`.github/workflows/release.yml`): static binaries for Linux, macOS (amd64, arm64) and Windows (amd64, arm64) with version metadata, archives named `mongorescue_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows) containing `README.md`, `LICENSE` and `scripts/`, a `checksums.txt` with SHA-256 sums, and the GitHub Release.
- **Container images** (`.github/workflows/docker.yml`): multi-arch (`linux/amd64`, `linux/arm64`) images at `ghcr.io/yigitcittan/mongorescue`, tagged `<version>` and `<major>.<minor>`. Pushes to `main` publish `latest`; every build also gets `sha-<commit>`.

Before tagging, make sure `main` is green, `CHANGELOG.md` has a section for the version, and `git status` is clean.

## Getting help

- Architecture questions and proposals: GitHub **Discussions**.
- Bugs and feature requests: GitHub **Issues**, using the templates.

## Test commands and variables

| Command | What it runs | Needs |
|---|---|---|
| `make test` / `make test-race` | Unit tests | Go |
| `make test-coverage` + `make coverage-check` | Unit tests with coverage; fails below 60% | Go |
| `make test-integration` | Unit + `integration`-tagged tests against the services in your `MONGORESCUE_TEST_*` env; missing services are skipped | Go, MongoDB Database Tools |
| `make test-integration-docker` | Same, against disposable MongoDB 7, MinIO and LocalStack containers | Docker, curl, Go, MongoDB Database Tools |
| `make docker-smoke` | Builds the image and checks health, auth, dashboard and bundled tools | Docker, curl |

The integration suite (`internal/integration`) drives the real `mongodump`/`mongorestore` binaries: backup and safe-clone restore, collection filters, gzip on and off, the full HTTP API in-process, and a storage conformance suite run against local disk and every configured S3 provider. Every test also asserts that the MongoDB password never appears in API responses, records or logs.

| Variable | Purpose |
|---|---|
| `MONGORESCUE_TEST_MONGO_URI` | MongoDB URI **with credentials**, e.g. `mongodb://root:pw@127.0.0.1:27017/?authSource=admin` |
| `MONGORESCUE_TEST_S3_<PROVIDER>_BUCKET` / `_ACCESS_KEY` / `_SECRET_KEY` | Enables a provider (`MINIO`, `LOCALSTACK`, `AWS`, `R2`, `B2`, `SPACES`, `WASABI`) |
| `MONGORESCUE_TEST_S3_<PROVIDER>_ENDPOINT` / `_REGION` / `_PATH_STYLE` | Endpoint, region (`auto` for R2) and path-style addressing (`true` for MinIO/LocalStack) |
| `MONGORESCUE_TEST_S3_<PROVIDER>_CREATE_BUCKET` | `true` creates the bucket if missing (emulators) |

