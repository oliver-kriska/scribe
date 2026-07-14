package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Warning classes for grouped lint output. Constants keep the warnf call
// sites and the lintHints table from drifting apart.
const (
	lintClassIndexTierMissing = "index_tier missing"
	lintClassThinArticle      = "thin article"
	lintClassBloatedArticle   = "bloated article"
	lintClassRollingOvergrown = "rolling file overgrown"
	lintClassFilenameAsTitle  = "filename-as-title duplicate"
	lintClassSelfNamedDir     = "directory named after the KB"
)

// lintHints maps a warning class to the command that remediates it.
// Single source of truth for remediation hints: the grouped summary
// appends "(run: <cmd>)" and verbose per-file lines append
// "(run `<cmd>`)" from this table — call sites never embed hints in
// their messages. Add an entry here when a new class gains a fix
// command; classes without one simply render bare.
var lintHints = map[string]string{
	lintClassIndexTierMissing: "scribe tier write --missing-only",
	lintClassFilenameAsTitle:  "scribe lint --fix",
}

// lintReport accumulates findings during a structural lint run and
// controls how they render. Three modes:
//
//   - default: per-file warnings are counted by class and printed as a
//     grouped summary by flush() — a production KB can emit hundreds of
//     identical "index_tier missing" lines, which is noise per-file but
//     signal as "412× index_tier missing".
//   - verbose: every warning prints per-file as it's found (the
//     pre-grouping behavior); flush() is a no-op.
//   - quiet: warnings and info lines are suppressed entirely — only
//     per-file ERROR lines and the caller's final summary line reach
//     stdout. Used when lint runs inside `scribe sync` so the cron log
//     isn't flooded mid-extract.
//
// ERROR lines always print per-file in every mode: each one is
// individually actionable, so grouping them would hide the work.
type lintReport struct {
	w        io.Writer
	verbose  bool
	quiet    bool
	errors   int
	warnings int

	classCounts map[string]int
	// errKinds tracks which remediation buckets the Phase-1 frontmatter
	// errors fell into, so the closing verdict can print a tailored
	// how-to-fix hint (some classes `--fix` repairs, others need a human).
	errKinds map[errKind]int
}

func newLintReport(w io.Writer, verbose, quiet bool) *lintReport {
	return &lintReport{w: w, verbose: verbose, quiet: quiet, classCounts: make(map[string]int), errKinds: make(map[errKind]int)}
}

// errKind buckets a frontmatter validation error by how it gets resolved.
type errKind int

const (
	errKindNone             errKind = iota
	errKindFixable                  // repaired by `scribe lint --fix`
	errKindNeedsTitle               // author must supply a title
	errKindNeedsConfidence          // author must pick high/medium/low
	errKindNeedsFrontmatter         // page has no valid `---` block
	errKindOther                    // anything else — no canned remedy
)

// classifyFrontmatterError maps a validateFile message to its remediation
// bucket. Keep in sync with autoFixArticle: a class is only errKindFixable
// if the fixer actually repairs it deterministically.
func classifyFrontmatterError(msg string) errKind {
	switch {
	case strings.Contains(msg, "no frontmatter delimiter"),
		strings.Contains(msg, "no closing frontmatter delimiter"),
		strings.Contains(msg, "invalid YAML frontmatter"),
		strings.Contains(msg, "fails struct validation"),
		strings.Contains(msg, "empty file"):
		return errKindNeedsFrontmatter
	case strings.Contains(msg, "missing required fields"):
		// title is the one required field --fix won't invent; everything
		// else (domain/dates/list defaults/type) is backfilled.
		if strings.Contains(msg, "title") {
			return errKindNeedsTitle
		}
		return errKindFixable
	case strings.Contains(msg, "title is empty"):
		return errKindNeedsTitle
	case strings.Contains(msg, "invalid confidence"):
		return errKindNeedsConfidence
	case strings.Contains(msg, "invalid domain"),
		strings.Contains(msg, "invalid type"),
		strings.Contains(msg, "should be a list"),
		strings.Contains(msg, "not in YYYY-MM-DD format"):
		return errKindFixable
	default:
		return errKindOther
	}
}

// noteErrorKind records the remediation bucket for one frontmatter error.
func (r *lintReport) noteErrorKind(msg string) {
	r.errKinds[classifyFrontmatterError(msg)]++
}

