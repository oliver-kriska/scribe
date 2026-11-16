package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// gitSHA returns the current HEAD SHA for a git repo.
func gitSHA(repoPath string) string {
	return runCmd(repoPath, "git", "rev-parse", "HEAD")
}

// gitBranch returns the current branch name.
func gitBranch(repoPath string) string {
	return runCmd(repoPath, "git", "branch", "--show-current")
}

// gitRemoteURL returns the origin remote URL, or empty string.
func gitRemoteURL(repoPath string) string {
	return runCmd(repoPath, "git", "remote", "get-url", "origin")
}

// gitSHARE is the only shape a manifest last_sha may take. Abbreviated
// (7+) and full SHA-1/SHA-256 names pass; option-looking strings do not.
var gitSHARE = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// gitChangedFiles returns files changed between two SHAs (or all files if oldSHA is empty).
func gitChangedFiles(repoPath, oldSHA string, patterns []string) []string {
	if oldSHA == "" {
		// Never synced: list every matching file. Ask git, not the
		// filesystem — findFiles skips only a fixed dir list and knows
		// nothing about .gitignore, so a gitignored dependency tree
		// (`.venv/**/site-packages` is full of *.md and *.txt) inflates
		// the count until it trips sync.max_extract_files and the
		// project is skipped on every run forever. See issue #86.
		// The incremental path below never had this problem: `git diff`
		// only ever reports tracked files.
		if files, ok := gitListFiles(repoPath, patterns); ok {
			return excludeClaudeTooling(repoPath, files)
		}
		return excludeClaudeTooling(repoPath, findFiles(repoPath, patterns))
	}

	// oldSHA comes from scripts/projects.json and is placed BEFORE the
	// `--` separator, so anything that is not a hex object name
	// (`--output=...`, `-c core.pager=...`) would be parsed by git as an
	// option. Refuse the diff and fall back to the full listing instead.
	if !gitSHARE.MatchString(oldSHA) {
		logMsg("git", "ignoring malformed last_sha for %s — listing all files", repoPath)
		return gitChangedFiles(repoPath, "", patterns)
	}
	args := make([]string, 0, 7+len(patterns))
	args = append(args, "-C", repoPath, "diff", "--name-only", oldSHA, "HEAD", "--")
	args = append(args, patterns...)
	out := runCmd("", "git", args...)
	if out == "" {
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, filepath.Join(repoPath, line))
		}
	}
	return excludeClaudeTooling(repoPath, files)
}

// gitListFiles lists the files git would consider part of repoPath —
// tracked plus untracked-but-not-ignored — filtered by pathspec. It is the
// .gitignore-aware counterpart to findFiles, used for a project's first
// extraction where there is no old SHA to diff against.
//
// Returns ok=false when repoPath is not inside a git work tree, or git
// fails, so the caller can fall back to a plain filesystem walk. An empty
// result with ok=true is meaningful: the repo genuinely has no matching
// files.
func gitListFiles(repoPath string, patterns []string) ([]string, bool) {
	if !insideGitWorkTree(repoPath) {
		return nil, false
	}

	args := make([]string, 0, 8+len(patterns))
	args = append(args, "-C", repoPath, "ls-files",
		"--cached", "--others", "--exclude-standard", "-z", "--")
	args = append(args, patterns...)

	// -z because git quotes unusual filenames on the default line-based
	// output; NUL separation keeps them byte-exact.
	out, err := runCmdRaw("", "git", args...)
	if err != nil {
		// The work-tree probe said yes, so this is a real git failure,
		// not a plain directory — dubious-ownership under cron, a
		// corrupt index. The
		// caller silently falls back to the filesystem walk, which
		// reintroduces the #86 overcount; say so, or it is undiagnosable.
		logMsg("git", "git ls-files failed in %s (%v) — falling back to a filesystem walk, which ignores .gitignore", repoPath, err)
		return nil, false
	}

	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		files = append(files, filepath.Join(repoPath, name))
	}
	return files, true
}

