package main

import (
	"bytes"
	"strings"
	"testing"
)

// Phase 2C test scope: TORCH_DEVICE knob, MPS-crash detection, and the
// bounded stderr capture used by the retry path. The actual marker
// subprocess + retry flow is exercised by hand against a real install
// — these tests cover the pure-Go decision layer.

func TestMarkerEnvWithDevice_AutoEmitsNoTorchDevice(t *testing.T) {
	got := markerEnvWithDevice(nil, true, "auto")
	for _, kv := range got {
		if strings.HasPrefix(kv, "TORCH_DEVICE=") {
			t.Errorf("auto must not emit TORCH_DEVICE; got %q", kv)
		}
	}
	// MPS fallback should still be set.
	if !containsKV(got, "PYTORCH_ENABLE_MPS_FALLBACK=1") {
		t.Errorf("auto with mpsFallback=true must emit PYTORCH_ENABLE_MPS_FALLBACK=1; got %v", got)
	}
}

func TestMarkerEnvWithDevice_PinsCPU(t *testing.T) {
	got := markerEnvWithDevice(nil, true, "cpu")
	if !containsKV(got, "TORCH_DEVICE=cpu") {
		t.Errorf("cpu device should emit TORCH_DEVICE=cpu; got %v", got)
	}
	if !containsKV(got, "PYTORCH_ENABLE_MPS_FALLBACK=1") {
		t.Errorf("MPS fallback should still be set on CPU device; got %v", got)
	}
}

func TestMarkerEnvWithDevice_PinsMPSAndCUDA(t *testing.T) {
	for _, dev := range []string{"mps", "cuda"} {
		got := markerEnvWithDevice(nil, false, dev)
		if !containsKV(got, "TORCH_DEVICE="+dev) {
			t.Errorf("%s device should emit TORCH_DEVICE=%s; got %v", dev, dev, got)
		}
		if containsKV(got, "PYTORCH_ENABLE_MPS_FALLBACK=1") {
			t.Errorf("mpsFallback=false must not emit fallback env on %s; got %v", dev, got)
		}
	}
}

func TestMarkerEnvWithDevice_UnknownDeviceIsIgnored(t *testing.T) {
	got := markerEnvWithDevice(nil, false, "tpu-9000")
	for _, kv := range got {
		if strings.HasPrefix(kv, "TORCH_DEVICE=") {
			t.Errorf("unknown device must not be forwarded; got %q", kv)
		}
	}
}

func TestMarkerEnvWithDevice_OverridesExisting(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"PYTORCH_ENABLE_MPS_FALLBACK=0", // user had it disabled
		"TORCH_DEVICE=mps",              // and pinned MPS
	}
	got := markerEnvWithDevice(base, true, "cpu")
	// Our values win on collision.
	if !containsKV(got, "TORCH_DEVICE=cpu") {
		t.Errorf("scribe's TORCH_DEVICE=cpu should override user's mps; got %v", got)
	}
	if !containsKV(got, "PYTORCH_ENABLE_MPS_FALLBACK=1") {
		t.Errorf("scribe's fallback=1 should override user's 0; got %v", got)
	}
	// Unrelated env survives.
	if !containsKV(got, "PATH=/usr/bin") {
		t.Errorf("PATH should pass through; got %v", got)
	}
	// No duplicate keys.
	count := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "TORCH_DEVICE=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("TORCH_DEVICE should appear exactly once; got %d in %v", count, got)
	}
}

func TestIsMPSCrash_HitsRealStackTrace(t *testing.T) {
	// Trimmed version of the actual crash captured during exercise:
	stderr := `Recognizing Layout: 0%|...
Traceback (most recent call last):
  File "/Users/oliverkriska/.local/pipx/venvs/marker-pdf/lib/python3.12/site-packages/surya/common/surya/encoder/__init__.py", line 438, in unpack_qkv_with_mask
    max_seq_len = seq_lengths.max().item()
torch.AcceleratorError: index 846921728 is out of bounds: 0, range 0 to 1
`
	if !isMPSCrash(stderr) {
		t.Error("expected real-world MPS stack trace to match")
	}
}

func TestIsMPSCrash_RejectsTransientUnrelatedErrors(t *testing.T) {
	cases := []string{
		"",
		"some innocent log line",
		// AcceleratorError alone (without surya/out-of-bounds) is
		// suspicious but not a definitive MPS crash signature.
		"torch.AcceleratorError: kernel timeout in third-party model",
		"PermissionError: cannot read /tmp/foo",
	}
	for _, s := range cases {
		if isMPSCrash(s) {
			t.Errorf("expected no match for %q", s)
		}
	}
}

func TestIsMPSCrash_NDArrayPattern(t *testing.T) {
	// The other crash family — same root cause (surya/MPS) but
	// different op.
	stderr := "RuntimeError in surya layout: NDArray shape mismatch on device mps"
	if !isMPSCrash(stderr) {
		t.Errorf("NDArray-in-surya should match")
	}
}

func TestBoundedWriter_CapsAtMaxBytes(t *testing.T) {
	var b boundedWriter
	b.max = 16
	b.w = bufHelper()
	// Write 50 bytes in two chunks.
	first := strings.Repeat("a", 30)
	second := strings.Repeat("b", 20)
	if _, err := b.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte(second)); err != nil {
		t.Fatal(err)
	}
	got := b.w.String()
	if len(got) > 16 {
		t.Errorf("buffer exceeded max: len=%d, content=%q", len(got), got)
	}
	// Last bytes win.
	if !strings.HasSuffix(got, "bbbbbbbbbb") {
		t.Errorf("expected tail to be most-recent bytes; got %q", got)
	}
}

func TestBoundedWriter_SingleChunkLargerThanMax(t *testing.T) {
	var b boundedWriter
	b.max = 8
	b.w = bufHelper()
	huge := strings.Repeat("x", 100)
	n, err := b.Write([]byte(huge))
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Errorf("Write should report bytes consumed (100), got %d", n)
	}
	if b.w.Len() != 8 {
		t.Errorf("buffer should hold exactly 8 trailing bytes; got %d (%q)", b.w.Len(), b.w.String())
	}
}

func TestIngestDefaults_MarkerDeviceIsAuto(t *testing.T) {
	def := ingestDefaults()
	if def.Marker.Device != "auto" {
		t.Errorf("default marker device should be auto, got %q", def.Marker.Device)
	}
}

func TestApplyIngestDefaults_PreservesUserDevice(t *testing.T) {
	cfg := IngestConfig{}
	cfg.Marker.Device = "cpu"
	applyIngestDefaults(&cfg)
	if cfg.Marker.Device != "cpu" {
		t.Errorf("user-set device should survive defaults; got %q", cfg.Marker.Device)
	}
}

func TestApplyIngestDefaults_FillsEmptyDevice(t *testing.T) {
	cfg := IngestConfig{}
	applyIngestDefaults(&cfg)
	if cfg.Marker.Device != "auto" {
		t.Errorf("empty device should default to auto; got %q", cfg.Marker.Device)
	}
}

// containsKV reports whether kv exists as an exact env entry (key=value).
func containsKV(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

// bufHelper returns a fresh bytes.Buffer for use as the inner writer
// of boundedWriter in tests.
func bufHelper() *bytes.Buffer { return &bytes.Buffer{} }
