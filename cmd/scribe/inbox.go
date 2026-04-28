package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// drainFileInbox processes raw/inbox/* through the convert dispatcher.
// Each file becomes a raw article in raw/articles/ and the original
// moves to raw/inbox/.processed/ on success or raw/inbox/.failed/<slug>/
// on failure (with an err.log so the user can inspect without losing
// the source). Returns the number of successfully ingested files.
//
// Idempotency: a file already present in .processed/ won't reappear in
// the watch list because we move (not copy). A failed file stays in
// .failed/ until the user manually requeues — no infinite retry loops.
//
// Phase 1B: synchronous, single-file marker invocations. Phase 2 will
// add marker_server batching for cold-load amortization.
func drainFileInbox(root string) (int, error) {
	cfg := loadConfig(root)
	inboxRel := cfg.Ingest.InboxPath
	if inboxRel == "" {
		inboxRel = "raw/inbox"
	}
	inbox := filepath.Join(root, inboxRel)

	st, err := os.Stat(inbox)
	if err != nil {
		// No inbox directory means nothing to do — not an error.
		// (Users opt in by creating raw/inbox/ themselves or by
		// dropping the first file there, which mkdir-as-needed
		// handles in the user-facing `scribe ingest file` flow.)
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat inbox: %w", err)
	}
	if !st.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", inbox)
	}

	processedDir := filepath.Join(inbox, ".processed")
	failedDir := filepath.Join(inbox, ".failed")
	if err := os.MkdirAll(processedDir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir .processed: %w", err)
	}

	entries, err := os.ReadDir(inbox)
	if err != nil {
		return 0, fmt.Errorf("read inbox: %w", err)
	}

	processed := 0
	for _, entry := range entries {
		// Skip our own state dirs and any dotfiles (.DS_Store, partial
		// downloads, etc.).
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		// Skip files still being written. Heuristic: a stat'd size of 0
		// in a non-empty filesystem is almost always a placeholder.
		full := filepath.Join(inbox, name)
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Size() == 0 {
			continue
		}

		err = ingestInboxFile(root, full, processedDir)
		if err != nil {
			logMsg("inbox", "failed: %s: %v", name, err)
			quarantineFailedFile(failedDir, full, err)
			continue
		}
		processed++
		logMsg("inbox", "ingested: %s", name)
	}
	return processed, nil
}

// ingestInboxFile converts and writes a single inbox file, then moves
// the original into the .processed/ dir. Errors propagate to the caller
// so quarantine can run.
func ingestInboxFile(root, full, processedDir string) error {
	data, err := os.ReadFile(full) //nolint:gosec // user-supplied source path; reading is the point
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(full))

	title, body := normalizeForAbsorbWithPath(full, ext, string(data), "")
	if body == "" {
		return fmt.Errorf("convert: empty result for %s", filepath.Base(full))
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(full), ext)
	}

	sourceURL := "file:///" + filepath.Base(full)
	rawPath, content := buildRawArticle(root, sourceURL, title, body, "inbox", "general", nil)
	if err := os.MkdirAll(filepath.Dir(rawPath), 0o755); err != nil {
		return fmt.Errorf("mkdir raw/articles: %w", err)
	}
	if err := os.WriteFile(rawPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write article: %w", err)
	}

	// Move original to .processed/ — uses time-stamped name to avoid
	// collisions when the same filename gets re-dropped later.
	stamp := time.Now().UTC().Format("20060102-150405")
	dest := filepath.Join(processedDir, stamp+"-"+filepath.Base(full))
	if err := os.Rename(full, dest); err != nil {
		return fmt.Errorf("archive original: %w", err)
	}
	return nil
}

// quarantineFailedFile moves a problematic source into .failed/<slug>/
// with an err.log capturing the conversion error. The user can inspect,
// fix marker config (or install marker), and move the file back to the
// inbox root to retry.
func quarantineFailedFile(failedRoot, full string, convErr error) {
	if err := os.MkdirAll(failedRoot, 0o755); err != nil {
		logMsg("inbox", "could not create failed dir: %v", err)
		return
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	stem := strings.TrimSuffix(filepath.Base(full), filepath.Ext(full))
	subdir := filepath.Join(failedRoot, stamp+"-"+stem)
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		logMsg("inbox", "could not create failed subdir: %v", err)
		return
	}
	dest := filepath.Join(subdir, filepath.Base(full))
	if err := os.Rename(full, dest); err != nil {
		logMsg("inbox", "could not quarantine %s: %v", filepath.Base(full), err)
		return
	}
	logPath := filepath.Join(subdir, "err.log")
	logBody := fmt.Sprintf("file:        %s\ntimestamp:   %s\nerror:       %v\n", filepath.Base(full), time.Now().UTC().Format(time.RFC3339), convErr)
	_ = os.WriteFile(logPath, []byte(logBody), 0o644)
}