// gitShowBytes reads a blob by revision spec (`:path` = index,
// `:2:path`/`:3:path` = merge stages, `<sha>:path` = a commit),
// byte-exact.
func gitShowBytes(repoPath, spec string) ([]byte, error) {
	return runCmdRaw(repoPath, "git", "show", spec)
}

// gitIsDirty returns true if the working tree has changes.
func gitIsDirty(repoPath string) bool {
	out := runCmd(repoPath, "git", "status", "--porcelain")
	return out != ""
}

// pullRebase runs `git pull --rebase --autostash` in repoPath. Returns
// (ok, pulled_anything, err). Non-fatal by design for sync's preflight:
// if the KB isn't a git repo, has no remote, or the pull fails (offline,
// auth, conflict), we log and let the caller continue. "pulled_anything"
// is a cheap HEAD-changed check so callers can decide whether a reindex
// is warranted.
func pullRebase(repoPath string) (ok bool, pulled bool, err error) {
	if !hasGit(repoPath) {
		return false, false, nil
	}
	if gitRemoteURL(repoPath) == "" {
		return false, false, nil
	}
	if dErr := ensureMergeDriver(repoPath); dErr != nil {
		logMsg("git", "merge driver not configured (git falls back to its built-in merge): %v", dErr)
	}
	// One resolver round per replayed commit: count local commits before
	// the pull rewrites HEAD.
	maxRounds := max(minRebaseRounds, localAheadCount(repoPath)+1)
	beforeSHA := gitSHA(repoPath)
	// commit.gpgsign=false: the rebase re-commits every replayed commit,
	// and a scheduled job cannot reach a signing agent (2026-11-07).
	cmd := exec.Command("git", "-c", "commit.gpgsign=false", "pull", "--rebase", "--autostash") //nolint:noctx // git subprocess, network-bound
	cmd.Dir = repoPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	relayMergeDriverLines(stderr.String())
	// Conflicts the merge driver did not settle — derived files, team
	// coordination files, delete/modify, autostash leftovers — are
	// resolved here without a human. Any other conflict aborts the
	// rebase, so a cron-driven sync never leaves the repo mid-rebase with
	// conflict markers on disk. This also runs after a successful pull:
	// --autostash re-applies local edits after the rebase and can
	// conflict with no rebase in progress.
	resolved, rErr := autoResolveDerivedConflicts(repoPath, maxRounds)
	invalidateRepoTrackedCache()
	if runErr != nil && !resolved {
		msg := firstLine(withoutMergeDriverLines(stderr.String()))
		if msg == "" {
			msg = runErr.Error()
		}
		if rErr != nil {
			return false, false, fmt.Errorf("%s; %w", msg, rErr)
		}
		return false, false, fmt.Errorf("%s", msg)
	}
	if rErr != nil {
		return false, false, rErr
	}
	if resolved {
		logMsg("git", "pull conflict auto-resolved (derived, coordination or delete/modify files)")
	}
	afterSHA := gitSHA(repoPath)
	return true, beforeSHA != "" && afterSHA != "" && beforeSHA != afterSHA, nil
}

// minRebaseRounds is the resolver's round cap when few commits replay.
const minRebaseRounds = 20

// localAheadCount counts local commits the upstream does not have (0 when
// there is no upstream).
func localAheadCount(repoPath string) int {
	n, err := strconv.Atoi(runCmd(repoPath, "git", "rev-list", "--count", "@{u}..HEAD"))
	if err != nil {
		return 0
	}
	return n
}

