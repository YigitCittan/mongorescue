# ==============================================================================
# MongoRescue Makefile
# ==============================================================================

BINARY_NAME=mongorescue
CMD_PATH=./cmd/mongorescue
DIST_DIR=dist
VERSION?=$(shell git describe --tags --always 2>/dev/null || echo "1.0.0")
COMMIT?=$(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
DATE?=$(shell date -u +%Y-%m-%d)

LDFLAGS=-s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.Date=$(DATE)

# Desktop app (Wails v2, cmd/mongorescue-desktop). The frontend is web/static, served by
# the Go handler, so the Wails npm steps and JS bindings are skipped. On Linux distros
# that only ship WebKitGTK 4.1 (Ubuntu 24.04+), set DESKTOP_TAGS=desktop,webkit2_41.
WAILS?=wails
DESKTOP_DIR=cmd/mongorescue-desktop
DESKTOP_TAGS?=desktop
DESKTOP_FLAGS=-clean -trimpath -s -skipbindings -m -nosyncgomod -tags "$(DESKTOP_TAGS)" -ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT)"

# Minimum total statement coverage (percent) enforced by coverage-check.
COVERAGE_MIN?=60
COVERAGE_FILE?=coverage.out
# Extra flags for integration runs (e.g. INTEGRATION_FLAGS="-coverprofile=integration-coverage.out").
INTEGRATION_FLAGS?=

# Fuzzing: time per fuzz target, and the <package>:<FuzzFunc> targets make fuzz runs
# (override FUZZ_TARGETS to run a subset).
FUZZTIME?=30s
FUZZ_TARGETS?= \
	./internal/config:FuzzLoadLegacy \
	./internal/desktop:FuzzArchiveTarget \
	./internal/desktop:FuzzExtractArchive \
	./internal/desktop:FuzzExtractArchiveNames \
	./internal/encryption:FuzzDecrypt \
	./internal/encryption:FuzzRoundTrip \
	./internal/models:FuzzRescueDatabaseName \
	./internal/models:FuzzValidateNamespace \
	./internal/mongotools:FuzzReadPrelude \
	./internal/mongotools:FuzzWithConnectionDefaults \
	./internal/mongotools:FuzzWriteURIConfig \
	./internal/mongouri:FuzzValidate \
	./internal/mongouri:FuzzValidateStored \
	./internal/notify:FuzzDestinationPolicy \
	./internal/notify:FuzzValidateEmail \
	./internal/notify:FuzzValidateWebhook \
	./internal/notify:FuzzWebhookHostForms \
	./internal/redact:FuzzTextPassword \
	./internal/redact:FuzzURI \
	./internal/redact:FuzzURIPassword \
	./internal/restore:FuzzPrepareNamespaces \
	./internal/scheduler:FuzzValidateCron \
	./internal/secretbox:FuzzOpen \
	./internal/secretbox:FuzzSealOpen \
	./internal/storage:FuzzLocalKey \
	./internal/update:FuzzCompare \
	./internal/update:FuzzFindChecksum \
	./internal/update:FuzzParseVersion

.PHONY: all build clean test test-race test-coverage coverage-check test-integration test-integration-docker fuzz cross-compile docker-build docker-smoke run

all: test-race build

## build: Compiles the native binary
build:
	@echo "==> Building $(BINARY_NAME)..."
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(BINARY_NAME) $(CMD_PATH)
	@echo "==> Built bin/$(BINARY_NAME)"

## test: Runs standard test suite
test:
	@echo "==> Running tests..."
	go test -v ./...

## test-race: Runs tests with race condition detector
test-race:
	@echo "==> Running race detection tests..."
	go test -v -race ./...

## test-coverage: Runs unit tests with race detector and writes $(COVERAGE_FILE)
test-coverage:
	@echo "==> Running unit tests with coverage..."
	go test -race -coverprofile=$(COVERAGE_FILE) -covermode=atomic ./...

## coverage-check: Fails if total coverage in $(COVERAGE_FILE) is below $(COVERAGE_MIN)%
coverage-check:
	@total=$$(go tool cover -func=$(COVERAGE_FILE) | awk '/^total:/ {sub("%", "", $$3); print $$3}'); \
	echo "==> Total coverage: $${total}% (minimum $(COVERAGE_MIN)%)"; \
	awk -v t="$$total" -v min="$(COVERAGE_MIN)" 'BEGIN { exit (t + 0 < min + 0) ? 1 : 0 }' || \
		{ echo "coverage $${total}% is below $(COVERAGE_MIN)%"; exit 1; }

