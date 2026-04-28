package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// InstallToolsCmd is scribe's explicit bootstrap surface — the user
// runs this once after `brew install scribe` to enable the full
// PDF/DOCX/PPTX/XLSX/EPUB conversion path. Lazy bootstrap (triggered
// from the convert dispatcher when a non-text format hits without
// marker present) calls into the same helpers.
//
// The install path:
//
//  1. Fetch a pinned static `uv` binary from astral.sh into
//     ~/.scribe/bin/uv (SHA256-verified, xattr-cleared on macOS).
//  2. Run `uv tool install marker-pdf` so marker_single lands on
//     PATH via uv's own ~/.local/bin shim.
//  3. Write ~/.scribe/install-state.json so doctor + lazy bootstrap
//     can detect "we already did this" without re-running.
//
// `brew upgrade scribe` does not touch ~/.scribe/, so marker upgrades
// are explicit (`scribe install-tools --upgrade`). That's the right
// boundary — we don't want a routine scribe bump pulling a
// breaking marker version onto the user's machine without warning.
type InstallToolsCmd struct {
	Upgrade bool `help:"Upgrade an already-installed marker-pdf to the latest pip release."`
	Force   bool `help:"Re-bootstrap uv even if it is already on PATH or in ~/.scribe/bin."`
}

// Pinned uv release. Update the constants together: bumping uvVersion
// without refreshing uvSHA256 will fail the SHA verification step.
//
// SHA256 sums sourced from the .sha256 sidecar files on each release
// asset (astral-sh publishes one per platform). Captured during
// Phase 2A development against uv 0.11.8.
const uvVersion = "0.11.8"

var uvSHA256 = map[string]string{
	"darwin/arm64": "c729adb365114e844dd7f9316313a7ed6443b89bb5681d409eebac78b0bd06c8",
	"darwin/amd64": "c59d73bf34b58bc8e33a11629f7a255c11789fd00f03cd3e68ab2d1603645de9",
	"linux/amd64":  "56dd1b66701ecb62fe896abb919444e4b83c5e8645cca953e6ddd496ff8a0feb",
	"linux/arm64":  "eee8dd658d20e5ac85fec9c2326b6cbc9d83a1eef09ef07433e58698ac849591",
}

var uvAssetName = map[string]string{
	"darwin/arm64": "uv-aarch64-apple-darwin.tar.gz",
	"darwin/amd64": "uv-x86_64-apple-darwin.tar.gz",
	"linux/amd64":  "uv-x86_64-unknown-linux-gnu.tar.gz",
	"linux/arm64":  "uv-aarch64-unknown-linux-gnu.tar.gz",
}

func (c *InstallToolsCmd) Run() error {
	if c.Upgrade {
		return upgradeMarker()
	}
	uvPath, err := ensureUV(c.Force)
	if err != nil {
		return fmt.Errorf("install uv: %w", err)
	}
	if err := installMarkerViaUV(uvPath); err != nil {
		return fmt.Errorf("install marker-pdf: %w", err)
	}
	return persistInstallState(uvPath)
}

// scribeHome resolves ~/.scribe (created on demand). Tests can swap
// the parent via SCRIBE_HOME for hermetic temp dirs.
func scribeHome() (string, error) {
	if d := os.Getenv("SCRIBE_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".scribe"), nil
}

// ensureUV returns a path to a usable uv binary.
//
// Priority (matches Phase 2A research notes):
//
//  1. SCRIBE_UV env var (test/CI override).
//  2. uv already on PATH (system install — pipx, brew, manual).
//  3. ~/.scribe/bin/uv (previous install-tools run).
//  4. Fresh download from astral.sh into ~/.scribe/bin/.
//
// Force=true skips priorities 2 and 3 to handle "the binary on PATH
// is broken" recovery.
func ensureUV(force bool) (string, error) {
	if d := os.Getenv("SCRIBE_UV"); d != "" {
		return d, nil
	}
	if !force {
		if path, err := exec.LookPath("uv"); err == nil {
			return path, nil
		}
		home, err := scribeHome()
		if err == nil {
			candidate := filepath.Join(home, "bin", "uv")
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			}
		}
	}
	return downloadUV()
}

// downloadUV fetches the pinned uv release for this OS/arch into
// ~/.scribe/bin/uv. SHA-verified before the binary is moved into
// place; on macOS we clear com.apple.quarantine so Gatekeeper doesn't
// silently block first launch.
func downloadUV() (string, error) {
	target := runtime.GOOS + "/" + runtime.GOARCH
	asset, ok := uvAssetName[target]
	if !ok {
		return "", fmt.Errorf("uv: no pinned binary for %s — install uv manually then re-run", target)
	}
	expectedSHA, ok := uvSHA256[target]
	if !ok {
		return "", fmt.Errorf("uv: missing SHA for %s — bump uvSHA256 or install manually", target)
	}

	home, err := scribeHome()
	if err != nil {
		return "", err
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", binDir, err)
	}

	url := fmt.Sprintf("https://github.com/astral-sh/uv/releases/download/%s/%s", uvVersion, asset)
	fmt.Printf("scribe: fetching uv %s for %s ...\n", uvVersion, target)

	tarball, err := httpGetVerified(url, expectedSHA)
	if err != nil {
		return "", err
	}

	uvPath := filepath.Join(binDir, "uv")
	if err := extractUVFromTarball(tarball, uvPath); err != nil {
		return "", err
	}

	// macOS: strip the quarantine xattr so the binary launches without
	// Gatekeeper popping a "downloaded from internet" dialog. Best-
	// effort — ENOATTR is fine (older macOS doesn't always tag
	// command-line downloads), and the xattr binary is in /usr/bin so
	// availability is reliable.
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = exec.CommandContext(ctx, "xattr", "-d", "com.apple.quarantine", uvPath).Run()
		cancel()
	}

	if err := os.Chmod(uvPath, 0o755); err != nil {
		return "", fmt.Errorf("chmod uv: %w", err)
	}
	fmt.Printf("scribe: installed uv to %s\n", uvPath)
	return uvPath, nil
}