// withoutMergeDriverLines drops the driver's per-file lines from git
// output, so an error message shows git's own reason.
func withoutMergeDriverLines(out string) string {
	var kept []string
	for line := range strings.SplitSeq(strings.ReplaceAll(out, "\r", "\n"), "\n") {
		if !strings.HasPrefix(line, mergeDriverLogPrefix) && strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// rebaseInProgress reports whether repoPath has a rebase mid-flight.
func rebaseInProgress(repoPath string) bool {
	for _, sub := range []string{"rebase-merge", "rebase-apply"} {
		p := runCmd(repoPath, "git", "rev-parse", "--git-path", sub)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(repoPath, p)
		}
		if dirExists(p) {
			return true
		}
	}
	return false
}

// conflictedFiles returns the repo-relative paths currently unmerged.
func conflictedFiles(repoPath string) []string {
	out := runCmd(repoPath, "git", "diff", "--name-only", "--diff-filter=U")
	if out == "" {
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			files = append(files, l)
		}
	}
	return files
}

// autoResolveDerivedConflicts finishes a conflicted rebase when every
// conflicted file is derived/regenerable (either side wins), has a
// merger in specialKBFiles (sides are merged — see gitmerge.go), or is a
// delete/modify conflict (the modified side is kept). It returns true
// once the conflicts are gone. Any other conflict — or failure to
// converge within maxRounds — aborts the rebase so the working tree is
// restored; the error then names every file a human must merge.
//
// With no rebase in progress it resolves conflicts that --autostash left
// when it re-applied local edits after the pull.
func autoResolveDerivedConflicts(repoPath string, maxRounds int) (bool, error) {
	if !rebaseInProgress(repoPath) {
		conflicted := conflictedFiles(repoPath)
		if len(conflicted) == 0 {
			return false, nil
		}
		if err := resolveConflictedFiles(repoPath, conflicted); err != nil {
			return false, fmt.Errorf("autostash: %w (local edits are kept in the stash: git stash list)", err)
		}
		return true, nil
	}
	for round := 0; round < maxRounds && rebaseInProgress(repoPath); round++ {
		if err := resolveConflictedFiles(repoPath, conflictedFiles(repoPath)); err != nil {
			_, _ = runCmdErr(repoPath, "git", "rebase", "--abort")
			return false, fmt.Errorf("%w (rebase aborted, working tree restored)", err)
		}
		// A resolution identical to the upstream side empties the
		// replayed commit; `rebase --continue` refuses an empty commit,
		// so skip it instead (the remote already has the content).
		if !gitHasStagedChanges(repoPath) && len(conflictedFiles(repoPath)) == 0 {
			out, err := runCmdErr(repoPath, "git", "-c", "commit.gpgsign=false", "rebase", "--skip")
			relayMergeDriverLines(out)
			if err != nil {
				if rebaseInProgress(repoPath) {
					continue
				}
				return false, fmt.Errorf("rebase --skip failed: %w", err)
			}
			continue
		}
		out, err := runCmdErr(repoPath, "git", "-c", "core.editor=true", "-c", "commit.gpgsign=false", "rebase", "--continue")
		relayMergeDriverLines(out)
		if err != nil {
			if rebaseInProgress(repoPath) {
				continue // stopped on the next commit's conflicts — next round
			}
			return false, fmt.Errorf("rebase --continue failed: %w", err)
		}
	}
	if rebaseInProgress(repoPath) {
		_, _ = runCmdErr(repoPath, "git", "rebase", "--abort")
		return false, fmt.Errorf("rebase did not converge in %d rounds while auto-resolving (aborted)", maxRounds)
	}
	return true, nil
}

// resolveConflictedFiles resolves every file or none: when any file needs
// a human, nothing is touched and the error names all of them.
func resolveConflictedFiles(repoPath string, files []string) error {
	var manual []string
	for _, f := range files {
		if _, ok := conflictResolution(repoPath, f); !ok {
			manual = append(manual, f)
		}
	}
	if len(manual) > 0 {
		return fmt.Errorf("conflict in %s needs manual resolution", strings.Join(manual, ", "))
	}
	for _, f := range files {
		if err := resolveConflictedFile(repoPath, f); err != nil {
			return err
		}
	}
	return nil
}

// conflictResolve names how resolveConflictedFile settles a file.
type conflictResolve int

const (
	resolveKeepModified conflictResolve = iota
	resolveMerge
	resolveEitherSide
)

