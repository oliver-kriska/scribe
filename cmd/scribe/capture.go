package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// CaptureCmd ports scripts/capture-imessage.sh: read iMessage self-chat from
// chat.db, extract URLs (and free-form notes), write raw/articles/ stubs, and
// optionally fetch URL content via fetch.go's three-tier pipeline.
//
// Requires Full Disk Access for the terminal running this command (macOS:
// System Settings → Privacy & Security → Full Disk Access).
type CaptureCmd struct {
	Since  string `help:"Capture from this date (YYYY-MM-DD). Defaults to last_capture from state file."`
	Fetch  bool   `help:"Also fetch URL content via fetch.go (FxTwitter / trafilatura / Jina)."`
	DryRun bool   `help:"Print what would be captured without writing." short:"n"`

	Refetch    bool `help:"Scan raw/articles/ for fetched_via: stub entries, retry fetching them. Parks failures in wiki/_unfetched-links.md. Skips the iMessage scan."`
	RefetchMax int  `help:"With --refetch, max stubs to process per run (0 = no limit)." default:"20"`
}

// captureState mirrors scripts/imessage-state.json. Field order/shape must
// stay compatible with the existing bash script during the transition.
type captureState struct {
	LastCapture   *string  `json:"last_capture"`
	CapturedURLs  []string `json:"captured_urls"`
	CapturedCount int      `json:"captured_count"`
}

// Apple's CFAbsoluteTime epoch is 2001-01-01 UTC.
const appleEpochOffset int64 = 978307200

// Default cutoff if state file has no last_capture (matches bash script).
const captureDefaultSince = "2026-03-01"

// URL extraction from text column. A Unicode-aware regex is fine here because
// text is a UTF-8 string with no binary content.
var captureURLTextRE = regexp.MustCompile(`https?://[^\s<>"')\]]+`)

// Skip list is loaded from cfg.Capture.SkipDomains — no hardcoded defaults.

