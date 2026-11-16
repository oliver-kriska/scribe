package main

import (
	"path"
	"sort"
	"strings"
)

// special_files.go — the single registry of KB files that scribe itself
// manages and that need special handling somewhere in the git pipeline.
// Three sites used to hardcode overlapping lists of these files
// (derivedRegenerable in gitops.go, semanticMergers in gitmerge.go, and
// an inline staging list in gitAddWiki); they now all derive from this
// one table, so a new shared file is registered exactly once.

// specialFileClass says how the git pipeline must treat a registered file.
type specialFileClass string

const (
	// classDerivedRegenerable — content scribe fully rebuilds (wiki
	// index, backlinks, team digest). A merge conflict on these carries
	// no information: either side can win because both go stale the
	// moment any machine regenerates.
	classDerivedRegenerable specialFileClass = "derived-regenerable"
	// classSemanticMerge — committed coordination files that accumulate
	// state from every machine. Picking a side in a conflict would throw
	// away a teammate's writes, so each has a semantic merge function.
	classSemanticMerge specialFileClass = "semantic-merge"
	// classMachineLocal — per-machine state (the discovery manifest).
	// Gitignored in team KBs; committed as-is in solo KBs. Never merged.
	classMachineLocal specialFileClass = "machine-local"
	// classAppendOnly — logs every machine appends to. Merge keeps both
	// sides' lines (mergeUnionLines).
	classAppendOnly specialFileClass = "append-only"
	// classArticle — a KB article. Only the merge driver merges these
	// (mergeArticle); the pull fallback still aborts on them.
	classArticle specialFileClass = "article"
	// classRawArticle — a captured source under raw/articles/. Merged
	// like an article, except the contextualize auto block is picked
	// deterministically (mergeRawArticle).
	classRawArticle specialFileClass = "raw-article"
)

// specialFileSpec describes one registered file.
type specialFileSpec struct {
	Class specialFileClass
	// Merge produces merged content from a conflict's two sides. Set
	// if and only if Class is classSemanticMerge or classAppendOnly
	// (asserted by test).
	Merge func(ours, theirs []byte) []byte
	// AutoStage marks files gitAddWiki stages explicitly alongside the
	// wiki content dirs. Files living under a wikiDirs directory (the
	// derived wiki/_* artifacts) are swept by the directory pathspec
	// and don't need this; scripts/dream-lease.json deliberately stays
	// out so only commitDreamLease's pathspec commit ever touches it.
	AutoStage bool
}

// specialKBFiles maps repo-relative paths to their handling spec. This
// is the one place a shared scribe-managed file gets registered. A key
// is an exact path, a path.Match pattern ("projects/*/learnings.md"), or
// "<dir>/**/*.md" for every markdown file under dir; specialFileFor
// resolves them in that order of precedence.
var specialKBFiles = withArticleDirs(map[string]specialFileSpec{
	"wiki/_index.md":       {Class: classDerivedRegenerable},
	"wiki/_backlinks.json": {Class: classDerivedRegenerable},
	"wiki/_digest.md":      {Class: classDerivedRegenerable},
	// Regenerated wholesale by `scribe hot` on every sync, so a
	// conflict here carries no information either.
	"wiki/_hot.md": {Class: classDerivedRegenerable},

	"scripts/extraction-ledger.json": {Class: classSemanticMerge, Merge: mergeLedgerContent, AutoStage: true},
	"scripts/dream-lease.json":       {Class: classSemanticMerge, Merge: mergeLeaseContent},
	"log.md":                         {Class: classAppendOnly, Merge: mergeUnionLines, AutoStage: true},
	// Session-mining ledgers. One shape, two surfaces (codex_log.go).
	// Each machine mines its own sessions, so `processed` unions cleanly;
	// the colliding line is `last_scan`, rewritten every run.
	"wiki/_sessions_log.json":       {Class: classSemanticMerge, Merge: mergeSessionsLogContent},
	"wiki/_codex_sessions_log.json": {Class: classSemanticMerge, Merge: mergeSessionsLogContent},

	"scripts/projects.json": {Class: classMachineLocal, AutoStage: true},

	"wiki/_unfetched-links.md":    {Class: classAppendOnly, Merge: mergeUnionLines},
	"projects/*/learnings.md":     {Class: classAppendOnly, Merge: mergeUnionLines},
	"projects/*/decisions-log.md": {Class: classAppendOnly, Merge: mergeUnionLines},
	"raw/articles/*.md":           {Class: classRawArticle},
})

// articleDirPattern is the specialKBFiles key suffix that matches every
// markdown file below a directory.
const articleDirPattern = "/**/*.md"

// withArticleDirs registers every content dir (wikiDirs) as the article
// class, so the list of content dirs stays in one place.
func withArticleDirs(files map[string]specialFileSpec) map[string]specialFileSpec {
	for _, dir := range wikiDirs {
		files[dir+articleDirPattern] = specialFileSpec{Class: classArticle}
	}
	return files
}

// specialFileFor returns the spec that governs repo-relative path rel:
// an exact entry first, then a path.Match pattern, then an article dir.
func specialFileFor(rel string) (specialFileSpec, bool) {
	if spec, ok := specialKBFiles[rel]; ok {
		return spec, true
	}
	for key, spec := range specialKBFiles {
		if strings.Contains(key, "*") && !strings.HasSuffix(key, articleDirPattern) {
			if ok, _ := path.Match(key, rel); ok {
				return spec, true
			}
		}
	}
	for key, spec := range specialKBFiles {
		if dir, ok := strings.CutSuffix(key, articleDirPattern); ok && strings.HasPrefix(rel, dir+"/") && strings.HasSuffix(rel, ".md") {
			return spec, true
		}
	}
	return specialFileSpec{}, false
}

// derivedRegenerable is the conflict-resolution view: files where either
// side of a conflict is acceptable because content regenerates after the
// pull. Derived from specialKBFiles — register new files there.
var derivedRegenerable = func() map[string]bool {
	out := map[string]bool{}
	for path, spec := range specialKBFiles {
		if spec.Class == classDerivedRegenerable {
			out[path] = true
		}
	}
	return out
}()

// mergeFuncFor is the conflict-resolution view for files that must be
// MERGED, not side-picked (see gitmerge.go for why): the Merge of the
// spec that governs rel, or nil.
func mergeFuncFor(rel string) func(ours, theirs []byte) []byte {
	spec, _ := specialFileFor(rel)
	return spec.Merge
}

// autoStagedSpecialFiles is the staging view: registered files gitAddWiki
// adds explicitly because they live outside (or hidden from) the wikiDirs
// sweep. Sorted for deterministic `git add` argument order.
func autoStagedSpecialFiles() []string {
	var out []string
	for path, spec := range specialKBFiles {
		if spec.AutoStage {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}
