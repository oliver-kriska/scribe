package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// `scribe config update` — discoverability for existing KBs. scribe.yaml
// is scaffolded once at init; options added to the template afterwards
// (secret_scan, subscriptions, owners, ...) never reach a KB that
// already exists. No migration is ever required — every new key
// defaults safely when absent — so this command's only job is to APPEND
// the missing template blocks as fully commented documentation. It is
// append-only and idempotent: existing content is never reordered or
// rewritten, active template lines are commented out before appending
// (defaults stay in force), and a key already mentioned in the user
// file — active or commented — is skipped.

type ConfigUpdateCmd struct {
	Check bool `help:"Only report which blocks would be appended; write nothing."`
}

func (c *ConfigUpdateCmd) ReadOnly() bool { return c.Check }

const configUpdateHeader = "# --- appended by `scribe config update`: options added to scribe since this\n" +
	"# --- file was scaffolded. Fully commented — defaults unchanged until you opt in.\n"

func (c *ConfigUpdateCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path := filepath.Join(root, "scribe.yaml")
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	missing := missingTemplateBlocks(string(current))
	if len(missing) == 0 {
		fmt.Println("scribe.yaml already documents every current option — nothing to append")
		return nil
	}

	keys := make([]string, 0, len(missing))
	for _, seg := range missing {
		keys = append(keys, seg.key)
	}
	if c.Check {
		fmt.Printf("would append %d commented block(s): %s\n", len(missing), strings.Join(keys, ", "))
		fmt.Println("run `scribe config update` to write them")
		return nil
	}

	updated := appendTemplateBlocks(string(current), missing)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("appended %d commented block(s) to scribe.yaml: %s\n", len(missing), strings.Join(keys, ", "))
	fmt.Println("each is documentation only — uncomment to opt in")
	return nil
}

// templateSegment is one top-level key of the embedded scribe.yaml
// template together with the doc comments above it.
type templateSegment struct {
	key   string
	lines []string
}

var (
	// Active top-level key at column 0 (`secret_scan:`).
	reActiveTopKey = regexp.MustCompile(`^([a-z_][a-z0-9_]*):`)
	// Commented top-level key (`# secret_scan:`) — exactly one `# `
	// prefix, so indented sub-keys (`#   disable:`) don't match.
	reCommentedTopKey = regexp.MustCompile(`^# ?([a-z_][a-z0-9_]*):`)
)

// templateConfigSegments parses the RAW embedded template into
// top-level segments. Segments containing Go-template directives are
// dropped — they can't be rendered without init-time answers, and the
// keys they cover (owner_name, domains, llm, capture, triage, ...) are
// in every scaffolded file anyway.
func templateConfigSegments() []templateSegment {
	data, err := templateFS.ReadFile("templates/scribe.yaml")
	if err != nil {
		return nil
	}
	var (
		segments []templateSegment
		current  *templateSegment
		pending  []string
	)
	flush := func() {
		if current != nil {
			segments = append(segments, *current)
			current = nil
		}
		pending = nil
	}
	begin := func(key, line string) {
		// A new top-level key starts its own segment even without a
		// separating blank — grouped keys (the Paths block) and a
		// commented example glued under an active block (owners under
		// domains) must stay individually addressable.
		docs := pending
		if current != nil {
			// Comment lines sitting directly above a key document THAT
			// key, not the block being closed — peel them across. A
			// fully commented segment has nothing peelable (cut == 0):
			// leave it intact.
			lines := current.lines
			cut := len(lines)
			for cut > 0 && strings.HasPrefix(lines[cut-1], "#") {
				cut--
			}
			if cut > 0 {
				docs = append([]string{}, lines[cut:]...)
				current.lines = lines[:cut]
			}
			segments = append(segments, *current)
		}
		current = &templateSegment{key: key, lines: append(docs, line)}
		pending = nil
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimRight(line, " \t")
		switch {
		case trimmed == "":
			flush()
		case reActiveTopKey.MatchString(trimmed):
			begin(reActiveTopKey.FindStringSubmatch(trimmed)[1], trimmed)
		case reCommentedTopKey.MatchString(trimmed):
			begin(reCommentedTopKey.FindStringSubmatch(trimmed)[1], trimmed)
		case current != nil:
			current.lines = append(current.lines, trimmed)
		default:
			pending = append(pending, trimmed)
		}
	}
	flush()

	out := segments[:0]
	for _, seg := range segments {
		if strings.Contains(strings.Join(seg.lines, "\n"), "{{") {
			continue
		}
		out = append(out, seg)
	}
	covered := map[string]bool{}
	for _, seg := range out {
		covered[seg.key] = true
	}
	for _, fb := range fallbackConfigSegments {
		if !covered[fb.key] {
			out = append(out, fb)
		}
	}
	return out
}

// fallbackConfigSegments covers keys whose template blocks live inside
// Go-template conditionals — every line starts with {{, so the parser
// above drops them. Without a fallback, `scribe config update` could
// never surface them to pre-existing KBs, and sources.allowed_remotes
// (the team discovery gate) is aimed at exactly that audience.
var fallbackConfigSegments = []templateSegment{
	{
		key: "sources",
		lines: []string{
			"# sources gates which project paths discovery may enroll. include /",
			"# exclude match path prefixes or globs (also against ancestors);",
			"# exclude always wins over include. Empty include = allow all.",
			"# allowed_remotes filters by git identity instead of path: when set,",
			"# only repos whose origin remote matches an entry (any spelling —",
			"# https://, git@host:, or bare host/org) are discovered, and repos",
			"# WITHOUT an origin remote are rejected.",
			"# sources:",
			"#   include:",
			"#     - ~/work",
			"#     - ~/Projects/client-*",
			"#   exclude:",
			"#     - ~/Projects/personal",
			"#   allowed_remotes:",
			"#     - github.com/myorg",
		},
	},
}

// configMentionsKey reports whether the user file already has the
// top-level key, active or commented — either way the user can see the
// option exists, so appending would duplicate. Generous on comment
// spelling (`#key:`, `##  key:`) since this is a skip-check: a false
// positive merely leaves a block unappended, while strictness would
// re-append blocks next to hand-edited comments forever.
func configMentionsKey(content, key string) bool {
	re := regexp.MustCompile(`(?m)^(?:#+[ \t]{0,3})?` + regexp.QuoteMeta(key) + `:`)
	return re.MatchString(content)
}

// missingTemplateBlocks lists template segments absent from content,
// in template order.
func missingTemplateBlocks(content string) []templateSegment {
	var out []templateSegment
	for _, seg := range templateConfigSegments() {
		if !configMentionsKey(content, seg.key) {
			out = append(out, seg)
		}
	}
	return out
}

// appendTemplateBlocks appends the segments as fully commented blocks.
// Active lines get a `# ` prefix so an appended block can never change
// behavior — absent keys keep their built-in defaults.
func appendTemplateBlocks(content string, segments []templateSegment) string {
	var b strings.Builder
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n" + configUpdateHeader)
	for _, seg := range segments {
		b.WriteString("\n")
		for _, line := range seg.lines {
			if line != "" && !strings.HasPrefix(line, "#") {
				line = "# " + line
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