func (c *CaptureCmd) Run() error {
	if c.Refetch {
		return (&CaptureRefetchCmd{Max: c.RefetchMax, DryRun: c.DryRun, Park: true}).Run()
	}
	root, err := kbDir()
	if err != nil {
		return err
	}
	cfg := loadConfig(root)

	if !c.DryRun {
		lockPath := lockPathFor(cfg.LockDir, "capture-imessage")
		lf, ok, err := acquireLock(lockPath)
		if err != nil {
			return fmt.Errorf("lock %s: %w", lockPath, err)
		}
		if !ok {
			logMsg("capture", "blocked by existing capture-imessage lock")
			return nil
		}
		defer releaseLock(lf)
	}

	statePath := filepath.Join(root, "scripts", "imessage-state.json")
	rawDir := filepath.Join(root, "raw", "articles")

	state, err := loadCaptureState(statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	since := c.Since
	if since == "" {
		if state.LastCapture != nil && *state.LastCapture != "" {
			since = *state.LastCapture
		} else {
			since = captureDefaultSince
		}
	}

	logMsg("capture", "capturing since %s", since)

	selfChatIDs := resolveSelfChatHandles(cfg.Capture)
	if len(selfChatIDs) == 0 {
		return errors.New("no self-chat handle configured\n\nSet one of:\n  scribe.yaml →\n    capture:\n      self_chat_handles:\n        - \"+1234567890\"\n        - \"you@icloud.com\"\n  or env:\n    SCRIBE_SELF_CHAT_ID=\"+1234567890,you@icloud.com\"\n\nList every iMessage address you use to message yourself — phone numbers and emails each map to a distinct chat in chat.db")
	}

	messages, err := readSelfChatMessages(selfChatIDs, since)
	if err != nil {
		return fmt.Errorf("read chat.db: %w", err)
	}

	captured := make(map[string]bool, len(state.CapturedURLs))
	for _, u := range state.CapturedURLs {
		captured[u] = true
	}

	newURLs, newNotes := sortCapturedMessages(messages, captured, cfg.Capture.SkipDomains)

	savedFiles := c.writeURLCaptures(rawDir, newURLs)
	savedFiles = append(savedFiles, c.writeNoteCaptures(rawDir, newNotes)...)

	logMsg("capture", "found %d URLs, %d notes, saved %d files", len(newURLs), len(newNotes), len(savedFiles))

	if c.DryRun {
		return nil
	}

	// Update state file when we captured anything.
	if len(newURLs) > 0 || len(newNotes) > 0 {
		today := time.Now().UTC().Format("2006-01-02")
		state.LastCapture = &today
		// captured map → sorted slice for stable diffs.
		urls := make([]string, 0, len(captured))
		for u := range captured {
			urls = append(urls, u)
		}
		sort.Strings(urls)
		state.CapturedURLs = urls
		state.CapturedCount += len(savedFiles)
		if err := saveCaptureState(statePath, state); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
	}

	logMsg("capture", "done")
	return nil
}

// capturedURL / capturedNote are the two kinds of self-chat content the
// scan yields: a shared link (→ raw article, optionally fetched) and a
// free-form text (→ idea note).
type capturedURL struct {
	URL        string
	Date       string
	Timestamp  time.Time
	SourceText string
}

type capturedNote struct {
	Text      string
	Date      string
	Timestamp time.Time
}

// messageURLs extracts shared links from one chat.db message — the text
// column first, then the attributedBody blob.
func messageURLs(m chatMessage) []string {
	var urls []string
	if m.Text != "" {
		urls = append(urls, captureURLTextRE.FindAllString(m.Text, -1)...)
	}

	// Fall back to attributedBody when text has no URLs (iMessage stores
	// shared link previews there as NSKeyedArchiver bytes). Go's regexp
	// package treats negated character classes as Unicode-aware, which lets
	// non-ASCII bytes through when matching against arbitrary binary data —
	// so we scan byte-by-byte instead of using regexp here.
	//
	// iMessage stores each shared URL in the blob at least twice: once as
	// the user-visible NSString and once wrapped in an NSURL NSKeyedArchive
	// entry that looks like "<url>WHttpURL<garbage byte>/<garbage>". The
	// byte scanner returns both, and the WHttpURL suffix is a reliable
	// sentinel we can truncate on to recover the clean URL.
	if len(urls) == 0 && len(m.AttributedBody) > 0 {
		for _, decoded := range extractURLsFromBytes(m.AttributedBody) {
			if idx := strings.Index(decoded, "WHttpURL"); idx >= 0 {
				decoded = decoded[:idx]
			}
			if decoded == "" {
				continue
			}
			urls = append(urls, decoded)
		}
	}
	return urls
}

// sortCapturedMessages splits self-chat messages into new URL captures
// and idea notes, marking each accepted URL in `captured` so dupes
// within and across runs are skipped.
func sortCapturedMessages(messages []chatMessage, captured map[string]bool, skipDomains []string) ([]capturedURL, []capturedNote) {
	var newURLs []capturedURL
	var newNotes []capturedNote

	for _, m := range messages {
		// Skip iMessage permission/control messages (matches bash filter).
		if m.Text != "" && (strings.Contains(m.Text, "Permission request") || strings.Contains(m.Text, "qnbsj")) {
			continue
		}

		urls := messageURLs(m)
		ts := appleNanosToTime(m.Date)
		dateStr := ts.UTC().Format("2006-01-02")

		if len(urls) > 0 {
			for _, u := range urls {
				u = strings.TrimRight(u, ".,;:!?)")
				if captured[u] {
					continue
				}
				if shouldSkipURL(u, skipDomains) {
					continue
				}
				captured[u] = true
				newURLs = append(newURLs, capturedURL{
					URL:        u,
					Date:       dateStr,
					Timestamp:  ts,
					SourceText: strings.TrimSpace(m.Text),
				})
			}
			continue
		}

		// Non-URL text → idea note.
		if m.Text != "" {
			clean := strings.TrimSpace(m.Text)
			if len(clean) > 10 && !strings.HasPrefix(clean, "Liked ") {
				newNotes = append(newNotes, capturedNote{
					Text:      clean,
					Date:      dateStr,
					Timestamp: ts,
				})
			}
		}
	}
	return newURLs, newNotes
}

// writeURLCaptures writes one raw article per captured URL (a stub, or
// fetched content with --fetch). Returns the filenames written.
func (c *CaptureCmd) writeURLCaptures(rawDir string, items []capturedURL) []string {
	var saved []string
	for _, item := range items {
		slug := slugFromCapturedURL(item.URL)
		fname := fmt.Sprintf("%s-imessage-%s.md", item.Date, slug)
		fpath := filepath.Join(rawDir, fname)

		if fileExists(fpath) {
			continue
		}

		if c.DryRun {
			fmt.Printf("  WOULD CAPTURE: %s -> %s\n", item.URL, fname)
			continue
		}

		title := slug
		body := fmt.Sprintf("# %s\n\nCaptured from iMessage self-chat on %s.\n\nOriginal message: %s\n",
			item.URL, item.Date, item.SourceText)
		via := "stub"

		if c.Fetch {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			res, ferr := fetchURL(ctx, item.URL, "auto")
			cancel()
			if ferr == nil && strings.TrimSpace(res.Body) != "" {
				if strings.TrimSpace(res.Title) != "" {
					title = res.Title
				}
				body = res.Body
				via = res.Via
			} else if ferr != nil {
				logMsg("capture", "  fetch failed: %s: %v", item.URL, ferr)
			}
		}

		content := buildCaptureArticle(item.URL, title, body, via, item.Date, []string{"imessage"})
		if err := writeCaptureFile(fpath, content); err != nil {
			logMsg("capture", "write failed: %s: %v", fname, err)
			continue
		}
		saved = append(saved, fname)
		if c.Fetch {
			logMsg("capture", "  fetched: %s (via %s)", fname, via)
			// Brief pause between fetches (matches bash behavior).
			time.Sleep(1 * time.Second)
		}
	}
	return saved
}

// writeNoteCaptures writes one idea-note article per captured text.
// Returns the filenames written.
func (c *CaptureCmd) writeNoteCaptures(rawDir string, notes []capturedNote) []string {
	var saved []string
	for _, note := range notes {
		slug := slugifyText(note.Text, 50)
		fname := fmt.Sprintf("%s-imessage-note-%s.md", note.Date, slug)
		fpath := filepath.Join(rawDir, fname)

		if fileExists(fpath) {
			continue
		}

		if c.DryRun {
			truncated := note.Text
			if len(truncated) > 80 {
				truncated = truncated[:80]
			}
			fmt.Printf("  WOULD CAPTURE NOTE: %s -> %s\n", truncated, fname)
			continue
		}

		title := note.Text
		if len(title) > 80 {
			title = title[:80]
		}
		body := fmt.Sprintf("%s\n\nCaptured from iMessage self-chat on %s.\n", note.Text, note.Date)
		content := buildCaptureArticle("", title, body, "imessage-note", note.Date, []string{"imessage", "note"})
		if err := writeCaptureFile(fpath, content); err != nil {
			logMsg("capture", "write failed: %s: %v", fname, err)
			continue
		}
		saved = append(saved, fname)
	}
	return saved
}

// --- chat.db reader ---

type chatMessage struct {
	RowID          int64
	Text           string
	AttributedBody []byte
	Date           int64 // nanoseconds since 2001-01-01 UTC
	IsFromMe       int
}

// resolveSelfChatHandles merges env override (comma-separated), the legacy
// singular SelfChatHandle, and the SelfChatHandles list. Returned slice is
// deduplicated and order-preserved.
func resolveSelfChatHandles(c CaptureConfig) []string {
	var raw []string
	if env := os.Getenv("SCRIBE_SELF_CHAT_ID"); env != "" {
		raw = append(raw, strings.Split(env, ",")...)
	} else {
		raw = append(raw, c.SelfChatHandles...)
		if c.SelfChatHandle != "" {
			raw = append(raw, c.SelfChatHandle)
		}
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, h := range raw {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// readSelfChatMessages opens chat.db read-only and returns self-sent messages
// to any of the configured handles since the cutoff date. Each handle maps to
// a distinct chat in chat.db (phone-number chat vs Apple-ID-email chat), so
// callers must pass every address they use to message themselves.
func readSelfChatMessages(selfChatIDs []string, since string) ([]chatMessage, error) {
	if len(selfChatIDs) == 0 {
		return nil, errors.New("no self-chat handles supplied")
	}
	dbPath := filepath.Join(os.Getenv("HOME"), "Library", "Messages", "chat.db")
	if !fileExists(dbPath) {
		return nil, fmt.Errorf("chat.db not found at %s", dbPath)
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open chat.db: %w", err)
	}
	defer db.Close()

	// Resolve handle ROWIDs. macOS stores one `handle` row per (id, service)
	// pair — a single phone number typically has separate ROWIDs for iMessage
	// and SMS. Pulling only the first row (QueryRow) silently dropped messages
	// that happened to live in the chat keyed off the other ROWID, which is
	// what hid 11 vacation links from the user. We now collect every ROWID
	// for each handle id.
	var handleIDs []int64
	var missing []string
	for _, id := range selfChatIDs {
		ids, qerr := handleRowIDs(db, id)
		if qerr != nil {
			msg := qerr.Error()
			if strings.Contains(msg, "operation not permitted") || strings.Contains(msg, "unable to open") {
				return nil, fmt.Errorf("query handle: %w\n\nfull disk access required. macOS blocks reads of ~/Library/Messages/chat.db without it.\nGrant it to the scribe binary:\n  System Settings → Privacy & Security → Full Disk Access → add %s\nFor LaunchAgent-driven captures, the binary itself needs the grant (inheriting from Terminal is not enough)", qerr, os.Args[0])
			}
			return nil, fmt.Errorf("query handle %s: %w", id, qerr)
		}
		handleIDs = append(handleIDs, ids...)
		if len(ids) == 0 {
			missing = append(missing, id)
		}
	}
	for _, id := range missing {
		logMsg("capture", "self-chat handle %q not found in chat.db (skipping)", id)
	}
	if len(handleIDs) == 0 {
		return nil, fmt.Errorf("none of the configured self-chat handles exist in chat.db: %s", strings.Join(selfChatIDs, ", "))
	}

	sinceTime, err := time.Parse("2006-01-02", since)
	if err != nil {
		return nil, fmt.Errorf("parse since date %q: %w", since, err)
	}
	sinceApple := (sinceTime.UTC().Unix() - appleEpochOffset) * 1_000_000_000

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(handleIDs)), ",")
	args := make([]any, 0, len(handleIDs)+1)
	for _, h := range handleIDs {
		args = append(args, h)
	}
	args = append(args, sinceApple)

	// placeholders is a literal '?,?,...' built from strings.Repeat — no user
	// input enters the SQL string, so the gosec G202 concatenation finding is
	// a false positive here. Every dynamic value is bound through args.
	//
	// `is_from_me = 1` was dropped from the WHERE clause. The original
	// intent was "only the user's own messages, not random correspondents"
	// — but this query is already scoped to chats keyed off the user's
	// own self-chat handles (single-participant chats by definition).
	// The catch is that a single-participant chat can still have
	// is_from_me=0 messages: when the user's iPhone (Apple-ID handle =
	// phone) sends to their Mac (Apple-ID handle = email), the Mac's
	// chat.db records the message with the iPhone as the sender →
	// is_from_me=0 from the Mac's perspective even though it's still
	// the user's own message. Filtering those out silently dropped
	// every link sent from one device to another.
	query := `
		SELECT DISTINCT m.ROWID, m.text, m.attributedBody, m.date, m.is_from_me
		FROM message m
		JOIN chat_message_join cmj ON m.ROWID = cmj.message_id
		JOIN chat_handle_join chj ON cmj.chat_id = chj.chat_id
		WHERE chj.handle_id IN (` + placeholders + `)
		  AND m.date > ?
		ORDER BY m.date ASC
	` //#nosec G202 -- placeholders are literal '?' chars, not user-controlled
	//nolint:noctx // CLI top-level, no context propagation yet
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var out []chatMessage
	for rows.Next() {
		var (
			m       chatMessage
			text    sql.NullString
			attr    []byte
			dateVal sql.NullInt64
			isFrom  sql.NullInt64
		)
		if err := rows.Scan(&m.RowID, &text, &attr, &dateVal, &isFrom); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		if text.Valid {
			m.Text = text.String
		}
		m.AttributedBody = attr
		if dateVal.Valid {
			m.Date = dateVal.Int64
		}
		if isFrom.Valid {
			m.IsFromMe = int(isFrom.Int64)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}
	return out, nil
}

// handleRowIDs returns every handle ROWID for one handle id. Split out of
// readSelfChatMessages so rows.Close can be deferred per query — the caller
// loops over handles, where a bare defer would pile up until function exit.
func handleRowIDs(db *sql.DB, id string) ([]int64, error) {
	//nolint:noctx // CLI top-level, no context propagation yet
	rows, err := db.Query("SELECT ROWID FROM handle WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, rowid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}

// extractURLsFromBytes scans raw bytes for URLs. Strictly byte-oriented — stops
// at any control char (0x00-0x1f, 0x7f), high-bit byte (0x80+), or URL-breaking
// ASCII punctuation. This matches Python's bytes-mode regex behavior used by
// the original scripts/capture-imessage.sh and avoids Go's Unicode-aware regex
// bleeding non-ASCII bytes into the match.
func extractURLsFromBytes(blob []byte) []string {
	var urls []string
	for i := 0; i < len(blob); {
		var end int
		switch {
		case i+7 <= len(blob) && string(blob[i:i+7]) == "http://":
			end = i + 7
		case i+8 <= len(blob) && string(blob[i:i+8]) == "https://":
			end = i + 8
		default:
			i++
			continue
		}
		for end < len(blob) && isURLByte(blob[end]) {
			end++
		}
		// Require at least one byte of path after the scheme.
		if end > i+8 {
			urls = append(urls, string(blob[i:end]))
		}
		i = end
	}
	return urls
}

func isURLByte(b byte) bool {
	if b <= 0x1f || b >= 0x7f {
		return false
	}
	switch b {
	case ' ', '<', '>', '"', '\'', ')', ']':
		return false
	}
	return true
}

// appleNanosToTime converts an Apple CFAbsoluteTime nanosecond value to time.Time.
func appleNanosToTime(nanos int64) time.Time {
	secs := nanos/1_000_000_000 + appleEpochOffset
	return time.Unix(secs, 0).UTC()
}

// --- helpers ---

func shouldSkipURL(u string, skipDomains []string) bool {
	for _, d := range skipDomains {
		if d == "" {
			continue
		}
		if strings.Contains(u, d) {
			return true
		}
	}
	return false
}

// slugFromCapturedURL matches the bash slug logic: strip scheme + www, replace
// non-alphanumeric runs with '-', truncate to 60, lowercase.
var captureNonAlnumRE = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func slugFromCapturedURL(rawURL string) string {
	s := rawURL
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	s = captureNonAlnumRE.ReplaceAllString(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	s = strings.Trim(s, "-")
	return strings.ToLower(s)
}

// slugifyText slugs free text (note titles): non-alnum to '-', truncate, lower.
func slugifyText(text string, limit int) string {
	if len(text) > limit {
		text = text[:limit]
	}
	s := captureNonAlnumRE.ReplaceAllString(text, "-")
	s = strings.Trim(s, "-")
	return strings.ToLower(s)
}

// buildCaptureArticle assembles frontmatter + body for a captured iMessage
// entry. Shape mirrors ingest.go's buildRawArticle but adds the source_url
// only when present (notes have no URL).
func buildCaptureArticle(rawURL, title, body, via, date string, tags []string) string {
	safeTitle := strings.ReplaceAll(title, `"`, `\"`)
	tagLine := "[]"
	if len(tags) > 0 {
		tagLine = "[" + strings.Join(tags, ", ") + "]"
	}

	var fm strings.Builder
	fm.WriteString("---\n")
	fmt.Fprintf(&fm, "title: \"%s\"\n", safeTitle)
	if rawURL != "" {
		fmt.Fprintf(&fm, "source_url: \"%s\"\n", rawURL)
	}
	fmt.Fprintf(&fm, "captured: %s\n", date)
	fmt.Fprintf(&fm, "fetched_via: %s\n", via)
	fm.WriteString("type: article\n")
	fm.WriteString("domain: general\n")
	fmt.Fprintf(&fm, "tags: %s\n", tagLine)
	fm.WriteString("---\n\n")
	fm.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		fm.WriteString("\n")
	}
	return fm.String()
}

func writeCaptureFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// --- state file I/O ---

func loadCaptureState(path string) (*captureState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &captureState{}, nil
		}
		return nil, err
	}
	var st captureState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state json: %w", err)
	}
	return &st, nil
}

func saveCaptureState(path string, st *captureState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
