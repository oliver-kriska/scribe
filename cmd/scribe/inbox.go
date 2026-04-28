package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// drainFileInbox processes raw/inbox/* through the convert dispatcher.
// Each file becomes a raw article in raw/articles/ and the original
// moves to raw/inbox/.processed/ on success or raw/inbox/.failed/<slug>/
// on failure (with an err.log so the user can inspect without losing
// the source). Returns the number of successfully ingested files.
//
// Idempotency story (Phase 2B):
//   - move-to-.processed/ on success means a re-run of sync skips the
//     same physical file (it isn't there anymore).
//   - .inbox-state.json keeps a sha256 → entry index, so a *new* drop
//     of the same content (different filename, re-downloaded copy,
//     etc.) skips marker entirely. Without this, dropping report.pdf
//     a second time costs 3 minutes of model load for a duplicate
//     article.
//
// Phase 1B: synchronous, single-file marker invocations. Phase 3 will
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

	state := loadInboxState(inbox)

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

		// Content hash skip-list: if we've already ingested a file
		// with this exact content, archive the duplicate without
		// re-running marker. The hash is cheap relative to a 3 GB
		// model load.
		hash, hashErr := sha256File(full)
		if hashErr == nil {
			if prev, hit := state.lookup(hash); hit {
				if err := archiveDuplicate(full, processedDir); err != nil {
					logMsg("inbox", "duplicate archive failed for %s: %v", name, err)
				} else {
					logMsg("inbox", "skip duplicate %s (already ingested as %s)", name, prev.RawPath)
				}
				continue
			}
		}

		raw, err := ingestInboxFile(root, full, processedDir)
		if err != nil {
			logMsg("inbox", "failed: %s: %v", name, err)
			quarantineFailedFile(failedDir, full, err)
			continue
		}
		if hash != "" {
			state.record(hash, inboxEntry{
				Filename:    name,
				RawPath:     raw,
				ProcessedAt: time.Now().UTC().Format(time.RFC3339),
			})
		}
		processed++
		logMsg("inbox", "ingested: %s", name)
	}
	if err := state.persist(inbox); err != nil {
		logMsg("inbox", "could not persist state: %v", err)
	}
	return processed, nil
}

