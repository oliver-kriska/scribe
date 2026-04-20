set dotenv-load
set shell := ["bash", "-euo", "pipefail", "-c"]

default:
  @just --list

# === Orchestration (all via scribe Go binary) ===
sync *args:
  scribe sync {{args}}

deep project *args:
  scribe deep {{project}} {{args}}

dream *args:
  scribe dream {{args}}

sessions *args:
  scribe sync --sessions {{args}}

commit *args:
  scribe commit {{args}}

# === Data processing ===
lint *args:
  scribe lint {{args}}

triage *args:
  scribe triage {{args}}

validate *args:
  scribe lint --changed {{args}}

scan project:
  scribe scan {{project}}

backlinks:
  scribe backlinks

index:
  scribe index

orphans *args:
  scribe orphans {{args}}

capture *args:
  scribe capture {{args}}

# === Dev ===
# No active *.sh scripts to check; legacy helpers live under scripts/legacy/.
# Run `shellcheck scripts/legacy/*.sh` manually if touching that directory.

test-legacy:
  bats scripts/legacy/tests/

build:
  CGO_ENABLED=1 go build -tags "sqlite_fts5" -ldflags "-X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o ~/.local/bin/scribe ./cmd/scribe

# === Compound ===
full-cycle: sync dream lint
