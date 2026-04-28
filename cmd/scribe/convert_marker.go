package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// MarkerStats summarizes what marker did to a single document. It comes
// from <stem>_meta.json which marker writes alongside the markdown.
// Pages = total page_stats entries. OCRPages counts pages whose
// text_extraction_method ≠ "pdftext" (marker uses "surya" / "ocr" /
// "ocr_error" for OCR'd pages — anything non-pdftext means the layout
// model handled the page, which is our confidence proxy). Tests can
// inspect both raw maps; production callers usually only need the
// derived ratio surfaced as `ocr_pct`.
type MarkerStats struct {
	Pages          int     `json:"pages"`
	OCRPages       int     `json:"ocr_pages"`
	OCRPct         float64 `json:"ocr_pct"`         // 0.0..1.0
	ExtractionMode string  `json:"extraction_mode"` // "pdftext" | "ocr" | "mixed"
}

// convertWithMarker reads ingest config from the resolved KB root.
// When called without a KB context (tests, edge cases), it falls back
// to defaults — never panics on missing config. Returns the converted
// markdown plus optional stats parsed from marker's meta.json.
func convertWithMarker(inputPath, _ string) (string, *MarkerStats, error) {
	abs, err := filepath.Abs(inputPath)
	if err != nil {
		return "", nil, fmt.Errorf("abs path: %w", err)
	}

	// Resolve marker config. kbDir() is how every other command finds
	// scribe.yaml; if we're outside a KB (tests, edge cases) the
	// loadConfig path returns built-in defaults so we never panic on a
	// missing yaml file.
	timeout := time.Duration(defaultMarkerTimeoutSec) * time.Second
	mpsFallback := true
	device := "auto"
	if root, kbErr := kbDir(); kbErr == nil {
		cfg := loadConfig(root)
		if cfg != nil {
			if cfg.Ingest.Marker.TimeoutSeconds > 0 {
				timeout = time.Duration(cfg.Ingest.Marker.TimeoutSeconds) * time.Second
			}
			if cfg.Ingest.Marker.MPSFallback != nil {
				mpsFallback = *cfg.Ingest.Marker.MPSFallback
			}
			if cfg.Ingest.Marker.Device != "" {
				device = cfg.Ingest.Marker.Device
			}
		}
	}

	// Stage output in a per-invocation temp dir. marker writes a
	// subdirectory named after the input stem; we read the .md back
	// from there. Cleanup is deferred so a panic still removes the dir.
	tmpDir, err := os.MkdirTemp("", "scribe-marker-*")
	if err != nil {
		return "", nil, fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	mdPath, err := runMarkerWithDevice(abs, tmpDir, timeout, mpsFallback, device)
	if err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(mdPath) //nolint:gosec // path is from a tmp dir we just created
	if err != nil {
		return "", nil, fmt.Errorf("read marker output: %w", err)
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

	// Pull OCR confidence proxy from marker's <stem>_meta.json. Best
	// effort — older marker versions or partial outputs leave us with
	// nil stats and that's fine; the article still gets written.
	stats := readMarkerStats(mdPath)
	return strings.TrimSpace(md), stats, nil
}

// runMarkerWithDevice invokes marker_single with the configured device
// preference and returns the path to its emitted markdown. Two run
// strategies live here:
//
//   - device == "cpu" / "mps" / "cuda": single attempt, no retry. The
//     user (or scribe.yaml) explicitly pinned the backend.
//   - device == "auto": first attempt picks whatever torch defaults to.
//     If marker exits non-zero AND the stderr matches the surya/MPS
//     crash signature (AcceleratorError, "out of bounds" inside the
//     vision encoder), retry once with TORCH_DEVICE=cpu. This is the
//     graceful-degradation path — fast happy path, cheap recovery
//     when the GPU detonates.
//
// The retry only applies on macOS; Linux/Windows users have stable
// CUDA/CPU paths and don't need the safety net.
func runMarkerWithDevice(absInput, tmpDir string, timeout time.Duration, mpsFallback bool, device string) (string, error) {
	mdPath, stderr, err := runMarkerOnce(absInput, tmpDir, timeout, mpsFallback, device)
	if err == nil {
		return mdPath, nil
	}

	// On 'auto' macOS only: detect MPS crash signatures in stderr and
	// retry on CPU. Any other failure (timeout, missing weights,
	// corrupt PDF) bubbles up as-is so the quarantine path captures
	// the right error.
	if device == "auto" && runtime.GOOS == "darwin" && isMPSCrash(stderr) {
		// Wipe the temp dir contents from the failed attempt — marker
		// may have written partial state we don't want to mistake for
		// success on findMarkerOutput. RemoveAll + recreate is cheap.
		_ = os.RemoveAll(tmpDir)
		if mkErr := os.MkdirAll(tmpDir, 0o755); mkErr != nil {
			return "", fmt.Errorf("recreate tmp after mps crash: %w", mkErr)
		}
		fmt.Fprintln(os.Stderr, "scribe: marker MPS backend crashed; retrying on CPU (slower)")
		mdPath, _, retryErr := runMarkerOnce(absInput, tmpDir, timeout, mpsFallback, "cpu")
		if retryErr == nil {
			return mdPath, nil
		}
		return "", fmt.Errorf("marker_single failed on CPU retry: %w (initial MPS failure: %s)", retryErr, err.Error())
	}
	return "", err
}

// runMarkerOnce executes a single marker_single invocation and returns
// the markdown path on success, the captured stderr in any case (for
// crash-pattern matching), and an error on non-zero exit.
//
// Stderr is dual-routed: streamed live to os.Stderr so the user sees
// progress bars during a long conversion AND captured to a buffer so
// the caller can pattern-match crash signatures for the retry decision.
// Bounded buffer (~64 KB tail) keeps memory in check on multi-GB
// document runs.
func runMarkerOnce(absInput, tmpDir string, timeout time.Duration, mpsFallback bool, device string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{absInput, "--output_dir", tmpDir, "--output_format", "markdown"}
	// Only forward an explicit device flag when the user pinned one.
	// 'auto' lets torch decide; 'cpu' on the retry path uses the env
	// var rather than the flag because older marker releases didn't
	// expose --device. TORCH_DEVICE is the universal lever.
	cmd := exec.CommandContext(ctx, "marker_single", args...) //nolint:gosec // marker_single resolved from PATH; args are scribe-controlled
	cmd.Env = markerEnvWithDevice(os.Environ(), mpsFallback, device)

	var tail bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &boundedWriter{w: &tail, max: 64 * 1024})

	runErr := cmd.Run()
	stderr := tail.String()
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", stderr, fmt.Errorf("marker_single timed out after %s on %s (set ingest.marker.timeout_seconds in scribe.yaml to extend)", timeout, filepath.Base(absInput))
		}
		return "", stderr, fmt.Errorf("marker_single failed: %w", runErr)
	}
	mdPath, findErr := findMarkerOutput(tmpDir, absInput)
	if findErr != nil {
		return "", stderr, findErr
	}
	return mdPath, stderr, nil
}

