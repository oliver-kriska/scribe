package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// initAction is one row of the init plan: what `scribe init` will do
// (Title — printed in the plan list) and why/how (Explain — printed on
// [e]xplain). Borrowed from the nix-installer flow: the user consents
// to the whole printed plan once, instead of discovering writes as
// after-the-fact warnings.
type initAction struct {
	Title   string
	Explain string
}

// buildInitPlan derives the action list from the SAME flags and answers
// the execution phase uses, including the actions deliberately skipped
// (global-state writes without --bind) — disclosure of what won't
// happen is half the trust.
func (c *InitCmd) buildInitPlan(abs string, vars templateVars, ucKBDir string, allowUserWrites, throwaway bool) []initAction {
	var plan []initAction

	scaffold := initAction{
		Title: fmt.Sprintf("write KB scaffold at %s", abs),
		Explain: "Renders the embedded templates — scribe.yaml, CLAUDE.md, .gitignore, " +
			"wiki/_index.md, wiki/_hot.md, log.md — creates the content directory tree " +
			"(wiki/, projects/, raw/, scripts/, output/, …) and seeds empty state files " +
			"(scripts/projects.json, backlinks, session logs). Everything in this step " +
			"stays inside the one target directory.",
	}
	if c.Force {
		scaffold.Title += " (--force: overwrites existing files)"
	}
	plan = append(plan, scaffold)

	if !c.NoGit {
		plan = append(plan, initAction{
			Title: "git init -b main",
			Explain: "Creates a git repository in the KB so article history is versioned " +
				"and `scribe sync` can auto-commit what it writes. Skipped automatically " +
				"when .git already exists.",
		})
	}

	if c.Team {
		plan = append(plan, initAction{
			Title: "team mode: per-machine manifest + config trust",
			Explain: "scripts/projects.json is gitignored so each member keeps their own " +
				"discovery/approval state, and scribe.yaml gets `team: true`. On every " +
				"member's FIRST sync the sensitive config keys (source filters, ingestion " +
				"dirs, capture, ollama_url) are snapshotted to ~/.config/scribe/trust.json; " +
				"after that, pushed changes to them warn instead of applying until accepted " +
				"with `scribe config trust`. iMessage capture is hard-off from the shared " +
				"config — members opt in via their own gitignored scribe.local.yaml. Because " +
				"the snapshot trusts whatever the config says at first sync, members should " +
				"clone only from a remote they trust.",
		})
	}

	const ucPath = "~/.config/scribe/config.yaml"
	pointExplain := "kb_dir in the user config is the default target for every scribe " +
		"command run outside a KB directory. Pointing it here makes this KB the " +
		"primary one; other KBs stay intact and remain reachable via SCRIBE_KB=<path> " +
		"or by running scribe inside their directory."
	skipExplain := "Bootstrap never re-points global state away from an existing KB " +
		"without an explicit opt-in (--bind, or --yes/--force on a normal path) — " +
		"that would be destructive for multi-KB setups. The new KB still works via " +
		"SCRIBE_KB=<path> or by running scribe inside it."
	switch {
	case ucKBDir == abs:
		plan = append(plan, initAction{
			Title:   "refresh " + ucPath + " (already points here)",
			Explain: pointExplain,
		})
	case allowUserWrites && ucKBDir != "":
		plan = append(plan, initAction{
			Title:   fmt.Sprintf("point %s at this KB (currently %s)", ucPath, ucKBDir),
			Explain: pointExplain,
		})
	case allowUserWrites:
		plan = append(plan, initAction{
			Title:   "point " + ucPath + " at this KB",
			Explain: pointExplain,
		})
	case throwaway:
		plan = append(plan, initAction{
			Title: "skip " + ucPath + " (temp path — pass --bind to make a temp KB primary)",
			Explain: "The target looks like a throwaway/temp location, so global scribe " +
				"state is left alone even under --yes/--force. " + skipExplain,
		})
	default:
		plan = append(plan, initAction{
			Title:   "skip " + ucPath + " (pass --bind to make this the default KB)",
			Explain: skipExplain,
		})
	}

	blockExplain := func(path, agent string) string {
		return "The block between the scribe markers in " + path + " tells every " + agent +
			" session to query this KB before decisions and to write drop files for " +
			"reusable knowledge. Content outside the markers is never touched."
	}
	switch {
	case c.NoClaudeMD:
		plan = append(plan, initAction{
			Title:   "skip ~/.claude/CLAUDE.md block (--no-claude-md)",
			Explain: blockExplain("~/.claude/CLAUDE.md", "Claude Code"),
		})
	case allowUserWrites:
		plan = append(plan, initAction{
			Title:   "install/refresh scribe block in ~/.claude/CLAUDE.md",
			Explain: blockExplain("~/.claude/CLAUDE.md", "Claude Code"),
		})
	default:
		plan = append(plan, initAction{
			Title:   "skip ~/.claude/CLAUDE.md block (pass --bind to install)",
			Explain: blockExplain("~/.claude/CLAUDE.md", "Claude Code") + " " + skipExplain,
		})
	}
	switch {
	case c.NoCodexMD:
		plan = append(plan, initAction{
			Title:   "skip ~/.codex/AGENTS.md block (--no-codex-md)",
			Explain: blockExplain("~/.codex/AGENTS.md", "Codex CLI"),
		})
	case allowUserWrites:
		plan = append(plan, initAction{
			Title:   "install/refresh scribe block in ~/.codex/AGENTS.md",
			Explain: blockExplain("~/.codex/AGENTS.md", "Codex CLI"),
		})
	default:
		plan = append(plan, initAction{
			Title:   "skip ~/.codex/AGENTS.md block (pass --bind to install)",
			Explain: blockExplain("~/.codex/AGENTS.md", "Codex CLI") + " " + skipExplain,
		})
	}

	if strings.EqualFold(vars.LLMProvider, "ollama") {
		plan = append(plan, initAction{
			Title: fmt.Sprintf("probe Ollama and pre-pull %s", vars.LLMModel),
			Explain: "Checks that an Ollama server is reachable and pulls the recommended " +
				"model now, so the first sync run doesn't stall on /api/pull. Non-fatal — " +
				"if Ollama isn't running yet, init prints a hint instead.",
		})
	}

	if runtime.GOOS == "darwin" && vars.SelfChatHandle != "" {
		plan = append(plan, initAction{
			Title: "offer Full Disk Access setup for iMessage capture",
			Explain: "scribe capture reads ~/Library/Messages/chat.db, which macOS gates " +
				"behind Full Disk Access. The helper walks through granting it; decline " +
				"anytime and re-run later with `scribe fda`.",
		})
	}

	plan = append(plan, initAction{
		Title: "run dependency self-test",
		Explain: "Probes PATH for the binaries scribe shells out to (claude, ccrider, qmd, " +
			"sqlite3, git + optional extras) — the same check `scribe doctor` runs — so a " +
			"missing dependency surfaces now, not on the first cron-driven sync.",
	})

	return plan
}

