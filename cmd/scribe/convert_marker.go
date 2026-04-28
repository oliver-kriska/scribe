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
// best-in-class PDF/DOCX/PPTX/XLSX/EPUB → markdown converter. Phase 1A/B
// uses per-file invocations only — every call cold-loads ~3 GB of
// weights, which is fine for `scribe absorb foo.pdf` (one-shot
// interactive use) but will be replaced by marker_server batching in
// Phase 2 for `scribe sync` (drains many files at once).
//
// marker_single CLI surface (verified against upstream docs):
//
//	marker_single <input> --output_dir <dir> --output_format markdown
//
// It writes <output_dir>/<stem>/<stem>.md plus an images directory.
// Phase 1A reads the .md back and discards images; image-sidecar
// preservation lands in Phase 2.
//
// MPS fallback: surya (marker's layout model) hits NDArray crashes on
// Apple Silicon when running pure-MPS for some PDFs (verified during
// Phase 1A real-world test against ortoped.pdf). Setting
// PYTORCH_ENABLE_MPS_FALLBACK=1 routes unsupported ops back to CPU,
// trading speed for correctness. Default ON via ingest.marker.mps_fallback;
// users can disable explicitly in scribe.yaml.
//
// Default timeout is configurable via ingest.marker.timeout_seconds.
// 300s is generous for small-to-medium PDFs on CPU and tight enough
// that a malformed input doesn't hang an interactive session.
const defaultMarkerTimeoutSec = 300

// convertWithMarker reads ingest config from the resolved KB root.
// When called without a KB context (tests, edge cases), it falls back
// to defaults — never panics on missing config.
func convertWithMarker(inputPath, _ string) (string, error) {
	abs, err := filepath.Abs(inputPath)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}

	// Resolve marker config. kbDir() is how every other command finds
	// scribe.yaml; if we're outside a KB (tests, edge cases) the
	// loadConfig path returns built-in defaults so we never panic on a
	// missing yaml file.
	timeout := time.Duration(defaultMarkerTimeoutSec) * time.Second
	mpsFallback := true
	if root, kbErr := kbDir(); kbErr == nil {
		cfg := loadConfig(root)
		if cfg != nil {
			if cfg.Ingest.Marker.TimeoutSeconds > 0 {
				timeout = time.Duration(cfg.Ingest.Marker.TimeoutSeconds) * time.Second
			}
			if cfg.Ingest.Marker.MPSFallback != nil {
				mpsFallback = *cfg.Ingest.Marker.MPSFallback
			}
		}
	}

	// Stage output in a per-invocation temp dir. marker writes a
	// subdirectory named after the input stem; we read the .md back
	// from there. Cleanup is deferred so a panic still removes the dir.
	tmpDir, err := os.MkdirTemp("", "scribe-marker-*")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "marker_single", abs, "--output_dir", tmpDir, "--output_format", "markdown")
	// Inherit stderr so the user sees marker's progress lines during a
	// long conversion. stdout we discard — marker writes the actual
	// markdown to disk, not stdout.
	cmd.Stderr = os.Stderr
	cmd.Env = markerEnv(os.Environ(), mpsFallback)

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("marker_single timed out after %s on %s (set ingest.marker.timeout_seconds in scribe.yaml to extend)", timeout, filepath.Base(abs))
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

// markerEnv returns the environment for a marker subprocess. Adds
// PYTORCH_ENABLE_MPS_FALLBACK=1 when the caller requests the MPS
// fallback (default on macOS — works around the surya NDArray crash on
// Apple Silicon for some PDFs). Existing values in baseEnv are
// preserved unchanged unless they collide with our additions, in
// which case our value wins (the user already opted in via config).
func markerEnv(baseEnv []string, mpsFallback bool) []string {
	if !mpsFallback {
		return baseEnv
	}
	const key = "PYTORCH_ENABLE_MPS_FALLBACK"
	out := make([]string, 0, len(baseEnv)+1)
	for _, kv := range baseEnv {
		if strings.HasPrefix(kv, key+"=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, key+"=1")
	return out
}

// markerVersionLine returns the marker-pdf package version, or
// "unknown" if it can't be resolved. marker_single itself doesn't
// support --version, so we ask Python for the package version
// directly. Three probe strategies in order of reliability:
//
//  1. `pipx list --short` — fast and exact when the user installed
//     via pipx (the install path scribe will recommend in Phase 2).
//  2. `python -m importlib.metadata` against the marker-pdf venv.
//  3. fall back to "installed" if marker_single is on PATH but no
//     version source answers.
//
// Best-effort throughout — never fails the caller.
func markerVersionLine() string {
	if !markerTierAvailable() {
		return "not installed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try pipx first — it's the install path scribe will manage in
	// Phase 2 and most explicit about versions.
	if _, err := exec.LookPath("pipx"); err == nil {
		out, err := exec.CommandContext(ctx, "pipx", "list", "--short").CombinedOutput()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "marker-pdf ") {
					// "marker-pdf 1.10.2"
					return line
				}
			}
		}
	}

	return "installed (version unknown — install via pipx for version reporting)"
}