// conflictResolution picks the rule for one conflicted file; ok is false
// when only a human can merge it.
func conflictResolution(repoPath, rel string) (conflictResolve, bool) {
	ours, theirs := conflictSidesPresent(repoPath, rel)
	switch {
	case ours != theirs:
		return resolveKeepModified, true
	case mergeFuncFor(rel) != nil:
		return resolveMerge, true
	case derivedRegenerable[rel]:
		return resolveEitherSide, true
	}
	return 0, false
}

// conflictSidesPresent reports which index stages hold rel: stage 2
// (ours — upstream during a rebase) and stage 3 (theirs — the local commit).
func conflictSidesPresent(repoPath, rel string) (ours, theirs bool) {
	out := runCmd(repoPath, "git", "ls-files", "-u", "--", rel)
	for line := range strings.SplitSeq(out, "\n") {
		meta, _, _ := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if len(fields) == 3 {
			ours = ours || fields[2] == "2"
			theirs = theirs || fields[2] == "3"
		}
	}
	return ours, theirs
}

// resolveConflictedFile settles and stages one file by conflictResolution.
func resolveConflictedFile(repoPath, rel string) error {
	rule, _ := conflictResolution(repoPath, rel)
	switch rule {
	case resolveKeepModified:
		// The driver is not called for delete/modify. Keep the edit: a
		// deletion loses nothing that the kept file does not still hold.
		side := "--theirs"
		if ours, _ := conflictSidesPresent(repoPath, rel); ours {
			side = "--ours"
		}
		if out, err := runCmdErr(repoPath, "git", "checkout", side, "--", rel); err != nil {
			return fmt.Errorf("keeping the modified side of %s failed: %s", rel, out)
		}
	case resolveMerge:
		if !semanticResolve(repoPath, rel) {
			return fmt.Errorf("semantic merge of %s failed", rel)
		}
		return nil
	case resolveEitherSide:
		// Either side works — the file regenerates right after the pull.
		// --theirs picks the local-commit version during a rebase.
		if out, err := runCmdErr(repoPath, "git", "checkout", "--theirs", "--", rel); err != nil {
			return fmt.Errorf("checking out %s failed: %s", rel, out)
		}
	}
	if out, err := runCmdErr(repoPath, "git", "add", "--", rel); err != nil {
		return fmt.Errorf("staging %s during auto-resolve failed: %s", rel, out)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// commitDebounced returns true when the configured commit debounce window
// has not yet elapsed since the last commit on HEAD. Callers should skip
// committing (and pushing) in that case; the staged/unstaged changes stay
// in the working tree for the next run to pick up and roll into a single
// larger commit. Zero or negative CommitDebounceMinutes disables the
// check entirely (existing behavior).
func commitDebounced(repoPath string, cfg *ScribeConfig) (bool, time.Duration, time.Duration) {
	minutes := 0
	if cfg != nil {
		minutes = cfg.Sync.CommitDebounceMinutes
	}
	if minutes <= 0 {
		return false, 0, 0
	}
	window := time.Duration(minutes) * time.Minute
	age := gitLastCommitAge(repoPath)
	return age < window, age, window
}

// gitLastCommitAge returns the time elapsed since the last commit on HEAD.
// Returns a very large duration if there is no HEAD yet or git can't be
// queried — so callers comparing with a debounce window treat "unknown"
// as "old enough to commit now".
func gitLastCommitAge(repoPath string) time.Duration {
	out := runCmd(repoPath, "git", "log", "-1", "--format=%ct")
	if out == "" {
		return 365 * 24 * time.Hour
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 365 * 24 * time.Hour
	}
	return time.Since(time.Unix(secs, 0))
}

// gitHasStagedChanges returns true if there are staged changes ready to commit.
// Uses `git diff --cached --quiet`: exit 0 = no changes, exit 1 = changes.
func gitHasStagedChanges(repoPath string) bool {
	cmd := exec.Command("git", "diff", "--cached", "--quiet") //nolint:noctx // quick status probe
	cmd.Dir = repoPath
	err := cmd.Run()
	// Exit 1 means there ARE staged changes; any other error means git itself failed.
	exitErr := &exec.ExitError{}
	return errors.As(err, &exitErr)
}

// gitAddWiki stages wiki content directories plus raw/ (see
// stagedContentDirs). Before staging, new articles get a `contributor:`
// frontmatter stamp — this funnel is shared by every commit path, so
// attribution lands regardless of which writer (envelope executor or
// tool-mode model) created the file.
//
// Returns false when the secret gate detected a credential it could
// not hold back — the staged set is then unsafe and callers must skip
// the commit (staged changes roll to the next run).
func gitAddWiki(root string) bool {
	stampContributor(root)
	// A pathspec that matches nothing makes the whole `git add` fatal
	// (nothing gets staged), so only paths that exist on disk join the
	// command — a KB missing a content dir or the extraction ledger
	// (absent until the first post-upgrade extraction) must not block
	// staging everything else.
	dirs := stagedContentDirs()
	args := make([]string, 0, 1+len(dirs)+3)
	args = append(args, "add")
	for _, d := range dirs {
		if dirExists(filepath.Join(root, d)) {
			args = append(args, d)
		}
	}
	for _, f := range autoStagedSpecialFiles() {
		if fileExists(filepath.Join(root, f)) {
			args = append(args, f)
		}
	}
	if len(args) == 1 {
		return true
	}
	cmd := exec.Command("git", args...) //nolint:noctx // git add subprocess
	cmd.Dir = root
	_ = cmd.Run()
	unstageConflictMarkers(root)
	// Commit gate: anything staged above that contains a real-shaped
	// secret (team mode) or a configured held word is unstaged again,
	// loudly; masked words are redacted in place.
	return commitGate(root, loadConfig(root))
}

// unstageConflictMarkers takes back out of the index every staged file
// that holds git conflict markers, so a half-finished merge (an autostash
// leftover, a manual merge) never reaches a commit and the team remote.
func unstageConflictMarkers(root string) {
	out, _ := runCmdRaw(root, "git", "diff", "--cached", "--name-only", "-z", "--diff-filter=AM")
	for rel := range strings.SplitSeq(string(out), "\x00") {
		if rel == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || firstConflictMarkerLine(data) == 0 {
			continue
		}
		if _, err := runCmdErr(root, "git", "reset", "-q", "--", rel); err == nil {
			logMsg("git", "not staging %s: it contains git conflict markers (resolve by hand, see `scribe doctor`)", rel)
		}
	}
}

// gitCommit creates a commit with the given message. Captures stderr so callers
// see the real reason (pre-commit hook failure, signing issue, nothing staged).
func gitCommit(root, message string) error {
	cmd := exec.Command("git", "commit", "-m", message, "--no-gpg-sign") //nolint:noctx // git commit subprocess
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("git commit: %w", err)
		}
		// Collapse multi-line stderr to first non-empty line to keep sync log tidy.
		first := msg
		if idx := strings.Index(msg, "\n"); idx > 0 {
			first = strings.TrimSpace(msg[:idx])
		}
		return fmt.Errorf("git commit: %s", first)
	}
	return nil
}

