// config_bool_aliasing_test.go — regression tests for the *bool default
// aliasing bug found while building the issue-#9 stub harness.
//
// loadConfig prefills the config struct with the defaults constructors
// BEFORE yaml.Unmarshal, and yaml.v3 writes through existing non-nil
// pointers instead of allocating new ones. absorbDefaults used a single
// `trueV` variable for both ChapterAware and Contextualize.Enabled, so a
// user writing `absorb.contextualize.enabled: false` silently flipped
// `chapter_aware` off too (and vice versa). ingestDefaults had the same
// aliasing between Marker.MPSFallback and SmartRouting.Enabled. The
// constructors now give every *bool its own variable; these tests pin it.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeKBConfig(t *testing.T, yaml string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write scribe.yaml: %v", err)
	}
	return root
}

func TestAbsorbDefaults_NoBoolPointerAliasing(t *testing.T) {
	t.Run("contextualize.enabled=false leaves chapter_aware on", func(t *testing.T) {
		root := writeKBConfig(t, "absorb:\n  contextualize:\n    enabled: false\n")
		cfg := loadConfig(root)
		if cfg.LoadErr != nil {
			t.Fatalf("LoadErr: %v", cfg.LoadErr)
		}
		if cfg.Absorb.ChapterAware == nil || !*cfg.Absorb.ChapterAware {
			t.Errorf("chapter_aware flipped off by contextualize.enabled=false (default-pointer aliasing)")
		}
		if cfg.Absorb.Contextualize.Enabled == nil || *cfg.Absorb.Contextualize.Enabled {
			t.Errorf("contextualize.enabled=false not honored")
		}
	})

	t.Run("chapter_aware=false leaves contextualize.enabled on", func(t *testing.T) {
		root := writeKBConfig(t, "absorb:\n  chapter_aware: false\n")
		cfg := loadConfig(root)
		if cfg.Absorb.Contextualize.Enabled == nil || !*cfg.Absorb.Contextualize.Enabled {
			t.Errorf("contextualize.enabled flipped off by chapter_aware=false (default-pointer aliasing)")
		}
		if cfg.Absorb.ChapterAware == nil || *cfg.Absorb.ChapterAware {
			t.Errorf("chapter_aware=false not honored")
		}
	})
}

func TestIngestDefaults_NoBoolPointerAliasing(t *testing.T) {
	root := writeKBConfig(t, "ingest:\n  smart_routing:\n    enabled: false\n")
	cfg := loadConfig(root)
	if cfg.Ingest.Marker.MPSFallback == nil || !*cfg.Ingest.Marker.MPSFallback {
		t.Errorf("marker.mps_fallback flipped off by smart_routing.enabled=false (default-pointer aliasing)")
	}
	if cfg.Ingest.SmartRouting.Enabled == nil || *cfg.Ingest.SmartRouting.Enabled {
		t.Errorf("smart_routing.enabled=false not honored")
	}
}
