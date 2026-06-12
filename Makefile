# scribe — personal KB pipeline
#
# FTS5 is required because ccrider's sessions.db uses a `messages_fts` virtual
# table and triage runs scored BM25 queries against it. go-sqlite3 ships without
# FTS5 by default; the `sqlite_fts5` build tag enables it.
#
# Build vs deploy are split (issue #18): `make build` compiles to ./bin/scribe
# (repo-local, gitignored) and never touches the live binary; `make install`
# deploys to $(PREFIX)/bin — the binary cron executes. On macOS, replacing the
# deployed binary invalidates the chat.db Full Disk Access grant; re-run
# `scribe fda` after `make install`.

GO      ?= go
GOFLAGS ?=
TAGS    ?= sqlite_fts5
PKG     := ./cmd/scribe/
BIN     := bin/scribe
PREFIX  ?= $(HOME)/.local
INSTALL_BIN := $(PREFIX)/bin/scribe
# If the user has GOBIN set (e.g. via mise), shadow that path too so `which
# scribe` returns the same binary cron uses. Without this, a mise-managed
# stale binary in $GOBIN will silently shadow the fresh deploy.
GOBIN_DIR := $(shell $(GO) env GOBIN)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install test tidy check fmt race lint vuln ci clean

# Default target builds — matches existing muscle memory, but the output now
# lands in ./bin/scribe instead of ~/.local/bin (deploy is `make install`).
# `make ci` runs the full pre-release gate (test+vet+race+lint+vuln).
build: ## Build the scribe binary to ./bin/scribe (default; does not deploy)
	@mkdir -p $(dir $(BIN))
	CGO_ENABLED=1 $(GO) build -tags "$(TAGS)" -ldflags "$(LDFLAGS)" $(GOFLAGS) -o $(BIN) $(PKG)

install: build ## Build, then deploy to $(PREFIX)/bin (and $GOBIN if set) — the binary cron runs
	install -d "$(PREFIX)/bin"
	install -m 0755 "$(BIN)" "$(INSTALL_BIN)"
	@if [ -n "$(GOBIN_DIR)" ] && [ "$(GOBIN_DIR)/scribe" != "$(INSTALL_BIN)" ]; then \
		echo "mirroring binary to $(GOBIN_DIR)/scribe (GOBIN is set — keeps \`which scribe\` in sync)"; \
		install -m 0755 "$(BIN)" "$(GOBIN_DIR)/scribe"; \
	fi
	@echo "deployed $(INSTALL_BIN) — on macOS, re-run \`scribe fda\` (replacing the binary drops the chat.db Full Disk Access grant)"

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

clean: ## Remove repo-local build output (never touches the deployed binary)
	rm -rf bin
