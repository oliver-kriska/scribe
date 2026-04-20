package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// runApplyIdentities parses wiki/_identity-proposals.md, finds blocks
// whose Suggested action is `add-aliases`, and appends the surface forms
// to the matching people/*.md file's `aliases:` frontmatter list.
//
// Safety guarantees — never expands beyond what the proposal literally
// said:
//
//  1. Only blocks with `Suggested action: add-aliases` are touched.
//     `create-new` and `skip` are deliberately ignored — creating a new
//     page needs human judgment on the slug.
//  2. Only aliases NOT already present are added; files whose alias list
//     already contains every proposed form are left untouched.
//  3. Files whose `Existing page:` path doesn't resolve are skipped with
//     a warning. We never create a page we didn't expect to already
//     exist.
//  4. Low-confidence blocks are skipped unless --apply-low.
//
// Re-runnable: a second apply pass is a silent no-op once everything is
// already in place.
func runApplyIdentities(proposalsPath string, applyLow, dryRun bool) error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path := proposalsPath
	if path == "" {
		path = filepath.Join(root, "wiki", "_identity-proposals.md")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no identity proposals — run `scribe lint --identities` first")
	}
	blocks := parseIdentityBlocks(string(data))
	if len(blocks) == 0 {
		fmt.Println("no add-aliases blocks in identity proposals")
		return nil
	}

	touched, skipped := 0, 0
	for _, b := range blocks {
		if b.Action != "add-aliases" {
			skipped++
			continue
		}
		if !applyLow && strings.EqualFold(b.Confidence, "low") {
			logMsg("identities", "skip %s: confidence=low (use --apply-low to force)", b.Page)
			skipped++
			continue
		}
		abs := filepath.Join(root, b.Page)
		if _, err := os.Stat(abs); err != nil {
			logMsg("identities", "skip %s: page not found", b.Page)
			skipped++
			continue
		}
		added, err := appendAliasesToPeopleFile(abs, b.SurfaceForms, dryRun)
		if err != nil {
			logMsg("identities", "skip %s: %v", b.Page, err)
			skipped++
			continue
		}
		if added == 0 {
			continue
		}
		prefix := "applied"
		if dryRun {
			prefix = "would apply"
		}
		logMsg("identities", "%s: %s (+%d alias)", prefix, b.Page, added)
		touched++
	}
	fmt.Printf("\nidentities apply: touched %d file(s), skipped %d block(s)%s\n",
		touched, skipped, dryRunSuffix(dryRun))
	return nil
}

func dryRunSuffix(dryRun bool) string {
	if dryRun {
		return " (dry-run — no files written)"
	}
	return ""
}

// identityProposalBlock is one `### Canonical Name` block parsed out of
// the proposals markdown. Only fields we need to act on are captured.
type identityProposalBlock struct {
	Page         string
	SurfaceForms []string
	Confidence   string
	Action       string
}

var (
	identityBlockStartRE = regexp.MustCompile(`^###\s+(.+?)\s*$`)
	identityPageRE       = regexp.MustCompile(`(?i)^-\s*Existing page:\s*(\S+)\s*$`)
	identityConfRE       = regexp.MustCompile(`(?i)^-\s*Confidence:\s*(\S+)\s*$`)
	identityActionRE     = regexp.MustCompile(`(?i)^-\s*Suggested action:\s*(\S+)\s*$`)
	identityFormItemRE   = regexp.MustCompile(`^\s{2,}-\s*(.+?)\s*$`)
)

