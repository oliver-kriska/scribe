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

# Both build/install delegate to the Makefile so the build flags can't drift.

# compile to ./bin/scribe (repo-local) — does NOT deploy
build:
  make build

# build + deploy ./bin/scribe to ~/.local/bin (the binary cron runs)
install:
  make install

# === Compound ===
full-cycle: sync dream lint
