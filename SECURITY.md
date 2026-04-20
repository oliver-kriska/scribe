# Security policy

Thanks for taking the time to report a security issue responsibly.

## How to report

Email **security@ideax.sk** with:

- A short description of the issue.
- Steps to reproduce, or a minimal proof-of-concept.
- The version / commit SHA you tested against.

Please do **not** open a public GitHub issue for anything that could
expose users to harm (arbitrary code execution, privilege escalation,
data exfiltration, credential leakage, etc.) — send the report by email
first so a fix can ship before details are public.

## What to expect

- Acknowledgement within **3 business days**.
- A fix or mitigation plan within **14 days** for confirmed issues, or
  an honest "we don't consider this a vulnerability, here's why" reply.
- Coordinated disclosure once a fix is released. You'll be credited in
  the release notes unless you ask not to be.

## Out of scope

- Bugs in dependencies — report those upstream. If the issue is in how
  scribe uses a dependency, that's in scope; if it's in the dependency
  itself, it's not.
- Social-engineering issues that rely on a compromised developer machine
  (scribe runs LLM prompts written by the user; if you can plant a
  malicious prompt on a developer's disk, the attacker already owns the
  machine).
- Performance regressions, unless they are exploitable as a DoS
  against a shared resource.

## Scope

scribe is a local-first single-user CLI. The surfaces worth scrutinising:

- How `scribe capture` reads from `~/Library/Messages/chat.db` (macOS
  Full Disk Access required).
- How `scribe ingest`/`scribe absorb` treats untrusted URL/file input
  before passing it to an LLM prompt.
- Any subprocess invocation (`claude -p`, `qmd`, `git`, `trafilatura`,
  `launchctl`) — argument injection, command injection, lock-file races.
- The embedded prompt templates — prompt-injection vectors that could
  cause scribe to write outside its KB root.
