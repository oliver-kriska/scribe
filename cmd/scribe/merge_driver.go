package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// merge_driver.go — `scribe merge-file`, a git merge driver for team KBs.
//
// Git calls a merge driver for each conflicting file in every operation
// that merges (rebase, merge, cherry-pick, stash apply), including a
// member's manual `git pull`. The KB's .gitattributes maps files to
// `merge=scribe`, and each clone's .git/config names this binary
// (ensureMergeDriver). A clone without that config falls back to git's
// built-in merge, so nothing gets worse when the driver is missing.
//
// Every merger here is a pure function of (base, side A, side B, path)
// with no LLM or network call, and is symmetric: two machines merging
// the same conflict from opposite sides write the same bytes.

// MergeFileCmd is the merge driver: `scribe merge-file %O %A %B %P`.
type MergeFileCmd struct {
	Base    string `arg:"" help:"Common ancestor version (git's %O)."`
	Current string `arg:"" help:"Current version (git's %A); the result is written here."`
	Other   string `arg:"" help:"Other branch's version (git's %B)."`
	Path    string `arg:"" help:"Repo-relative path of the file (git's %P)."`
}

// ReadOnly keeps the driver out of the run records: git calls it once
// per conflicted file and it writes only git's own temp file.
func (c *MergeFileCmd) ReadOnly() bool { return true }

// errMergeUnresolved makes the driver exit 1, which tells git the file
// still conflicts.
var errMergeUnresolved = errors.New("merge-file: conflict left for git")