// boundedWriter keeps only the last `max` bytes of input. Used to cap
// stderr capture so a noisy marker run (those infinite-looking
// progress bars) doesn't balloon scribe's heap on a multi-GB PDF.
// Trade-off accepted: pattern-matching looks at the tail of stderr,
// which is where Python tracebacks land — happy path for our retry
// decision.
type boundedWriter struct {
	w   *bytes.Buffer
	max int
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n >= b.max {
		b.w.Reset()
		b.w.Write(p[n-b.max:]) //nolint:errcheck // bytes.Buffer.Write never errors
		return n, nil
	}
	if b.w.Len()+n > b.max {
		// Shift the existing tail forward to make room.
		excess := (b.w.Len() + n) - b.max
		dropped := make([]byte, excess)
		_, _ = b.w.Read(dropped) // discard oldest bytes; bytes.Buffer.Read never errors on a non-empty buffer
	}
	b.w.Write(p) //nolint:errcheck // bytes.Buffer.Write never errors
	return n, nil
}

// isMPSCrash matches the crash signatures we have observed when surya
// detonates on Apple Silicon GPUs. Conservative: we only retry on
// patterns specific to the layout/encoder path — not on every
// AcceleratorError. The cost of a false negative (one quarantined
// file the user has to retry manually) is much lower than the cost
// of a false positive (re-running every transient error on CPU
// silently).
func isMPSCrash(stderr string) bool {
	if stderr == "" {
		return false
	}
	// Combinations we have seen in the wild:
	//   "torch.AcceleratorError: index ... is out of bounds"
	//   "RuntimeError: ... NDArray ..."
	//   "Placeholder shape mismatches" inside surya
	signals := []string{
		"AcceleratorError",
		"is out of bounds",
		"NDArray",
		"surya",
	}
	hits := 0
	for _, s := range signals {
		if strings.Contains(stderr, s) {
			hits++
		}
	}
	// Require at least two distinct signals so a transient
	// "AcceleratorError" with a totally different cause doesn't
	// trigger a wasted retry.
	return hits >= 2
}