// errorHint prints a closing how-to-fix block tailored to the errors seen
// this run: the `--fix` line when any mechanical error is present, plus a
// per-class bullet for each residual that needs a human. No-op in quiet
// mode or when no Phase-1 errors were classified.
func (r *lintReport) errorHint() {
	if r.quiet || len(r.errKinds) == 0 {
		return
	}
	if r.errKinds[errKindFixable] > 0 {
		fmt.Fprintln(r.w, "→ some errors are mechanical — run `scribe lint --fix`")
		fmt.Fprintln(r.w, "  (repairs duplicate keys, list formatting, dates, invalid domains, and missing defaults)")
	}
	var manual []string
	if r.errKinds[errKindNeedsTitle] > 0 {
		manual = append(manual, "missing title — add a `title:` line (--fix won't invent one)")
	}
	if r.errKinds[errKindNeedsConfidence] > 0 {
		manual = append(manual, "invalid confidence — set it to high, medium, or low")
	}
	if r.errKinds[errKindNeedsFrontmatter] > 0 {
		manual = append(manual, "no frontmatter — the page has no `---` block; add one")
	}
	if len(manual) > 0 {
		fmt.Fprintln(r.w, "→ these need a human:")
		for _, m := range manual {
			fmt.Fprintf(r.w, "    • %s\n", m)
		}
	}
}

// errorf prints a per-file ERROR line and counts one error. Errors are
// never grouped or suppressed.
func (r *lintReport) errorf(format string, args ...any) {
	r.errors++
	r.errorLinef(format, args...)
}

// errorLinef prints a per-file ERROR line without counting — for callers
// that count errors at a coarser grain (per file, not per finding).
func (r *lintReport) errorLinef(format string, args ...any) {
	fmt.Fprintf(r.w, "  ERROR "+format+"\n", args...)
}

// warnf records a per-file warning of the given class. Verbose mode
// prints it immediately, with the class's remediation hint appended when
// lintHints knows one; default mode counts it for the grouped flush;
// quiet mode only counts.
func (r *lintReport) warnf(class, format string, args ...any) {
	r.warnings++
	if r.quiet {
		return
	}
	if r.verbose {
		msg := fmt.Sprintf(format, args...)
		if hint := lintHints[class]; hint != "" {
			msg += fmt.Sprintf(" (run `%s`)", hint)
		}
		fmt.Fprintf(r.w, "  WARN %s\n", msg)
		return
	}
	r.classCounts[class]++
}

// warnAggregatef records a warning that is already one line per class
// (orphan totals, index drift). It prints inline in default and verbose
// modes — there is nothing to group — and is suppressed in quiet mode.
func (r *lintReport) warnAggregatef(format string, args ...any) {
	r.warnings++
	if r.quiet {
		return
	}
	fmt.Fprintf(r.w, "  "+format+"\n", args...)
}

// infof prints a neutral informational line (suppressed in quiet mode).
func (r *lintReport) infof(format string, args ...any) {
	if r.quiet {
		return
	}
	fmt.Fprintf(r.w, "  "+format+"\n", args...)
}

// flush renders the grouped warning summary in default mode: one line
// per class, sorted by count descending (ties alphabetical), counts
// right-aligned and hints column-aligned:
//
//	412× index_tier missing          (run: scribe tier write --missing-only)
//	 23× thin article
//
// No-op in verbose mode (warnings already printed per-file), quiet mode,
// or when no per-file warnings were recorded.
func (r *lintReport) flush() {
	if r.verbose || r.quiet || len(r.classCounts) == 0 {
		return
	}

	classes := make([]string, 0, len(r.classCounts))
	for c := range r.classCounts {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool {
		ci, cj := r.classCounts[classes[i]], r.classCounts[classes[j]]
		if ci != cj {
			return ci > cj
		}
		return classes[i] < classes[j]
	})

	countW := len(strconv.Itoa(r.classCounts[classes[0]]))
	nameW := 0
	for _, c := range classes {
		if len(c) > nameW {
			nameW = len(c)
		}
	}

	fmt.Fprintln(r.w)
	for _, c := range classes {
		if hint := lintHints[c]; hint != "" {
			fmt.Fprintf(r.w, "%*d× %-*s (run: %s)\n", countW, r.classCounts[c], nameW, c, hint)
		} else {
			fmt.Fprintf(r.w, "%*d× %s\n", countW, r.classCounts[c], c)
		}
	}
}
