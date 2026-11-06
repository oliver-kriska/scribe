package main

import (
	"fmt"
	"strings"
)

// Warning classes for the superseded-marker checks.
const (
	lintClassSupersededSpelling = "non-canonical status: superseded"
	lintClassSupersededByLive   = "superseded_by without status: superseded"
)

// supersededMarkerWarnings returns the lint warnings for an article's
// superseded marker, each tagged with its warning class:
//
//   - a status that isSuperseded accepts but that is not spelled exactly
//     `superseded` ("Superseded", " superseded "). The article is already
//     treated as superseded; the warning makes the KB converge on the
//     canonical spelling.
//   - a non-empty `superseded_by:` on an article whose status is not
//     superseded. The article is still indexed and linked as live, which is
//     usually an oversight. Lint only reports it: whether the article is
//     really replaced is the author's call.
func supersededMarkerWarnings(fm *Frontmatter) []supersededWarning {
	if fm == nil {
		return nil
	}
	var out []supersededWarning
	if isSuperseded(fm) && fm.Status != statusSuperseded {
		msg := fmt.Sprintf("status %q should be spelled `status: superseded`", fm.Status)
		out = append(out, supersededWarning{lintClassSupersededSpelling, msg})
	}
	if !isSuperseded(fm) && hasRelationTarget(fm.SupersededBy) {
		msg := "has superseded_by: but is not marked `status: superseded`, so it is still indexed and linked as live"
		out = append(out, supersededWarning{lintClassSupersededByLive, msg})
	}
	return out
}

type supersededWarning struct{ class, msg string }

// hasRelationTarget reports whether a relation field holds any non-blank
// target. It descends into nested lists: an unquoted `superseded_by: [[X]]`
// parses as a list inside a list.
func hasRelationTarget(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.Trim(strings.TrimSpace(t), "[]") != ""
	case []string:
		for _, s := range t {
			if hasRelationTarget(s) {
				return true
			}
		}
	case []any:
		for _, item := range t {
			if hasRelationTarget(item) {
				return true
			}
		}
	}
	return false
}

// lintSupersededMarkers reports supersededMarkerWarnings for one article.
func lintSupersededMarkers(rep *lintReport, rel string, content []byte) {
	fm, err := parseFrontmatter(content)
	if err != nil {
		return
	}
	for _, w := range supersededMarkerWarnings(fm) {
		rep.warnf(w.class, "%s: %s", rel, w.msg)
	}
}