## test-integration: Runs unit + integration tests (needs MONGORESCUE_TEST_* env, see CONTRIBUTING.md)
test-integration:
	@if [ -z "$$MONGORESCUE_TEST_MONGO_URI" ]; then \
		echo "==> hint: MONGORESCUE_TEST_MONGO_URI is not set; MongoDB integration tests will be skipped."; \
		echo "          Use 'make test-integration-docker' to run against disposable containers."; \
	fi
	@echo "==> Running integration tests..."
	go test -race -tags=integration -count=1 $(INTEGRATION_FLAGS) ./...

## test-integration-docker: Runs integration tests against disposable MongoDB, MinIO and LocalStack containers
test-integration-docker:
	./scripts/test-integration-docker.sh

## fuzz: Runs each fuzz target in FUZZ_TARGETS for FUZZTIME (default 30s), one after another
fuzz:
	@set -e; for target in $(FUZZ_TARGETS); do \
		pkg=$${target%%:*}; name=$${target##*:}; \
		echo "==> Fuzzing $$name in $$pkg for $(FUZZTIME)..."; \
		go test -run='^$$' -fuzz="^$$name\$$" -fuzztime=$(FUZZTIME) $$pkg; \
	done

## clean: Removes build artifacts
clean:
	@echo "==> Cleaning artifacts..."
	rm -rf bin $(DIST_DIR) coverage.out integration-coverage.out

## cross-compile: Cross-compiles for Linux, macOS, and Windows
cross-compile:
	@echo "==> Cross-compiling for Linux, macOS, and Windows..."
	@mkdir -p $(DIST_DIR)
	# Linux AMD64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)_linux_amd64 $(CMD_PATH)
	# Linux ARM64
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)_linux_arm64 $(CMD_PATH)
	# Darwin ARM64 (Apple Silicon)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)_darwin_arm64 $(CMD_PATH)
	# Darwin AMD64 (Intel Mac)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)_darwin_amd64 $(CMD_PATH)
	# Windows AMD64 (.exe)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)_windows_amd64.exe $(CMD_PATH)
	@echo "==> Cross-compilation complete in $(DIST_DIR)/"

## docker-build: Builds lightweight Docker container
docker-build:
	@echo "==> Building Docker image..."
	docker build -t mongorescue:$(VERSION) -t mongorescue:latest .

## docker-smoke: Builds the image and smoke-tests it (health, auth, dashboard, bundled tools)
docker-smoke:
	docker build -t mongorescue:smoke .
	./scripts/docker-smoke.sh mongorescue:smoke

## run: Runs local development server
run: build
	./bin/$(BINARY_NAME)

## desktop: Builds the desktop app for the host OS with the Wails CLI (CGO and the
## platform webview are required: Xcode CLT on macOS, WebView2 on Windows,
## libgtk-3-dev + libwebkit2gtk-4.0-dev (or 4.1, see DESKTOP_TAGS) on Linux).
## Output: $(DESKTOP_DIR)/build/bin/
desktop:
	@echo "==> Building the desktop app..."
	cd $(DESKTOP_DIR) && CGO_ENABLED=1 $(WAILS) build $(DESKTOP_FLAGS)

## desktop-windows: Builds the Windows desktop app and its NSIS installer (run on Windows, needs makensis)
desktop-windows:
	@echo "==> Building the Windows desktop app and installer..."
	cd $(DESKTOP_DIR) && CGO_ENABLED=1 $(WAILS) build $(DESKTOP_FLAGS) -platform windows/amd64 -nsis

.PHONY: desktop desktop-windows vulncheck lint-ci release-snapshot

## vulncheck: Scans reachable code for known vulnerabilities (govulncheck)
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## lint-ci: Lints GitHub workflows (actionlint) and shell scripts (shellcheck)
lint-ci:
	go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
	shellcheck scripts/*.sh

## release-snapshot: Builds a local GoReleaser snapshot in $(DIST_DIR)/ (needs syft for SBOMs)
release-snapshot:
	go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish,docker,sign