// parseIdentityBlocks walks the proposal markdown line-by-line. The
// format is fixed by prompts/identities.md — `###` starts a block,
// dashed items follow, `---` or a new `###` ends it.
func parseIdentityBlocks(body string) []identityProposalBlock {
	var out []identityProposalBlock
	var cur *identityProposalBlock
	inSurfaceForms := false
	flush := func() {
		if cur != nil && cur.Page != "" {
			out = append(out, *cur)
		}
		cur = nil
		inSurfaceForms = false
	}
	for line := range strings.SplitSeq(body, "\n") {
		if m := identityBlockStartRE.FindStringSubmatch(line); m != nil {
			flush()
			cur = &identityProposalBlock{}
			continue
		}
		if cur == nil {
			continue
		}
		if m := identityPageRE.FindStringSubmatch(line); m != nil {
			cur.Page = strings.TrimSpace(strings.TrimPrefix(m[1], "./"))
			inSurfaceForms = false
			continue
		}
		if m := identityConfRE.FindStringSubmatch(line); m != nil {
			cur.Confidence = m[1]
			inSurfaceForms = false
			continue
		}
		if m := identityActionRE.FindStringSubmatch(line); m != nil {
			cur.Action = strings.ToLower(m[1])
			inSurfaceForms = false
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "- Surface forms:") {
			inSurfaceForms = true
			continue
		}
		if inSurfaceForms {
			if m := identityFormItemRE.FindStringSubmatch(line); m != nil {
				cur.SurfaceForms = append(cur.SurfaceForms, m[1])
				continue
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			// Any non-indented line ends the surface-forms sub-list.
			inSurfaceForms = false
		}
	}
	flush()
	return out
}

// appendAliasesToPeopleFile reads a people/*.md file, appends forms to
// its `aliases:` frontmatter list (creating the key if needed), and
// returns how many NEW aliases were added. Idempotent — running twice
// with the same input yields 0 on the second pass.
func appendAliasesToPeopleFile(path string, forms []string, dryRun bool) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return 0, fmt.Errorf("no frontmatter")
	}
	end := strings.Index(content[3:], "\n---")
	if end < 0 {
		return 0, fmt.Errorf("no closing frontmatter delimiter")
	}
	fmBlock := content[3 : end+3]
	rest := content[end+3:]

	existing := existingAliases(fmBlock)
	added := 0
	var toAdd []string
	for _, f := range forms {
		key := strings.ToLower(strings.TrimSpace(f))
		if key == "" || existing[key] {
			continue
		}
		existing[key] = true
		toAdd = append(toAdd, f)
		added++
	}
	if added == 0 {
		return 0, nil
	}
	if dryRun {
		return added, nil
	}

	newFM := insertOrExtendAliases(fmBlock, toAdd)
	newContent := "---" + newFM + rest
	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		return 0, err
	}
	return added, nil
}

// existingAliases returns a lowercase set of aliases present in a
// frontmatter block. Handles both inline (`aliases: [a, b]`) and block
// list forms (`aliases:\n  - a\n  - b`). Overly permissive by design —
// we want false positives (skip adding a duplicate) over false negatives
// (add a dup).
func existingAliases(fm string) map[string]bool {
	out := make(map[string]bool)
	lines := strings.Split(fm, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "aliases:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "aliases:"))
		// Inline array form: `aliases: [a, b, "c"]`
		if strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]") {
			items := strings.SplitSeq(rest[1:len(rest)-1], ",")
			for it := range items {
				it = strings.TrimSpace(it)
				it = strings.Trim(it, `"'`)
				if it != "" {
					out[strings.ToLower(it)] = true
				}
			}
			return out
		}
		// Block list form: subsequent indented `- x` lines.
		for j := i + 1; j < len(lines); j++ {
			l := lines[j]
			if !strings.HasPrefix(l, "  -") && !strings.HasPrefix(l, "  - ") {
				// Any non-list line ends the list.
				if strings.TrimSpace(l) == "" {
					continue
				}
				break
			}
			item := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "-"))
			item = strings.Trim(item, `"'`)
			if item != "" {
				out[strings.ToLower(item)] = true
			}
		}
		return out
	}
	return out
}

// insertOrExtendAliases adds entries to the `aliases:` frontmatter list,
// creating the key when absent. Uses the block-list form for both new
// and extended cases because it round-trips through parseFrontmatter
// cleanly and plays well with line-oriented diffs.
func insertOrExtendAliases(fm string, toAdd []string) string {
	lines := strings.Split(fm, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "aliases:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "aliases:"))
		if strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]") {
			// Convert inline to block form with the additions.
			existing := rest[1 : len(rest)-1]
			var items []string
			for it := range strings.SplitSeq(existing, ",") {
				it = strings.TrimSpace(it)
				if it == "" {
					continue
				}
				items = append(items, strings.Trim(it, `"'`))
			}
			items = append(items, toAdd...)
			blk := make([]string, 0, 1+len(items))
			blk = append(blk, "aliases:")
			for _, it := range items {
				blk = append(blk, "  - "+it)
			}
			lines[i] = strings.Join(blk, "\n")
			return strings.Join(lines, "\n")
		}
		// Find end of existing block list, then insert after.
		insertAt := i + 1
		for j := i + 1; j < len(lines); j++ {
			l := lines[j]
			if strings.HasPrefix(l, "  - ") || strings.TrimSpace(l) == "" {
				insertAt = j + 1
				continue
			}
			break
		}
		var newLines []string
		for _, a := range toAdd {
			newLines = append(newLines, "  - "+a)
		}
		out := append([]string{}, lines[:insertAt]...)
		out = append(out, newLines...)
		out = append(out, lines[insertAt:]...)
		return strings.Join(out, "\n")
	}
	// `aliases:` not present — append the whole block.
	lines = append(lines, "aliases:")
	for _, a := range toAdd {
		lines = append(lines, "  - "+a)
	}
	return strings.Join(lines, "\n")
}