// printInitPlan renders the numbered plan. Always printed — --yes and
// --check skip the consent prompt, never the disclosure.
func printInitPlan(abs string, plan []initAction) {
	fmt.Printf("\nInit plan — %s\n", abs)
	for i, a := range plan {
		fmt.Printf("  %2d. %s\n", i+1, a.Title)
	}
}

// promptInitProceed asks for consent to the whole plan. [e]xplain
// prints a paragraph per action and re-prompts; EOF (non-interactive
// stdin without --yes) defaults to yes, matching promptYesNo.
func promptInitProceed(plan []initAction) bool {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\nProceed? ([Y]es/[n]o/[e]xplain): ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return true
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return true
		case "n", "no":
			return false
		case "e", "explain":
			fmt.Println()
			for i, a := range plan {
				fmt.Printf("%2d. %s\n%s\n\n", i+1, a.Title, indent(a.Explain, "    "))
			}
		}
	}
}

// printDepsCheck probes PATH for every scribe dependency and prints one
// line each; returns how many REQUIRED ones are missing. Shared by
// bootstrap's self-test phase and `scribe init` status mode.
func printDepsCheck() int {
	missingRequired := 0
	for _, d := range scribeDeps {
		path, err := exec.LookPath(d.Binary)
		status := path
		if err != nil {
			status = "MISSING"
			if d.Required {
				missingRequired++
			} else {
				status = "missing (optional)"
			}
		}
		fmt.Printf("  %-13s %s\n", d.Name, status)
		if d.Note != "" && (err != nil || d.Required) {
			fmt.Printf("               %s\n", d.Note)
		}
		if err != nil && d.Fix != "" {
			fmt.Printf("               fix: %s\n", d.Fix)
		}
	}
	return missingRequired
}