func (c *MergeFileCmd) Run() error {
	base, err := os.ReadFile(c.Base)
	if err != nil {
		base = nil // add/add conflicts have an empty or missing base
	}
	current, err := os.ReadFile(c.Current)
	if err != nil {
		return err
	}
	other, err := os.ReadFile(c.Other)
	if err != nil {
		return err
	}
	rel := filepath.ToSlash(c.Path)
	merged, class, ok := mergeKBFile(rel, base, current, other)
	if !ok {
		// Leave git's usual conflict result so the pull fallback (or a
		// person) sees exactly what the built-in merge would have left.
		_, _ = runCmdErr("", "git", "merge-file", "-L", "current", "-L", "base", "-L", "other", c.Current, c.Base, c.Other)
		fmt.Fprintf(os.Stderr, "%s%s %s unresolved\n", mergeDriverLogPrefix, classOrUnclassified(class), rel)
		return errMergeUnresolved
	}
	if err := os.WriteFile(c.Current, merged, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s%s %s resolved\n", mergeDriverLogPrefix, class, rel)
	return nil
}

// mergeDriverLogPrefix starts every line the driver prints. Git passes
// the driver's stderr through; relayMergeDriverLines copies these lines
// into the sync log.
const mergeDriverLogPrefix = "merge-file: "

func classOrUnclassified(class specialFileClass) specialFileClass {
	if class == "" {
		return "unclassified"
	}
	return class
}

// relayMergeDriverLines logs the driver's per-file lines from captured git
// output, so the sync log records what the driver resolved.
func relayMergeDriverLines(gitOutput string) {
	// git's progress lines end in \r, so a driver line can follow one.
	for line := range strings.SplitSeq(strings.ReplaceAll(gitOutput, "\r", "\n"), "\n") {
		if strings.HasPrefix(line, mergeDriverLogPrefix) {
			logMsg("git", "%s", strings.TrimSpace(line))
		}
	}
}

// mergeKBFile merges one conflicted file by its path class. ok is false
// when no merger applies; the caller then leaves the conflict to git.
func mergeKBFile(rel string, base, current, other []byte) ([]byte, specialFileClass, bool) {
	spec, found := specialFileFor(rel)
	if !found {
		return nil, "", false
	}
	switch spec.Class {
	case classDerivedRegenerable:
		// Either side is fine (it regenerates); pick one deterministically.
		first, _ := lexOrder(current, other)
		return first, spec.Class, true
	case classSemanticMerge:
		// Existing mergers keep their own side rules (ours = current).
		return spec.Merge(current, other), spec.Class, true
	case classAppendOnly:
		first, second := lexOrder(current, other)
		return spec.Merge(first, second), spec.Class, true
	case classArticle:
		merged, ok := mergeArticle(string(base), string(current), string(other))
		return []byte(merged), spec.Class, ok
	case classRawArticle:
		merged, ok := mergeRawArticle(string(base), string(current), string(other))
		return []byte(merged), spec.Class, ok
	default:
		return nil, spec.Class, false
	}
}

// lexOrder returns a and b sorted lexically: the symmetric tie-break
// every merger here uses when the sides truly disagree.
func lexOrder[T ~string | ~[]byte](a, b T) (T, T) {
	if string(a) <= string(b) {
		return a, b
	}
	return b, a
}

// pickThreeWay resolves one value from three versions: equal sides or a
// one-sided change win; ok is false when both sides changed it.
func pickThreeWay(base, a, b string) (string, bool) {
	switch {
	case a == b, b == base:
		return a, true
	case a == base:
		return b, true
	}
	return "", false
}

// mergeArticle merges a markdown article: frontmatter field by field,
// the body as a 3-way text merge. ok is false only when a side's
// frontmatter is not valid YAML.
func mergeArticle(base, a, b string) (string, bool) {
	if merged, ok := pickThreeWay(base, a, b); ok {
		return merged, true
	}
	fmBase, bodyBase := splitArticle(base)
	fmA, bodyA := splitArticle(a)
	fmB, bodyB := splitArticle(b)
	fm, ok := mergeFrontmatter(fmBase, fmA, fmB)
	if !ok {
		return "", false
	}
	body, ok := mergeBody(bodyBase, bodyA, bodyB)
	if !ok {
		return "", false
	}
	if fm == "" {
		return body, true
	}
	return "---\n" + fm + "---\n" + body, true
}

// splitArticle returns the frontmatter YAML (without fences) and the body.
// A file without valid fences is all body.
func splitArticle(s string) (fm, body string) {
	span, err := frontmatterSpan(s)
	if err != nil {
		return "", s
	}
	return s[span.YAMLStart:span.YAMLEnd], s[span.BodyStart:]
}

// mergeListKeys are frontmatter lists merged as an ordered union.
var mergeListKeys = map[string]bool{"related": true, "sources": true, "tags": true, "aliases": true}

// mergeFrontmatter merges frontmatter YAML key by key. Keys keep base
// order; keys new on either side follow, sorted.
func mergeFrontmatter(base, a, b string) (string, bool) {
	if merged, ok := pickThreeWay(base, a, b); ok {
		return merged, true
	}
	baseMap, okBase := parseYAMLMapping(base)
	aMap, okA := parseYAMLMapping(a)
	bMap, okB := parseYAMLMapping(b)
	if !okBase || !okA || !okB {
		return "", false
	}
	var keys []string
	seen := map[string]bool{}
	for _, k := range baseMap.keys {
		seen[k] = true
		keys = append(keys, k)
	}
	var added []string
	for _, k := range append(append([]string{}, aMap.keys...), bMap.keys...) {
		if !seen[k] {
			seen[k] = true
			added = append(added, k)
		}
	}
	sort.Strings(added)
	keys = append(keys, added...)

	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, k := range keys {
		value := mergeFrontmatterValue(k, baseMap.values[k], aMap.values[k], bMap.values[k])
		if value == nil {
			continue
		}
		out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, value)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return "", false
	}
	return buf.String(), true
}

// yamlMapping is a parsed frontmatter mapping that keeps key order.
type yamlMapping struct {
	keys   []string
	values map[string]*yaml.Node
}

