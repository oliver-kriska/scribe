# scribe — personal KB pipeline
#
# FTS5 is required because ccrider's sessions.db uses a `messages_fts` virtual
# table and triage runs scored BM25 queries against it. go-sqlite3 ships without
# FTS5 by default; the `sqlite_fts5` build tag enables it.
#
# Build vs deploy are split (issue #18): `make build` compiles to ./bin/scribe
# (repo-local, gitignored) and never touches the live binary; `make install`
# deploys to $(PREFIX)/bin — the binary cron executes. On macOS, install signs
# with the first available Developer ID Application identity. The stable
# signing identity lets the chat.db Full Disk Access grant survive rebuilds.

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
UNAME_S := $(shell uname -s)
# Override when multiple Developer ID identities are installed. Use the
# certificate SHA-1 rather than its display name so spaces and punctuation
# cannot confuse codesign. GoReleaser's cross-platform signer uses the binary
# basename as its identifier, so local installs must use "scribe" too: matching
# Team ID + identifier is what keeps the TCC designated requirement stable.
CODESIGN_IDENTITY ?= $(shell if [ "$(UNAME_S)" = "Darwin" ]; then security find-identity -v -p codesigning 2>/dev/null | awk '/"Developer ID Application:/{print $$2; exit}'; fi)
CODESIGN_IDENTIFIER ?= scribe

.PHONY: build sign install test tidy check fmt race lint vuln ci clean

# Default target builds — matches existing muscle memory, but the output now
# lands in ./bin/scribe instead of ~/.local/bin (deploy is `make install`).
# `make ci` runs the full pre-release gate (test+vet+race+lint+vuln).
build: ## Build the scribe binary to ./bin/scribe (default; does not deploy)
	@mkdir -p $(dir $(BIN))
	CGO_ENABLED=1 $(GO) build -tags "$(TAGS)" -ldflags "$(LDFLAGS)" $(GOFLAGS) -o $(BIN) $(PKG)

sign: build ## On macOS, Developer-ID-sign the binary when an identity is available
	@set -e; if [ "$(UNAME_S)" = "Darwin" ] && [ -n "$(CODESIGN_IDENTITY)" ]; then \
		codesign --force --timestamp --options runtime \
			--identifier "$(CODESIGN_IDENTIFIER)" \
			--sign "$(CODESIGN_IDENTITY)" "$(BIN)"; \
		codesign --verify --strict --verbose=2 "$(BIN)"; \
		echo "signed $(BIN) with Developer ID identity $(CODESIGN_IDENTITY)"; \
	elif [ "$(UNAME_S)" = "Darwin" ]; then \
		echo "no Developer ID Application identity found — leaving $(BIN) unsigned"; \
	fi

install: sign ## Build, sign on macOS when possible, then deploy — the binary cron runs
	install -d "$(PREFIX)/bin"
	install -m 0755 "$(BIN)" "$(INSTALL_BIN)"
	@if [ -n "$(GOBIN_DIR)" ] && [ "$(GOBIN_DIR)/scribe" != "$(INSTALL_BIN)" ]; then \
		echo "mirroring binary to $(GOBIN_DIR)/scribe (GOBIN is set — keeps \`which scribe\` in sync)"; \
		install -m 0755 "$(BIN)" "$(GOBIN_DIR)/scribe"; \
	fi
	@if [ "$(UNAME_S)" = "Darwin" ] && [ -z "$(CODESIGN_IDENTITY)" ]; then \
		echo "deployed $(INSTALL_BIN) unsigned — re-run \`scribe fda\` after replacing it"; \
	else \
		echo "deployed $(INSTALL_BIN)"; \
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

clean: ## Remove repo-local build output (never touches the deployed binary)
	rm -rf bin
