# Set up scribe — human and coding-agent runbook

This is the zero-to-running setup guide for scribe. It is deliberately
procedural: choose one recipe, run the commands in order, and finish with the
verification checklist. It covers personal Anthropic, local Ollama, hosted
providers, team owners and members, multiple KBs, and Linux scheduling.

Public URL: <https://getscribe.dev/setup.md>

## Give this guide to Claude Code, Codex, or another coding agent

Paste this prompt:

> Read https://getscribe.dev/setup.md and set up scribe on this machine.
> Before changing anything, ask me for the setup profile, KB path, and LLM provider.
> For a team setup, also ask whether I am creating or joining the KB and ask for the private git remote.
> Inspect existing state before changing it. Execute the applicable steps; do not only explain them.
> Never use --force. Never print or commit secrets.
> Ask before the first real scribe sync because it may call an LLM and incur cost.
> Finish with the runbook's verification checklist and report what passed and what still needs human action.

The agent should stop and ask the human for actions it cannot safely perform,
such as signing into Claude, creating a private git repository, granting macOS
Full Disk Access, adding a hosted-provider API key, or installing Linux crontab
entries.

## Choose one profile

| Goal | Recipe |
|---|---|
| Your first personal KB, Anthropic-backed | [Personal KB](#personal-kb) with `--provider anthropic` |
| Your first personal KB, no API spend | [Personal KB](#personal-kb) with `--provider ollama` |
| Use Together, Groq, Fireworks, Hugging Face, or another `/v1` endpoint | [Hosted provider](#hosted-openai-compatible-provider) |
| Create and publish a KB for a team | [Team owner](#team-owner-create-and-publish) |
| Join an existing team KB | [Team member](#team-member-clone-and-onboard) |
| Add a team KB while keeping a personal KB as default | [A second KB on one machine](#a-second-kb-on-one-machine) |
| Linux scheduling | Use the same recipe, then follow [Linux cron](#linux-cron) |

## Before changing anything

An agent should inspect, not guess:

```sh
uname -s
command -v scribe || true
command -v git || true
command -v sqlite3 || true
command -v ccrider || true
command -v qmd || true
command -v claude || true
command -v ollama || true
scribe --version 2>/dev/null || true
```

If the intended KB directory already contains `scribe.yaml`, treat it as an
existing KB. Do not run a fresh `scribe init --path …` over it and do not use
`--force`. For an existing checkout, `cd` into it and run `scribe init --check`
or the team-member recipe below.

Do not paste API keys into a prompt, command history, committed `scribe.yaml`,
or a team repository. Hosted-provider keys belong in the per-machine
`~/.config/scribe/config.yaml` (mode `0600`) or an environment variable.

## Install the CLI and required tools

### Homebrew (macOS or Linuxbrew)

```sh
brew tap oliver-kriska/scribe
brew install oliver-kriska/scribe/scribe
```

The formula installs `git`, `sqlite`, and `ccrider`. Install the remaining
required tools:

```sh
curl -fsSL https://claude.ai/install.sh | bash
npm install -g @tobilu/qmd
```

Use `claude` once and complete its normal sign-in when the Anthropic provider
will be used. The Ollama profile does not spend Anthropic tokens, but the
current dependency check still expects the Claude CLI to be present.

For fully local inference, also install and start Ollama:

```sh
brew install ollama
ollama serve
```

On macOS the Ollama app/service may already be running; do not start a second
server if `ollama list` works. When it is not running, start `ollama serve` in a
dedicated terminal or OS service; a setup agent should not leave an
uncontrolled foreground server attached to its command session.

### Shell installer

```sh
curl -fsSL https://raw.githubusercontent.com/oliver-kriska/scribe/main/install.sh | bash
```

The binary lands in `$HOME/.local/bin` by default. Ensure that directory is on
`PATH`, then install `git`, `sqlite3`, `ccrider`, `qmd`, and the selected LLM
provider using their upstream instructions. Check the result:

```sh
scribe --version
```

## Personal KB

Set the target once so later commands are copy-safe:

```sh
KB="$HOME/my-kb"
```

### Anthropic

```sh
scribe init --path "$KB" --bind --provider anthropic
```

### Local Ollama

```sh
scribe init --path "$KB" --bind --provider ollama
```

`--bind` matters: it makes this KB the machine default and installs the scribe
handshake into `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`, and
`~/.config/amp/AGENTS.md`. Without it,
init may deliberately leave machine-global state untouched, especially when
another KB is already configured.

Continue with either provider:

```sh
cd "$KB"
scribe skill install
scribe sync --discover
scribe projects review
scribe sync --dry-run --estimate
scribe cron install
scribe doctor
```

What these steps do:

1. `scribe skill install` adds the operational agent skills inside the KB. It
   does not replace install/init; it is useful after the binary and KB exist.
2. `sync --discover` records candidate repositories without extracting them.
3. `projects review` is the consent gate. Approve only repositories this KB is
   allowed to read.
4. `sync --dry-run --estimate` previews pending work without writes or LLM
   calls. A real `scribe sync` can spend tokens; run it only after reviewing the
   estimate.
5. `cron install` installs macOS LaunchAgents or prints Linux crontab entries.
6. `doctor` checks dependencies, configuration, scheduling, qmd, and local
   Ollama health when selected. A new empty KB can have freshness warnings;
   hard failures must be fixed.

Optional: add a private git remote so the markdown KB is backed up:

```sh
git -C "$KB" remote add origin git@github.com:you/my-kb.git
git -C "$KB" push -u origin main
```

### Non-interactive personal init

Supply every value that should not use a default:

```sh
scribe init \
  --path "$KB" \
  --bind \
  --provider anthropic \
  --owner-name "Alice" \
  --owner-context "Platform engineer working on the API and infrastructure." \
  --domains platform,infra \
  --yes
```

Use `--provider ollama` for local mode. Omit `--handle` to leave optional
iMessage capture disabled.

## Hosted OpenAI-compatible provider

The init flag accepts `anthropic` or `ollama`; hosted routing is configured
after scaffolding. A personal user starts with the Anthropic personal recipe
above. A team owner starts with the team-owner recipe below. In either case,
replace the generated top-level `llm:` block with the chosen provider's real
model and endpoint policy before publishing the configuration. Example:

```yaml
llm:
  provider: together
  model: MiniMaxAI/MiniMax-M3
  api_key_env: TOGETHER_API_KEY
  pricing:
    "together/MiniMaxAI/MiniMax-M3":
      input: 0.30
      output: 1.20
```

Named providers (`together`, `groq`, `fireworks`, `huggingface`) have built-in
base URLs. Use `provider: openai-compat` plus `base_url` for another compatible
`/v1/chat/completions` endpoint. Confirm the exact serverless model ID and
current pricing with the provider; catalog names and rates change.

The key must stay outside the KB. Have the human place it in an environment
variable or add `llm_api_key` / provider-specific `llm_api_keys` to
`~/.config/scribe/config.yaml`, then restrict the file:

```sh
chmod 600 ~/.config/scribe/config.yaml
scribe doctor
scribe sync --dry-run --estimate
```

`doctor` validates the parsed routing and budget configuration but does not
send a hosted-provider probe. The endpoint, key, and model are fully validated
only by an approved real LLM call, which can incur cost. The final sync in this
guide is that end-to-end check.

For a team KB, the routing block is shared and trust-locked, but every member
supplies their own key locally. Never commit a key to `scribe.yaml` or ask a
member to paste one into an agent prompt.

## Team owner: create and publish

One person creates the KB. Everyone else clones it; teammates must **not** run
`scribe init --team --path …` independently.

Before init, choose a checkout convention shared by the team. The example uses
`~/work`; the quotes are important because they preserve `~` in committed
`scribe.yaml`, allowing scribe to expand it separately on every machine.

```sh
KB="$HOME/team-kb"

scribe init \
  --path "$KB" \
  --bind \
  --team \
  --kb-name acme-kb \
  --allow '~/work' \
  --provider anthropic

cd "$KB"
scribe skill install
```

Use `--provider ollama` only if every participating machine will run the chosen
Ollama model. If teammates keep repositories in different directory layouts,
prefer a git-identity allowlist. Before pushing, edit the generated
`scribe.yaml`:

```yaml
sources:
  include:
    - ~/work
  allowed_remotes:
    - github.com/acme
```

`allowed_remotes` rejects repositories without a matching `origin`, regardless
of checkout location. Keep personal/client repositories out with shared
`sources.exclude` rules or stricter per-person rules in the gitignored
`scribe.local.yaml`.

Init creates the scaffold commit before skills or later config edits, so commit
those additions and publish the private repository:

```sh
git add scribe.yaml .claude/skills .agents/skills
git commit -m "chore: configure team KB"
git remote add origin git@github.com:acme/team-kb.git
git push -u origin main
```

Then onboard the owner's machine like any other member:

```sh
scribe config trust
scribe sync --discover
scribe projects review
scribe sync --dry-run --estimate
scribe cron install
scribe doctor
```

Do not place a personal iMessage handle, API key, personal source path, or NDA
stop word in committed `scribe.yaml`. Put per-person values in
`scribe.local.yaml` or `~/.config/scribe/config.yaml`.

## Team member: clone and onboard

Every member installs the CLI and dependencies first. Use the same scribe
release and LLM provider policy as the rest of the team, then:

If this machine already has a personal KB and it should remain the default
agent handshake, follow [A second KB on one machine](#a-second-kb-on-one-machine)
instead of running `--bind`. If the team KB should become the default, the bind
below changes the global handshake, but the previous personal KB is preserved as
an explicit registry entry so it stays in the machine-level schedule.

```sh
KB="$HOME/team-kb"

git clone git@github.com:acme/team-kb.git "$KB"
cd "$KB"

git config user.name "Alice Example"
git config user.email "alice@example.com"

scribe init --bind --yes
scribe config diff
scribe config trust
scribe skill install --check
scribe sync --discover
scribe projects review
scribe sync --dry-run --estimate
scribe cron install
scribe doctor
```

The member command is `scribe init --bind --yes` **inside the clone**. Do not
pass `--path`, `--team`, `--kb-name`, `--allow`, or `--provider`: those are
team-owner choices already committed in `scribe.yaml`. This init invocation
checks the checkout and installs that member's machine-local default and agent
handshakes without recreating the KB.

`scribe config diff` shows the trust boundary; `scribe config trust` explicitly
accepts the cloned sensitive settings on that machine. `skill install --check`
ensures the committed skills match the member's binary. If it reports drift,
coordinate one skill update through the shared repository instead of having
every member create competing updates.

Each member has their own gitignored `scripts/projects.json`, so each must run
discovery and approve repositories. Those approvals and absolute paths are not
shared. The shared commands/flags live in committed `scribe.yaml`; the local
manifest, credentials, subscriptions, capture handles, and additional source
filters remain per-machine.

## A second KB on one machine

The scheduler supports multiple registered KBs, but the global Claude/Codex/Amp
handshake and `kb_dir` default point to one KB at a time.

To keep an existing personal KB as the default while adding a team KB:

```sh
KB="$HOME/team-kb"
git clone git@github.com:acme/team-kb.git "$KB"
cd "$KB"

scribe init --check
scribe config trust
scribe kb add "$KB"
scribe cron install
scribe doctor
```

Do not use `--bind` in this profile: it would repoint the one global handshake.
Target the team KB by running inside it or explicitly:

```sh
scribe -C "$KB" status
scribe -C "$KB" sync --dry-run --estimate
```

To intentionally make the team KB the new default, bind the team checkout.
Rebinding preserves the previous default as an explicit registry entry, so the
personal KB keeps its place in the machine-level schedule; `scribe kb list`
confirms it:

```sh
cd "$KB"
scribe init --bind --yes
scribe kb list
```

One KB-agnostic LaunchAgent set runs `scribe each` across the registry; a
second KB does not require a second set of plists. Configure per-KB pacing with
`each.cadence` in that KB's `scribe.yaml` if needed.

## Linux cron

On Linux, `scribe cron install` prints crontab lines instead of installing
them. Review the output, paste the block into `crontab -e`, then verify:

```sh
crontab -l
scribe cron status
scribe kb list
```

`scribe cron status` reports whether the scribe block is actually present in the
user crontab. Do not claim scheduling is installed merely because scribe printed
the block; the user must actually save it.

## Optional macOS iMessage capture

Personal KB: add the self-chat handle during interactive init or later in
`scribe.yaml`. Team KB: place it only in the gitignored `scribe.local.yaml`.
Then grant Full Disk Access to the exact scribe binary:

```sh
scribe fda
scribe fda --verify
```

Official macOS release binaries are Developer ID signed and notarized, so a
signed replacement at the same registered path keeps the grant. Homebrew moves
the raw executable to a new versioned Cellar path, which TCC records separately,
and an unsigned local `make install` changes the code identity. Re-run
`scribe fda` after `brew upgrade` or an unsigned rebuild to verify and re-grant.

## Verification checklist

Use `-C` so every check names the intended KB explicitly:

```sh
scribe --version
scribe -C "$KB" init --check
scribe -C "$KB" doctor
scribe kb list
scribe -C "$KB" projects list
scribe -C "$KB" sync --dry-run --estimate
scribe -C "$KB" skill install --check
git -C "$KB" status --short
```

`init --check` is read-only and non-interactive on its own — don't reach for
`--yes` here. Without `--check`, `--yes` repoints the KB binding in your global
agent files, so pairing the two teaches a habit that is one dropped flag away
from rewriting state you didn't mean to touch. Verify scheduling — `cron status`
covers both
platforms (LaunchAgents on macOS, the crontab block on Linux):

```sh
scribe cron status

# Linux, to see the block itself
crontab -l
```

For a team KB, also run:

```sh
scribe -C "$KB" config diff
git -C "$KB" remote -v
```

Setup is complete when:

- `scribe` resolves to the expected binary and reports a version.
- `init --check` finds the intended KB without prompts or writes.
- `doctor` has no unexplained hard failures; qmd is healthy, and Ollama is
  reachable with the chosen local model when that provider is selected.
- The KB appears in `scribe kb list`, and cron is installed (or its Linux block
  has actually been added).
- Only intended repositories are approved.
- The dry-run estimate is plausible before any paid sync.
- Agent skills match, and team config has no unreviewed drift.
- The KB git remote is private and the worktree contains no accidental secret
  or machine-local file.

For a hosted provider, the checklist proves local configuration but deliberately
does not spend tokens on a probe. A successful first real sync below is the
endpoint/key/model verification.

Only then, and with the user's approval when inference may cost money, run the
first real pipeline pass:

```sh
scribe -C "$KB" sync
```

## What `init`, the handshake, and skills each do

- `scribe init --bind` creates or checks the KB **and** wires the one
  machine-global Claude Code/Codex/Amp handshake. That handshake makes agents in
  other project directories query the KB and write reusable drop files.
- `scribe skill install` writes `scribe-kb`, `scribe-kb-tidy`, and `scribe-cli`
  into the KB's agent-skill directories. Those skills help an agent operate and
  maintain an existing KB when a session is opened where the skills are
  discoverable.
- Therefore the skills command is useful, but it is not a bootstrap mechanism:
  it requires an installed binary and a KB, does not install dependencies, does
  not choose personal versus team mode, and does not replace `init --bind`,
  source approval, cron setup, or `doctor` verification.