func parseYAMLMapping(s string) (yamlMapping, bool) {
	m := yamlMapping{values: map[string]*yaml.Node{}}
	if strings.TrimSpace(s) == "" {
		return m, true
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(s), &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return m, false
	}
	mapping := doc.Content[0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i].Value
		if _, dup := m.values[key]; !dup {
			m.keys = append(m.keys, key)
		}
		m.values[key] = mapping.Content[i+1]
	}
	return m, true
}

// yamlText renders a node for comparison; "" means absent.
func yamlText(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	out, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return string(out)
}

// mergeFrontmatterValue merges one key. nil means the key is dropped.
func mergeFrontmatterValue(key string, base, a, b *yaml.Node) *yaml.Node {
	textBase, textA, textB := yamlText(base), yamlText(a), yamlText(b)
	switch {
	case textA == textB, textB == textBase:
		return a
	case textA == textBase:
		return b
	}
	// Both sides changed the key.
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case key == "updated" && a.Kind == yaml.ScalarNode && b.Kind == yaml.ScalarNode:
		if a.Value >= b.Value {
			return a
		}
		return b
	case mergeListKeys[key] && a.Kind == yaml.SequenceNode && b.Kind == yaml.SequenceNode:
		return unionSequence(base, a, b)
	}
	if textA <= textB {
		return a
	}
	return b
}

// unionSequence merges two YAML lists: base items still on either side
// first (base order), then the new items of the side that sorts first,
// then the other side's. Items are deduped after normalizing wikilinks.
func unionSequence(base, a, b *yaml.Node) *yaml.Node {
	first, second := a, b
	if yamlText(b) < yamlText(a) {
		first, second = b, a
	}
	present := map[string]bool{}
	for _, side := range []*yaml.Node{a, b} {
		for _, item := range side.Content {
			present[normalizeListItem(item)] = true
		}
	}
	out := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: first.Style}
	seen := map[string]bool{}
	add := func(item *yaml.Node) {
		key := normalizeListItem(item)
		if seen[key] || !present[key] {
			return
		}
		seen[key] = true
		out.Content = append(out.Content, item)
	}
	if base != nil && base.Kind == yaml.SequenceNode {
		for _, item := range base.Content {
			add(item)
		}
	}
	for _, side := range []*yaml.Node{first, second} {
		for _, item := range side.Content {
			add(item)
		}
	}
	return out
}

// normalizeListItem keys a list item for dedupe: "[[Foo Bar]]" and
// "foo bar" are the same entry.
func normalizeListItem(n *yaml.Node) string {
	if n.Kind != yaml.ScalarNode {
		return yamlText(n)
	}
	v := strings.TrimSpace(n.Value)
	v = strings.TrimSuffix(strings.TrimPrefix(v, "[["), "]]")
	return strings.ToLower(strings.TrimSpace(v))
}

// conflictMarkerSize is wider than any marker an article shows in prose,
// so mergeBody never mistakes article text for git's markers.
const conflictMarkerSize = 41

// mergedSectionHeading starts the section that keeps the other side's
// text of a hunk both machines edited.
const mergedSectionHeading = "## Merged from another machine"

// mergeBody merges article bodies with `git merge-file`. Clean hunks
// merge as usual. For a hunk both sides changed, the side that sorts
// first keeps its text in place and the other side's text goes into a
// dated section at the end, unless the body already has that text.
func mergeBody(base, a, b string) (string, bool) {
	if merged, ok := pickThreeWay(base, a, b); ok {
		return merged, true
	}
	primary, secondary := lexOrder(a, b)
	dir, err := os.MkdirTemp("", "scribe-merge-")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(dir)
	paths := map[string]string{"primary": primary, "base": base, "secondary": secondary}
	for name, content := range paths {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return "", false
		}
	}
	cmd := exec.Command("git", "merge-file", "-p", fmt.Sprintf("--marker-size=%d", conflictMarkerSize), //nolint:noctx // local, bounded subprocess
		filepath.Join(dir, "primary"), filepath.Join(dir, "base"), filepath.Join(dir, "secondary"))
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// Exit code = number of conflicts (1..127); anything else is a failure.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() <= 0 || exitErr.ExitCode() > 127 {
			return "", false
		}
	}
	kept, otherHunks := splitConflictHunks(stdout.String())
	var extra []string
	for _, hunk := range otherHunks {
		text := strings.TrimSpace(hunk)
		if text != "" && !strings.Contains(kept, text) && !strings.Contains(strings.Join(extra, "\n"), text) {
			extra = append(extra, text)
		}
	}
	if len(extra) == 0 {
		return kept, true
	}
	date := time.Now().UTC().Format("2006-01-02")
	return strings.TrimRight(kept, "\n") + "\n\n" + mergedSectionHeading + " (" + date + ")\n\n" + strings.Join(extra, "\n\n") + "\n", true
}

