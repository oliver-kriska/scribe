package main

import (
	"slices"
	"testing"
)

// TestSpecialFileRegistryViews pins the three derived views to the one
// registry, and the registry to its known contents — so a file can only
// be added/moved by editing specialKBFiles, and each class keeps its
// invariants (Merge set iff semantic-merge).
func TestSpecialFileRegistryViews(t *testing.T) {
	// Invariants per class.
	for path, spec := range specialKBFiles {
		hasMerge := spec.Merge != nil
		wantMerge := spec.Class == classSemanticMerge
		if hasMerge != wantMerge {
			t.Errorf("%s (class %s): Merge set = %v, want %v — Merge belongs to semantic-merge entries only",
				path, spec.Class, hasMerge, wantMerge)
		}
		if spec.Class == classDerivedRegenerable && spec.AutoStage {
			t.Errorf("%s: derived wiki artifacts are staged by the wikiDirs sweep, not AutoStage", path)
		}
	}

	// derivedRegenerable == exactly the derived-regenerable entries.
	for path, spec := range specialKBFiles {
		if derivedRegenerable[path] != (spec.Class == classDerivedRegenerable) {
			t.Errorf("derivedRegenerable[%s] = %v, registry class = %s", path, derivedRegenerable[path], spec.Class)
		}
	}
	for path := range derivedRegenerable {
		if _, ok := specialKBFiles[path]; !ok {
			t.Errorf("derivedRegenerable has %s but the registry does not", path)
		}
	}

	// semanticMergers == exactly the semantic-merge entries.
	for path, spec := range specialKBFiles {
		if (semanticMergers[path] != nil) != (spec.Class == classSemanticMerge) {
			t.Errorf("semanticMergers[%s] present = %v, registry class = %s", path, semanticMergers[path] != nil, spec.Class)
		}
	}
	for path := range semanticMergers {
		if _, ok := specialKBFiles[path]; !ok {
			t.Errorf("semanticMergers has %s but the registry does not", path)
		}
	}

	// The staging view == exactly the AutoStage entries.
	staged := autoStagedSpecialFiles()
	for path, spec := range specialKBFiles {
		if slices.Contains(staged, path) != spec.AutoStage {
			t.Errorf("autoStagedSpecialFiles contains %s = %v, registry AutoStage = %v",
				path, slices.Contains(staged, path), spec.AutoStage)
		}
	}
	if !slices.IsSorted(staged) {
		t.Errorf("autoStagedSpecialFiles not sorted: %v", staged)
	}

	// Pin the historical contents so an accidental removal (which would
	// silently change conflict/staging behavior) is loud.
	want := map[string]specialFileClass{
		"wiki/_index.md":                 classDerivedRegenerable,
		"wiki/_backlinks.json":           classDerivedRegenerable,
		"wiki/_digest.md":                classDerivedRegenerable,
		"scripts/extraction-ledger.json": classSemanticMerge,
		"scripts/dream-lease.json":       classSemanticMerge,
		"log.md":                         classSemanticMerge,
		"scripts/projects.json":          classMachineLocal,
	}
	for path, class := range want {
		if spec, ok := specialKBFiles[path]; !ok || spec.Class != class {
			t.Errorf("registry missing or misclassifies %s: want %s, got %+v", path, class, spec)
		}
	}
	wantStaged := []string{"log.md", "scripts/extraction-ledger.json", "scripts/projects.json"}
	if !slices.Equal(staged, wantStaged) {
		t.Errorf("autoStagedSpecialFiles() = %v, want %v", staged, wantStaged)
	}
}