// readMarkerStats parses <stem>_meta.json that marker writes alongside
// the markdown output. We only extract page count + extraction method
// distribution — the file also has a debug_data_path and a verbose
// table_of_contents we don't need. Returns nil on any parse failure;
// callers must tolerate nil.
func readMarkerStats(mdPath string) *MarkerStats {
	stem := strings.TrimSuffix(filepath.Base(mdPath), ".md")
	metaPath := filepath.Join(filepath.Dir(mdPath), stem+"_meta.json")
	data, err := os.ReadFile(metaPath) //nolint:gosec // metaPath is in the same tmp dir we just created
	if err != nil {
		return nil
	}
	var raw struct {
		PageStats []struct {
			TextExtractionMethod string `json:"text_extraction_method"`
		} `json:"page_stats"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	stats := &MarkerStats{Pages: len(raw.PageStats)}
	if stats.Pages == 0 {
		return stats
	}
	for _, p := range raw.PageStats {
		// pdftext = digital text layer extraction. Anything else
		// (surya, ocr, ocr_error) means the layout model had to
		// handle the page — quality drops accordingly.
		if p.TextExtractionMethod != "" && p.TextExtractionMethod != "pdftext" {
			stats.OCRPages++
		}
	}
	stats.OCRPct = float64(stats.OCRPages) / float64(stats.Pages)
	switch stats.OCRPages {
	case 0:
		stats.ExtractionMode = "pdftext"
	case stats.Pages:
		stats.ExtractionMode = "ocr"
	default:
		stats.ExtractionMode = "mixed"
	}
	return stats
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
	return markerEnvWithDevice(baseEnv, mpsFallback, "auto")
}

// markerEnvWithDevice extends markerEnv with a TORCH_DEVICE override
// when the caller pinned the backend (cpu/mps/cuda). On 'auto' we
// emit no TORCH_DEVICE so torch picks the platform default. The MPS
// fallback flag still applies on every macOS run regardless of
// device choice — fallback is a per-op safety net, not a backend
// switch.
func markerEnvWithDevice(baseEnv []string, mpsFallback bool, device string) []string {
	const fallbackKey = "PYTORCH_ENABLE_MPS_FALLBACK"
	const deviceKey = "TORCH_DEVICE"

	out := make([]string, 0, len(baseEnv)+2)
	for _, kv := range baseEnv {
		if strings.HasPrefix(kv, fallbackKey+"=") || strings.HasPrefix(kv, deviceKey+"=") {
			continue
		}
		out = append(out, kv)
	}
	if mpsFallback {
		out = append(out, fallbackKey+"=1")
	}
	switch device {
	case "cpu", "mps", "cuda":
		out = append(out, deviceKey+"="+device)
	}
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