// splitConflictHunks walks merge-file output: lines outside conflicts and
// the primary side of each conflict go to kept; the secondary side of
// each conflict is returned on its own. A diff3 base section is dropped.
func splitConflictHunks(merged string) (kept string, otherHunks []string) {
	open := strings.Repeat("<", conflictMarkerSize) + " "
	baseSep := strings.Repeat("|", conflictMarkerSize)
	mid := strings.Repeat("=", conflictMarkerSize)
	closing := strings.Repeat(">", conflictMarkerSize) + " "
	const (
		outside = iota
		inPrimary
		inBase
		inSecondary
	)
	state := outside
	var out, hunk strings.Builder
	for _, line := range strings.SplitAfter(merged, "\n") {
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case state == outside && strings.HasPrefix(trimmed, open):
			state = inPrimary
		case state == inPrimary && strings.HasPrefix(trimmed, baseSep):
			state = inBase
		case (state == inPrimary || state == inBase) && trimmed == mid:
			state = inSecondary
			hunk.Reset()
		case state == inSecondary && strings.HasPrefix(trimmed, closing):
			state = outside
			otherHunks = append(otherHunks, hunk.String())
		case state == inSecondary:
			hunk.WriteString(line)
		case state == inBase:
		default:
			out.WriteString(line)
		}
	}
	return out.String(), otherHunks
}

// retrievalContextRE matches the block contextualize inserts
// (retrievalContextBlock). shortcut: a multi-paragraph context keeps its
// later paragraphs in the body; upgrade if contextualize starts writing them.
var retrievalContextRE = regexp.MustCompile(`\n` + regexp.QuoteMeta(retrievalContextMarker) + `\n> \*\*Retrieval context \(auto\):\*\* ([^\n]*)\n\n`)

// splitRetrievalContext removes the auto block from a raw article and
// returns its paragraph ("" when there is none) and the remaining text.
func splitRetrievalContext(content string) (paragraph, rest string) {
	loc := retrievalContextRE.FindStringSubmatchIndex(content)
	if loc == nil {
		return "", content
	}
	return content[loc[2]:loc[3]], content[:loc[0]] + content[loc[1]:]
}

// mergeRawArticle merges a raw article like an article, with the
// contextualize auto block taken out first and put back after: a
// one-sided block wins, and when both sides wrote different blocks the
// one that sorts first is kept, so both machines converge.
func mergeRawArticle(base, a, b string) (string, bool) {
	if merged, ok := pickThreeWay(base, a, b); ok {
		return merged, true
	}
	paraBase, restBase := splitRetrievalContext(base)
	paraA, restA := splitRetrievalContext(a)
	paraB, restB := splitRetrievalContext(b)
	rest, ok := mergeArticle(restBase, restA, restB)
	if !ok {
		return "", false
	}
	para, ok := pickThreeWay(paraBase, paraA, paraB)
	if !ok {
		first, second := lexOrder(paraA, paraB)
		para = first
		if para == "" {
			para = second
		}
	}
	if para == "" {
		return rest, true
	}
	merged, err := insertRetrievalContext(rest, para)
	if err != nil {
		return "", false
	}
	return merged, true
}