// gitDiffShortstat returns (files_changed, lines_added, lines_deleted) for a
// commit range — e.g. gitDiffShortstat(root, "HEAD~1", "HEAD"). Returns zeros
// on any git error (never fails the caller — this is used for instrumentation).
func gitDiffShortstat(repoPath, from, to string) (files, added, deleted int) {
	cmd := exec.Command("git", "diff", "--shortstat", from, to) //nolint:noctx // git diff subprocess
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, 0
	}
	// Example: " 3 files changed, 45 insertions(+), 2 deletions(-)"
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0, 0, 0
	}
	for part := range strings.SplitSeq(line, ",") {
		part = strings.TrimSpace(part)
		var n int
		if _, err := fmt.Sscanf(part, "%d ", &n); err != nil {
			continue
		}
		switch {
		case strings.Contains(part, "file"):
			files = n
		case strings.Contains(part, "insertion"):
			added = n
		case strings.Contains(part, "deletion"):
			deleted = n
		}
	}
	return files, added, deleted
}

// gitPush pushes to origin. On a non-fast-forward failure (remote advanced
// while we were committing locally) we try a `pull --rebase` and push again
// once — that handles the common case where an earlier scribe run pushed
// while another was still extracting. A force-push is never attempted: KB
// history is append-only and losing remote commits would be worse than a
// failed push we can reconcile manually.
func gitPush(root string) error {
	out, err := runGitPush(root)
	if err == nil {
		return nil
	}
	// One push, output captured: the old code streamed the first push to
	// the terminal and then ran a SECOND push just to read its message,
	// so every failure hit the remote twice and the reason was only on
	// screen, never in the log doctor reads.
	logMsg("git", "push failed: %s", strings.TrimSpace(tailLines(out, 5)))
	if !isNonFastForward(out) {
		return err
	}
	logMsg("git", "push rejected (non-fast-forward); attempting `git pull --rebase`")
	// Route through pullRebase, not a raw `git pull --rebase`: it
	// auto-resolves derived/coordination-file conflicts and ABORTS any
	// other conflicted rebase. The raw command used to return non-zero
	// and leave the repo mid-rebase with markers on disk for hours —
	// the exact state the conflict machinery exists to prevent.
	if ok, _, rerr := pullRebase(root); !ok || rerr != nil {
		logMsg("git", "pull --rebase failed: %v — resolve manually then push", rerr)
		return err
	}
	out, err = runGitPush(root)
	if err != nil {
		logMsg("git", "push after rebase failed: %s", strings.TrimSpace(tailLines(out, 5)))
	}
	return err
}

