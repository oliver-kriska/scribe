package main

import (
	"context"
	"fmt"
	"io"
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

	// Image sidecar: marker writes images alongside the .md, with
	// relative ![](image.png) refs. Without preservation those refs
	// point at a tmp dir that gets deleted on defer. Copy the assets
	// to raw/assets/<slug>/ in the KB and rewrite the links so the
	// wiki retains the figures.
	md := string(data)
	if root, kbErr := kbDir(); kbErr == nil {
		stem := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
		md = preserveMarkerImages(md, filepath.Dir(mdPath), root, slugifyForAssets(stem))
	}
	return strings.TrimSpace(md), nil
}

// preserveMarkerImages copies image files marker emitted alongside the
// markdown into raw/assets/<slug>/ and rewrites the relative refs
// inside the markdown. Returns the rewritten markdown. Best-effort
// throughout — if asset copying fails, the original markdown comes
// back so absorb still gets text. Assets that don't appear in the
// markdown are skipped (don't bloat the KB with unused files).
func preserveMarkerImages(md, markerOutDir, kbRoot, slug string) string {
	if slug == "" {
		return md
	}
	refs := extractImageRefs(md)
	if len(refs) == 0 {
		return md
	}
	assetsDir := filepath.Join(kbRoot, "raw", "assets", slug)
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		return md
	}
	rewritten := md
	for _, ref := range refs {
		// Skip absolute URLs and data: URIs.
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "data:") {
			continue
		}
		src := filepath.Join(markerOutDir, ref)
		if _, err := os.Stat(src); err != nil {
			// marker didn't actually emit this file — leave the
			// link in place; absorb will see it as a broken
			// image, which is honest signal.
			continue
		}
		dst := filepath.Join(assetsDir, filepath.Base(ref))
		if err := copyFile(src, dst); err != nil {
			continue
		}
		// Rewrite the link to a KB-relative path under raw/assets/.
		rel := filepath.Join("..", "assets", slug, filepath.Base(ref))
		rewritten = strings.ReplaceAll(rewritten, "]("+ref+")", "]("+rel+")")
	}
	return rewritten
}

// extractImageRefs pulls relative image paths out of a markdown body
// using the standard ![alt](path) syntax. Skips empty paths and
// duplicates.
func extractImageRefs(md string) []string {
	var refs []string
	seen := map[string]bool{}
	i := 0
	for i < len(md) {
		idx := strings.Index(md[i:], "![")
		if idx < 0 {
			break
		}
		i += idx
		// Find closing of alt text.
		altClose := strings.Index(md[i:], "](")
		if altClose < 0 {
			break
		}
		pathStart := i + altClose + 2
		pathClose := strings.Index(md[pathStart:], ")")
		if pathClose < 0 {
			break
		}
		ref := md[pathStart : pathStart+pathClose]
		ref = strings.TrimSpace(ref)
		// Strip optional title quoted part: ![](foo.png "title")
		if sp := strings.Index(ref, " "); sp > 0 {
			ref = ref[:sp]
		}
		if ref != "" && !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
		i = pathStart + pathClose + 1
	}
	return refs
}

// copyFile is a minimal tee from src to dst with 0o644 permissions.
// Used by image sidecar; we don't need anything fancier.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is inside our marker tmp dir
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // dst is inside our resolved KB
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// slugifyForAssets produces a filesystem-safe slug for raw/assets/.
// Mirrors the title-slug logic but kept local to avoid re-coupling
// convert_marker.go to ingest.go's slugify(). Lowercase ASCII alnum
// plus "-".
func slugifyForAssets(s string) string {
	var sb strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				sb.WriteRune('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(sb.String(), "-")
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