// httpGetVerified downloads url, verifies the SHA256 matches expected
// (lowercase hex), and returns the body bytes. Streaming to memory is
// fine — the uv tarball is ~30 MB.
func httpGetVerified(url, expectedSHA string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url) //nolint:noctx,gosec // URL pinned by version constant; download is intentional
	if err != nil {
		return nil, fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http get %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, expectedSHA) {
		return nil, fmt.Errorf("sha256 mismatch: got %s, want %s — refusing to install untrusted binary", got, expectedSHA)
	}
	return body, nil
}

// extractUVFromTarball pulls the uv executable out of the tarball and
// writes it atomically to dst. uv ships a single executable inside a
// version-named directory; we walk the tar and pick the first regular
// file named "uv".
func extractUVFromTarball(tarball []byte, dst string) error {
	gz, err := gzip.NewReader(strings.NewReader(string(tarball)))
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if base != "uv" {
			continue
		}
		tmp := dst + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // path computed from scribeHome, not user input
		if err != nil {
			return fmt.Errorf("open tmp: %w", err)
		}
		if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // tarball already SHA-verified upstream
			f.Close()
			return fmt.Errorf("write uv: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close uv: %w", err)
		}
		return os.Rename(tmp, dst)
	}
	return fmt.Errorf("uv binary not found in tarball")
}

// installMarkerViaUV runs `uv tool install marker-pdf`. uv prints
// progress to its own stderr; we inherit so the user sees the ~3 GB
// weights download in real time.
func installMarkerViaUV(uvPath string) error {
	if markerTierAvailable() {
		fmt.Println("scribe: marker_single already on PATH — skipping uv tool install")
		return nil
	}
	fmt.Println("scribe: installing marker-pdf via uv (first run downloads ~3 GB of model weights — one-time)")
	// 30-minute ceiling for the model download — generous enough for
	// slow connections, tight enough that a wedged install doesn't
	// hang scribe forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, uvPath, "tool", "install", "marker-pdf")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("uv tool install marker-pdf: %w", err)
	}
	return nil
}

// upgradeMarker runs `uv tool upgrade marker-pdf` (or installs if
// missing). Symmetric to install — never touches uv itself; that
// upgrade is bundled into a future scribe release.
func upgradeMarker() error {
	uvPath, err := ensureUV(false)
	if err != nil {
		return err
	}
	if !markerTierAvailable() {
		return installMarkerViaUV(uvPath)
	}
	fmt.Println("scribe: upgrading marker-pdf to latest release")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, uvPath, "tool", "upgrade", "marker-pdf")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("uv tool upgrade: %w", err)
	}
	return persistInstallState(uvPath)
}

// installState records the most recent successful bootstrap. doctor
// surfaces the timestamp + uv version so users can spot drift between
// scribe releases and their pinned tool stack.
type installState struct {
	UVPath        string `json:"uv_path"`
	UVVersion     string `json:"uv_version"`
	MarkerVersion string `json:"marker_version,omitempty"`
	InstalledAt   string `json:"installed_at"`
}

func persistInstallState(uvPath string) error {
	home, err := scribeHome()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("mkdir scribe home: %w", err)
	}
	st := installState{
		UVPath:        uvPath,
		UVVersion:     uvVersion,
		MarkerVersion: markerVersionLine(),
		InstalledAt:   time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal install state: %w", err)
	}
	dst := filepath.Join(home, "install-state.json")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// readInstallState returns the persisted bootstrap record, or nil if
// no install-state.json exists. Callers must tolerate nil.
func readInstallState() *installState {
	home, err := scribeHome()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, "install-state.json"))
	if err != nil {
		return nil
	}
	var st installState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	return &st
}

// lazyBootstrapMarker is the entry point convert_marker.go calls when
// it discovers marker is missing on a code path that needs it. We
// only auto-bootstrap when the user has consented via env var or
// scribe.yaml — first-run downloads of 3 GB without consent would be
// surprising. Returns nil if marker is now available, an error
// otherwise (caller surfaces "install marker-pdf or set
// SCRIBE_AUTO_INSTALL_TOOLS=1").
func lazyBootstrapMarker() error {
	if os.Getenv("SCRIBE_AUTO_INSTALL_TOOLS") != "1" {
		return fmt.Errorf("marker not installed; set SCRIBE_AUTO_INSTALL_TOOLS=1 or run `scribe install-tools` to bootstrap")
	}
	cmd := &InstallToolsCmd{}
	return cmd.Run()
}
