package main

import "sort"

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
)

// specialFileSpec describes one registered file.
type specialFileSpec struct {
	Class specialFileClass
	// Merge produces merged content from a conflict's two sides. Set
	// if and only if Class is classSemanticMerge (asserted by test).
	Merge func(ours, theirs []byte) []byte
	// AutoStage marks files gitAddWiki stages explicitly alongside the
	// wiki content dirs. Files living under a wikiDirs directory (the
	// derived wiki/_* artifacts) are swept by the directory pathspec
	// and don't need this; scripts/dream-lease.json deliberately stays
	// out so only commitDreamLease's pathspec commit ever touches it.
	AutoStage bool
}

// specialKBFiles maps repo-relative paths to their handling spec. This
// is the one place a shared scribe-managed file gets registered.
var specialKBFiles = map[string]specialFileSpec{
	"wiki/_index.md":       {Class: classDerivedRegenerable},
	"wiki/_backlinks.json": {Class: classDerivedRegenerable},
	"wiki/_digest.md":      {Class: classDerivedRegenerable},

	"scripts/extraction-ledger.json": {Class: classSemanticMerge, Merge: mergeLedgerContent, AutoStage: true},
	"scripts/dream-lease.json":       {Class: classSemanticMerge, Merge: mergeLeaseContent},
	"log.md":                         {Class: classSemanticMerge, Merge: mergeUnionLines, AutoStage: true},

	"scripts/projects.json": {Class: classMachineLocal, AutoStage: true},
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

// semanticMergers is the conflict-resolution view for coordination files
// that must be MERGED, not side-picked (see gitmerge.go for why). Derived
// from specialKBFiles — register new files there.
var semanticMergers = func() map[string]func(ours, theirs []byte) []byte {
	out := map[string]func(ours, theirs []byte) []byte{}
	for path, spec := range specialKBFiles {
		if spec.Class == classSemanticMerge {
			out[path] = spec.Merge
		}
	}
	return out
}()

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