// runGitPush runs `git push` and returns its combined output with the
// error, so callers log what git said instead of only that it failed.
func runGitPush(root string) (string, error) {
	cmd := exec.Command("git", "push") //nolint:noctx // git push subprocess
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// isNonFastForward recognizes the two strings git emits for a rejected push
// that needs a rebase/merge first. Keeping the detection string-level means
// we don't need to parse exit codes (exec.ExitError doesn't distinguish
// non-fast-forward from, say, auth failure).
func isNonFastForward(stderr string) bool {
	return strings.Contains(stderr, "non-fast-forward") ||
		strings.Contains(stderr, "rejected") && strings.Contains(stderr, "fetch first") ||
		strings.Contains(stderr, "Updates were rejected because the tip of your current branch is behind")
}

// findFiles finds files matching patterns in a directory, excluding build artifacts.
func findFiles(root string, patterns []string) []string {
	excludeDirs := map[string]bool{
		"_build": true, "deps": true, "node_modules": true,
		".git": true, ".elixir_ls": true, "temp": true,
	}

	var files []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		if info.IsDir() {
			if excludeDirs[info.Name()] || nestedCheckout(root, path) || isClaudeToolingDir(root, path) {
				return filepath.SkipDir
			}
			return nil
		}
		for _, pattern := range patterns {
			if matched, _ := filepath.Match(pattern, info.Name()); matched {
				files = append(files, path)
				break
			}
		}
		return nil
	})
	return files
}

// hasGit returns true if path contains a .git directory.
func hasGit(path string) bool {
	return dirExists(filepath.Join(path, ".git"))
}

// insideGitWorkTree reports whether git treats path as part of a work
// tree. Deliberately broader than hasGit, which stats for a .git
// *directory*: a linked worktree's .git is a regular file and a
// subdirectory of a checkout has no .git at all, yet git lists both
// correctly. Worktree paths do reach extraction — discovery folds them
// into the main checkout now, but scripts/projects.json still carries
// entries enrolled before it did — and a false negative there is not
// harmless: it silently reinstates the issue #86 filesystem walk that
// counts gitignored dependency trees.
func insideGitWorkTree(path string) bool {
	return runCmd(path, "git", "rev-parse", "--is-inside-work-tree") == "true"
}
