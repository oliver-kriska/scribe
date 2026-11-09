package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// adoption_transcripts.go computes the KB-first adoption metric (#23)
// from Claude Code's own session files instead of ccrider.
//
// Why not ccrider: ccrider's importer drops every message with no
// extracted text, and Claude Code writes each tool call as its own
// assistant JSONL line with no text block. Measured on 2026-11-09, ccrider
// held 26,620 Claude assistant rows from the last 30 days and exactly ONE
// of them contained a tool_use block. The ccrider-based metric therefore
// reported 0/0 decision sessions for months while looking healthy. The
// raw files under ~/.claude/projects/<encoded-cwd>/<session>.jsonl keep
// every tool call, in order, with the session's cwd on each line.
//
// Scope matches the ccrider path (plan D3): main sessions only (files
// directly inside a project folder; subagent transcripts under
// <session>/subagents/ are skipped), modified within the window, cwd in an
// approved manifest project, and not a session inside the KB itself.

// transcriptLine is the subset of one Claude Code JSONL line we read.
type transcriptLine struct {
	Type    string `json:"type"`
	Cwd     string `json:"cwd"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type transcriptToolBlock struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Input struct {
		Command string `json:"command"`
	} `json:"input"`
}

// qmdBashMarkers are the CLI forms the handshake block documents as the
// fallback when the qmd MCP server is unavailable.
var qmdBashMarkers = []string{"qmd query", "qmd search", "qmd vsearch", "qmd get", "qmd multi-get"}

// isQMDToolName reports whether an MCP tool name is a qmd retrieval call,
// whatever alias the user registered the server under
// (mcp__plugin_qmd_qmd__query, mcp__qmd__query, ...).
func isQMDToolName(name string) bool {
	if !strings.HasPrefix(name, "mcp__") {
		return false
	}
	for _, suffix := range []string{"qmd__query", "qmd__get", "qmd__multi_get", "qmd__search", "qmd__vsearch"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func isDecisionToolName(name string) bool {
	switch name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

// classifyTranscriptBlocks inspects one assistant line's content array.
func classifyTranscriptBlocks(raw json.RawMessage) (isQMD, isDecision bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return false, false // plain-string content carries no tool calls
	}
	var blocks []transcriptToolBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false, false
	}
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		if isQMDToolName(b.Name) || (b.Name == "Bash" && containsAny(b.Input.Command, qmdBashMarkers)) {
			isQMD = true
		}
		if isDecisionToolName(b.Name) {
			isDecision = true
		}
	}
	return isQMD, isDecision
}

// transcriptSessionOutcome is what one session file contributes.
type transcriptSessionOutcome struct {
	Cwd         string
	HasDecision bool
	KBFirst     bool // a qmd call happened before the first decision call
}

// transcriptLineMax bounds a single JSONL line. Tool results inlining whole
// files can be large; lines beyond this are skipped, not fatal.
const transcriptLineMax = 16 << 20

var assistantMarker = []byte(`"type":"assistant"`)

// scanTranscriptForAdoption reads one session file up to its first
// decision call. It stops there because nothing after the first edit can
// change the KB-first verdict.
func scanTranscriptForAdoption(path string) (transcriptSessionOutcome, error) {
	var out transcriptSessionOutcome
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256<<10), transcriptLineMax)
	sawQMD := false
	for sc.Scan() {
		line := sc.Bytes()
		// Cheap pre-filter: only assistant lines carry tool_use, and we
		// need the first line with a cwd for scoping. A false positive
		// (the marker inside a user line's text) is caught by the JSON
		// type check below.
		if out.Cwd != "" && !bytes.Contains(line, assistantMarker) {
			continue
		}
		var tl transcriptLine
		if err := json.Unmarshal(line, &tl); err != nil {
			continue
		}
		if out.Cwd == "" && tl.Cwd != "" {
			out.Cwd = tl.Cwd
		}
		if tl.Type != "assistant" {
			continue
		}
		isQMD, isDecision := classifyTranscriptBlocks(tl.Message.Content)
		if isQMD {
			sawQMD = true
		}
		if isDecision {
			out.HasDecision = true
			out.KBFirst = sawQMD
			return out, nil
		}
	}
	// A too-long line ends the scan early; treat what we saw as the
	// outcome rather than discarding the session.
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return out, err
	}
	return out, nil
}

// computeAdoptionFromTranscripts runs one window over projectsDir.
// ok=false when projectsDir does not exist, so the caller can fall back
// to the ccrider path.
func computeAdoptionFromTranscripts(root, projectsDir string, days int, now time.Time) (adoptionWindowResult, bool, error) {
	result := adoptionWindowResult{Days: days}
	if projectsDir == "" || !dirExists(projectsDir) {
		return result, false, nil
	}
	manifest, err := loadManifest(root)
	if err != nil {
		return result, true, err
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)

	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return result, true, err
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(projectsDir, p.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fe := range files {
			if fe.IsDir() || !strings.HasSuffix(fe.Name(), ".jsonl") {
				continue
			}
			info, err := fe.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				continue
			}
			o, err := scanTranscriptForAdoption(filepath.Join(dir, fe.Name()))
			if err != nil || o.Cwd == "" || sessionInKB(root, o.Cwd) {
				continue
			}
			entry := manifest.entryForPath(o.Cwd)
			if entry == nil || !entry.IsApproved() {
				continue
			}
			if !o.HasDecision {
				result.ExcludedNoDecision++
				continue
			}
			result.DecisionSessions++
			if o.KBFirst {
				result.KBFirstSessions++
			}
		}
	}
	return result, true, nil
}