// mergeDriverCommand is the merge.scribe.driver value. A var so tests can
// point git at the test binary. The absolute path matters: LaunchAgents
// run with a short PATH.
var mergeDriverCommand = func() string {
	return shellQuote(resolveScribeBinary()) + " merge-file %O %A %B %P"
}

// ensureMergeDriver writes merge.scribe.driver into the KB's .git/config
// when it is missing or stale. Without .gitattributes lines that name
// merge=scribe the setting is inert, so it is safe on any KB.
func ensureMergeDriver(root string) error {
	if !hasGit(root) {
		return nil
	}
	want := mergeDriverCommand()
	if runCmd(root, "git", "config", "--get", "merge.scribe.driver") == want {
		return nil
	}
	if out, err := runCmdErr(root, "git", "config", "merge.scribe.name", "scribe KB merge driver"); err != nil {
		return fmt.Errorf("git config merge.scribe.name: %s", out)
	}
	if out, err := runCmdErr(root, "git", "config", "merge.scribe.driver", want); err != nil {
		return fmt.Errorf("git config merge.scribe.driver: %s", out)
	}
	return nil
}

// gitattributesLines lists the .gitattributes lines a team KB needs: one
// for markdown (the driver picks the class) and one per registered
// non-markdown file it can merge.
func gitattributesLines() []string {
	lines := []string{"*.md merge=scribe"}
	for rel, spec := range specialKBFiles {
		if strings.HasSuffix(rel, ".md") || strings.Contains(rel, "*") || spec.Class == classMachineLocal {
			continue
		}
		lines = append(lines, rel+" merge=scribe")
	}
	sort.Strings(lines[1:])
	return lines
}

// ensureGitattributes appends the missing merge=scribe lines to the KB's
// .gitattributes. Returns true when it wrote the file.
func ensureGitattributes(root string) (bool, error) {
	path := filepath.Join(root, ".gitattributes")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	have := map[string]bool{}
	for line := range strings.SplitSeq(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, line := range gitattributesLines() {
		if !have[line] {
			missing = append(missing, line)
		}
	}
	if len(missing) == 0 {
		return false, nil
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if !have["# scribe merge driver (scribe merge-file); see `scribe doctor`"] {
		content += "# scribe merge driver (scribe merge-file); see `scribe doctor`\n"
	}
	content += strings.Join(missing, "\n") + "\n"
	return true, os.WriteFile(path, []byte(content), 0o644)
}

// mergeDriverCheck is the doctor `merge-driver` row. It shows for team
// KBs and for any KB whose .gitattributes names merge=scribe. Doctor only
// reports: sync sets the driver before every pull, and init sets it too.
func mergeDriverCheck(root string, team bool) (check, bool) {
	if !hasGit(root) {
		return check{}, false
	}
	attrs, _ := os.ReadFile(filepath.Join(root, ".gitattributes"))
	mapped := strings.Contains(string(attrs), "merge=scribe")
	if !team && !mapped {
		return check{}, false
	}
	row := check{Section: "state", Name: "merge-driver", Status: statusOK, Detail: "merge.scribe.driver set; .gitattributes maps KB files to merge=scribe"}
	driver := runCmd(root, "git", "config", "--get", "merge.scribe.driver")
	switch {
	case !mapped:
		row.Status, row.Detail = statusWarn, "team KB without merge=scribe lines in .gitattributes: content conflicts stop the pull"
		row.Fix = "run `scribe init`, then commit and push .gitattributes"
	case driver == "":
		row.Status, row.Detail = statusWarn, "merge.scribe.driver is not set: git uses its built-in merge"
		row.Fix = "run `scribe init` (sync also sets it before each pull)"
	case driver != mergeDriverCommand():
		row.Status, row.Detail = statusWarn, fmt.Sprintf("merge.scribe.driver is %q, expected %q", driver, mergeDriverCommand())
		row.Fix = "run `scribe init` (sync also sets it before each pull)"
	}
	return row, true
}