// ingestInboxFile converts and writes a single inbox file, then moves
// the original into the .processed/ dir. Returns the raw article path
// on success so callers can record it in the idempotency state file.
// Errors propagate so quarantine can run.
func ingestInboxFile(root, full, processedDir string) (string, error) {
	data, err := os.ReadFile(full) //nolint:gosec // user-supplied source path; reading is the point
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(full))

	// Direct dispatch for non-text formats so we can surface marker's
	// MarkerStats in the article frontmatter. Plain text/markdown still
	// flows through normalizeForAbsorbWithPath via convertFile's
	// passthrough nil result.
	res, err := convertFile(full, ext, data, "")
	if err != nil {
		return "", fmt.Errorf("convert: %w", err)
	}
	var (
		title string
		body  string
		stats *MarkerStats
	)
	if res == nil {
		// Plain-text passthrough — fall back to legacy normalize so
		// .md/.txt continue to behave the same.
		title, body = normalizeForAbsorbWithPath(full, ext, string(data), "")
	} else {
		title = res.Title
		body = res.Markdown
		stats = res.Stats
	}
	if body == "" {
		return "", fmt.Errorf("convert: empty result for %s", filepath.Base(full))
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(full), ext)
	}

	sourceURL := "file:///" + filepath.Base(full)
	rawPath, content := buildRawArticleWithStats(root, sourceURL, title, body, "inbox", "general", nil, stats)
	if err := os.MkdirAll(filepath.Dir(rawPath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir raw/articles: %w", err)
	}
	if err := os.WriteFile(rawPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write article: %w", err)
	}

	// Phase 3A: persist the chapter outline alongside the article so
	// chapter-aware absorb (3A.5) can splice the body on real
	// boundaries instead of fixed-token windows. Best-effort — a
	// sidecar miss never blocks ingestion.
	if err := writeTOCSidecar(rawPath, filepath.Base(full), stats); err != nil {
		logMsg("inbox", "toc sidecar warning for %s: %v", filepath.Base(rawPath), err)
	}

	// Move original to .processed/ — uses time-stamped name to avoid
	// collisions when the same filename gets re-dropped later.
	stamp := time.Now().UTC().Format("20060102-150405")
	dest := filepath.Join(processedDir, stamp+"-"+filepath.Base(full))
	if err := os.Rename(full, dest); err != nil {
		return "", fmt.Errorf("archive original: %w", err)
	}
	relRaw, relErr := filepath.Rel(root, rawPath)
	if relErr != nil {
		relRaw = rawPath
	}
	return relRaw, nil
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

// inboxStateFilename is the name of the JSON file we keep at the
// inbox root for content-hash deduplication. It lives next to
// .processed/ and .failed/ so it travels with the inbox if the user
// moves the directory.
const inboxStateFilename = ".inbox-state.json"

type inboxEntry struct {
	Filename    string `json:"filename"`
	RawPath     string `json:"raw_path"`     // KB-relative path to the raw article we wrote
	ProcessedAt string `json:"processed_at"` // RFC3339 UTC
}

// inboxState wraps a sha256 → entry map with persistence and a small
// mutex. The map is small (one entry per ingested file) so we keep
// it in-memory and rewrite on every drain cycle. Concurrent callers
// would race the write; the mutex is cheap insurance.
type inboxState struct {
	mu      sync.Mutex
	Version int                   `json:"version"`
	Entries map[string]inboxEntry `json:"entries"`
}

// loadInboxState reads .inbox-state.json from inboxDir. Always returns
// a usable struct — missing files / corrupt JSON yield an empty index
// rather than blocking the drain. We log neither path because the
// caller has more context than we do here.
func loadInboxState(inboxDir string) *inboxState {
	st := &inboxState{Version: 1, Entries: map[string]inboxEntry{}}
	data, err := os.ReadFile(filepath.Join(inboxDir, inboxStateFilename))
	if err != nil {
		return st
	}
	parsed := struct {
		Version int                   `json:"version"`
		Entries map[string]inboxEntry `json:"entries"`
	}{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return st
	}
	if parsed.Entries != nil {
		st.Entries = parsed.Entries
	}
	if parsed.Version > 0 {
		st.Version = parsed.Version
	}
	return st
}

func (s *inboxState) lookup(hash string) (inboxEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.Entries[hash]
	return e, ok
}

func (s *inboxState) record(hash string, e inboxEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Entries == nil {
		s.Entries = map[string]inboxEntry{}
	}
	s.Entries[hash] = e
}

// persist writes the state file atomically (write to .tmp, rename) so
// a crash mid-write doesn't leave a half-truncated index. Returns
// nil on a no-op (zero entries means nothing worth persisting and
// avoids creating empty noise files).
func (s *inboxState) persist(inboxDir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Entries) == 0 {
		return nil
	}
	if s.Version == 0 {
		s.Version = 1
	}
	data, err := json.MarshalIndent(struct {
		Version int                   `json:"version"`
		Entries map[string]inboxEntry `json:"entries"`
	}{Version: s.Version, Entries: s.Entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	dst := filepath.Join(inboxDir, inboxStateFilename)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state tmp: %w", err)
	}
	return os.Rename(tmp, dst)
}

// sha256File hashes the bytes of full into a lowercase hex digest.
// Streams from disk so we don't double the memory footprint of large
// PDFs. Returns "" + nil-error semantics by reading; callers tolerate
// any read error by treating the file as un-deduplicated.
func sha256File(full string) (string, error) {
	f, err := os.Open(full) //nolint:gosec // user-supplied source path; reading is the point
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// archiveDuplicate moves a re-dropped file into .processed/ alongside
// the original ingestion artifact, prefixed `dup-<stamp>-` so the
// audit trail makes it obvious this copy was deduplicated rather than
// freshly processed.
func archiveDuplicate(full, processedDir string) error {
	stamp := time.Now().UTC().Format("20060102-150405")
	dest := filepath.Join(processedDir, "dup-"+stamp+"-"+filepath.Base(full))
	return os.Rename(full, dest)
}
