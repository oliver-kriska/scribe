# scribe — personal KB pipeline
#
# FTS5 is required because ccrider's sessions.db uses a `messages_fts` virtual
# table and triage runs scored BM25 queries against it. go-sqlite3 ships without
# FTS5 by default; the `sqlite_fts5` build tag enables it.

GO      ?= go
GOFLAGS ?=
TAGS    ?= sqlite_fts5
PKG     := ./cmd/scribe/
PREFIX  ?= $(HOME)/.local
BIN     := $(PREFIX)/bin/scribe
# If the user has GOBIN set (e.g. via mise), shadow that path too so `which
# scribe` returns the same binary cron uses. Without this, a mise-managed
# stale binary in $GOBIN will silently shadow the fresh build.
GOBIN_DIR := $(shell $(GO) env GOBIN)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install test tidy check fmt race lint vuln ci clean

# Default target builds — matches existing muscle memory. `make ci` runs the
# full pre-release gate (test+vet+race+lint+vuln).
build: ## Build the scribe binary (default)
	CGO_ENABLED=1 $(GO) build -tags "$(TAGS)" -ldflags "$(LDFLAGS)" $(GOFLAGS) -o $(BIN) $(PKG)

install: build ## Build and install to $PREFIX/bin (and $GOBIN if set)
	@if [ -n "$(GOBIN_DIR)" ] && [ "$(GOBIN_DIR)/scribe" != "$(BIN)" ]; then \
		echo "mirroring binary to $(GOBIN_DIR)/scribe (GOBIN is set — keeps \`which scribe\` in sync)"; \
		install -m 0755 "$(BIN)" "$(GOBIN_DIR)/scribe"; \
	fi

test: ## Run tests
	$(GO) test -tags "$(TAGS)" $(GOFLAGS) -count=1 ./...

race: ## Run tests with race detector
	$(GO) test -race -tags "$(TAGS)" $(GOFLAGS) -count=1 ./...

fmt: ## Format source (gofmt)
	gofmt -w $(shell git ls-files '*.go')

lint: ## Run golangci-lint (install: brew install golangci-lint)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed — brew install golangci-lint"; exit 1; }
	golangci-lint run --timeout 5m --build-tags "$(TAGS)"

vuln: ## Run govulncheck against current deps + stdlib
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest -tags "$(TAGS)" ./...

tidy: ## Tidy go.mod
	$(GO) mod tidy

check: test ## Run test + vet (quick dev gate)
	$(GO) vet -tags "$(TAGS)" ./...

ci: check race lint vuln ## Full pre-release gate — test+vet+race+lint+vuln

clean: ## Remove built binary
	rm -f $(BIN)
