package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// convertWithMarker shells out to marker_single (datalab-to/marker), the
// best-in-class PDF/DOCX/PPTX/XLSX/EPUB → markdown converter. Phase 1A
// uses per-file invocations only — every call cold-loads ~3 GB of
// weights, which is fine for `scribe absorb foo.pdf` (one-shot
// interactive use) but will be replaced by marker_server batching in
// Phase 2 for `scribe sync` (drains many files at once).
//
// marker_single CLI surface (verified against upstream docs):
//
//	marker_single <input> <output_dir> [--output_format markdown|json|html]
//
// It writes <output_dir>/<stem>/<stem>.md plus an images directory.
// Phase 1A reads the .md back and discards images; image-sidecar
// preservation lands in Phase 2 (see auto-ingestion-plan.md §Phase 2).
//
// markerTimeout is the per-file ceiling. Default 5 minutes is generous
// for small-to-medium PDFs on CPU and tight enough that a malformed
// input doesn't hang an interactive session indefinitely.
const markerTimeout = 5 * time.Minute

func convertWithMarker(inputPath, _ string) (string, error) {
	abs, err := filepath.Abs(inputPath)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}

	// Stage output in a per-invocation temp dir. marker writes a
	// subdirectory named after the input stem; we read the .md back
	// from there. Cleanup is deferred so a panic still removes the dir.
	tmpDir, err := os.MkdirTemp("", "scribe-marker-*")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	ctx, cancel := context.WithTimeout(context.Background(), markerTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "marker_single", abs, "--output_dir", tmpDir, "--output_format", "markdown")
	// Inherit stderr so the user sees marker's progress lines during a
	// long conversion. stdout we discard — marker writes the actual
	// markdown to disk, not stdout.
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("marker_single timed out after %s on %s (set ingest.marker.timeout_seconds in scribe.yaml to extend)", markerTimeout, filepath.Base(abs))
		}
		return "", fmt.Errorf("marker_single failed: %w", err)
	}

	mdPath, err := findMarkerOutput(tmpDir, abs)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(mdPath) //nolint:gosec // path is from a tmp dir we just created
	if err != nil {
		return "", fmt.Errorf("read marker output: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// findMarkerOutput locates the .md file marker wrote inside outputDir.
// Marker's layout is outputDir/<stem>/<stem>.md, but the stem may be
// derived from the input filename in non-trivial ways (spaces, unicode).
// We walk the directory and pick the first .md file as the canonical
// output. If marker wrote nothing, return an error pointing at the
// stderr the user already saw.
func findMarkerOutput(outputDir, inputPath string) (string, error) {
	stem := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))
	expected := filepath.Join(outputDir, stem, stem+".md")
	if _, err := os.Stat(expected); err == nil {
		return expected, nil
	}

	var found string
	err := filepath.Walk(outputDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // skip unreadable, keep walking
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(path), ".md") && found == "" {
			found = path
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk marker output: %w", err)
	}
	if found == "" {
		return "", fmt.Errorf("marker_single produced no .md output for %s — check stderr above", filepath.Base(inputPath))
	}
	return found, nil
}

// markerVersionLine returns the first line of `marker_single --version`,
// or "unknown" if the command isn't installed or doesn't support
// --version. Used by `scribe doctor` (Phase 1B) and the install-tools
// upgrade check (Phase 2). Best-effort — never fails the caller.
func markerVersionLine() string {
	if !markerTierAvailable() {
		return "not installed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "marker_single", "--version").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return "unknown"
	}
	return line
}
