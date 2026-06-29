package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestSensitiveConfigWiringComplete (config_trust_wiring_test.go)
// enumerates sensitiveConfig's OWN fields — it catches a field added to
// sensitiveConfig but mis-wired, not a field added to ScribeConfig and
// never put into sensitiveConfig at all. This test closes that gap from
// the other side: every leaf of ScribeConfig must either alter the
// sensitive view (= trust-locked) or appear in nonSensitiveAllowlist
// below. Adding any config field now forces a conscious decision; the
// allowlist entry IS the recorded decision.

// nonSensitiveAllowlist names every ScribeConfig leaf that is
// deliberately NOT trust-locked. Paths are Go field names, dot-joined.
//
// Rationale per group — a pushed change to these can cost tokens or
// degrade output quality, but it cannot widen what scribe ingests from
// this machine, redirect file contents to a foreign endpoint, or weaken
// the credential gate (the three classes sensitiveConfig locks):
//
//   - LoadErr: runtime parse-state, yaml:"-", never read from the repo.
//   - identity/cosmetics (owner, domains, kb_name, owners): display and
//     routing labels inside content that is already shared.
//   - provider/model/mode/limits knobs: with every OllamaURL locked, a
//     pushed provider flip can only route to an already-trusted
//     endpoint ("Per-op provider flips stay unlocked" — config_trust.go).
//   - pipeline tuning (sync caps, absorb thresholds, triage weights,
//     ingest converters, identities stopwords, meta rolling targets,
//     subscriptions): shapes what the pipeline does with data it was
//     already allowed to read.
//
// If a new field does NOT fit those rationales — it points scribe at a
// new input path, a new network endpoint, or relaxes a safety gate —
// it belongs in sensitiveConfig, not here.
var nonSensitiveAllowlist = map[string]bool{
	"LoadErr": true,

	// Identity / cosmetics.
	"OwnerName":    true,
	"OwnerContext": true,
	"Domains":      true,
	"Owners":       true,
	"KBName":       true,
	"LockDir":      true,
	"DefaultModel": true,

	// Top-level LLM routing (URL + key-env are locked; the rest is
	// tuning). Pricing is a local cost-display table only — it never
	// affects what's ingested, where data goes, or any gate.
	"LLM.Provider": true,
	"LLM.Model":    true,
	"LLM.NumCtx":   true,
	"LLM.Pricing":  true,

	// Sync pipeline caps + git cadence.
	"Sync.MaxExtractions":                   true,
	"Sync.MaxSessions":                      true,
	"Sync.MaxAbsorb":                        true,
	"Sync.ParallelExtractions":              true,
	"Sync.CheckpointInterval":               true,
	"Sync.MaxExtractFiles":                  true,
	"Sync.CommitDebounceMinutes":            true,
	"Sync.AutoApprove":                      true,
	"Sync.AlwaysPullBeforeSync":             true,
	"Sync.DailyAnthropicOutputTokenCeiling": true,
	"Sync.DailyOutputTokenCeiling":          true,

	"Deep.BatchMax": true,

	// Triage scoring.
	"Triage.Keywords": true,
	"Triage.Weights":  true,

	// Absorb pipeline tuning (URL leaves are locked; these shape
	// chunking/threshold/provider-model routing only).
	"Absorb.BriefThresholdWords":      true,
	"Absorb.BriefThresholdHeadings":   true,
	"Absorb.DenseThresholdWords":      true,
	"Absorb.DenseThresholdHeadings":   true,
	"Absorb.MaxPerRun":                true,
	"Absorb.Strictness":               true,
	"Absorb.SinglePassProvider":       true,
	"Absorb.SinglePassModel":          true,
	"Absorb.SinglePassTimeoutMin":     true,
	"Absorb.SinglePassNumCtx":         true,
	"Absorb.Pass1Provider":            true,
	"Absorb.Pass1Model":               true,
	"Absorb.Pass1TimeoutMin":          true,
	"Absorb.Pass2Provider":            true,
	"Absorb.Pass2Model":               true,
	"Absorb.Pass2Mode":                true,
	"Absorb.Pass2TimeoutMin":          true,
	"Absorb.Pass2Parallel":            true,
	"Absorb.Pass2NumCtx":              true,
	"Absorb.ChapterAware":             true,
	"Absorb.ChapterThreshold":         true,
	"Absorb.ChapterParallel":          true,
	"Absorb.AtomicFacts":              true,
	"Absorb.FactsProvider":            true,
	"Absorb.FactsModel":               true,
	"Absorb.FactsTimeoutMin":          true,
	"Absorb.Contextualize.Enabled":    true,
	"Absorb.Contextualize.Provider":   true,
	"Absorb.Contextualize.Model":      true,
	"Absorb.Contextualize.TimeoutSec": true,
	"Absorb.Contextualize.MaxPerRun":  true,

	// Ingest converters + routing.
	"Ingest.InboxPath":                true,
	"Ingest.Marker.TimeoutSeconds":    true,
	"Ingest.Marker.MPSFallback":       true,
	"Ingest.Marker.Device":            true,
	"Ingest.Converters":               true,
	"Ingest.SmartRouting.Enabled":     true,
	"Ingest.SmartRouting.MaxPDFBytes": true,
	"Ingest.SmartRouting.MaxPDFPages": true,

	// Identity clustering stopwords.
	"Identities.HandleStopwords":      true,
	"Identities.EmailDomainStopwords": true,

	// Per-op LLM routing: URLs are locked, the rest is tuning.
	"Relations.Provider": true,
	"Relations.Model":    true,

	"SessionMine.Provider":           true,
	"SessionMine.Model":              true,
	"SessionMine.Mode":               true,
	"SessionMine.TranscriptMaxChars": true,
	"SessionMine.TimeoutMin":         true,
	"SessionMine.NumCtx":             true,

	// Codex mining tuning (the Mine enable flag itself is locked).
	"Codex.SessionsMax":   true,
	"Codex.LookbackHours": true,
	"Codex.MinScore":      true,

	"Dream.Provider":   true,
	"Dream.Model":      true,
	"Dream.Mode":       true,
	"Dream.TimeoutMin": true,
	"Dream.NumCtx":     true,

	"Assess.Provider": true,
	"Assess.Model":    true,
	"Assess.Mode":     true,
	"Assess.NumCtx":   true,

	"DeepIngest.Provider": true,
	"DeepIngest.Model":    true,
	"DeepIngest.Mode":     true,
	"DeepIngest.NumCtx":   true,

	"Extract.Provider":      true,
	"Extract.Model":         true,
	"Extract.Mode":          true,
	"Extract.MaxFileChars":  true,
	"Extract.MaxTotalChars": true,
	"Extract.TimeoutMin":    true,
	"Extract.NumCtx":        true,

	// Digest subscriptions + meta side-channel targets.
	"Subscriptions.Domains": true,
	"Subscriptions.Tags":    true,
	"Subscriptions.Notify":  true,

	"Meta.RollingTargets": true,
}

// configLeaf is one settable leaf of ScribeConfig.
type configLeaf struct {
	path  string
	index []int
}

// collectConfigLeaves walks struct fields depth-first; anything that is
// not a struct counts as a leaf (slices/maps/pointers are mutated whole).
func collectConfigLeaves(typ reflect.Type, prefix string, index []int, out *[]configLeaf) {
	for i := range typ.NumField() {
		field := typ.Field(i)
		path := field.Name
		if prefix != "" {
			path = prefix + "." + field.Name
		}
		idx := append(append([]int{}, index...), i)
		if field.Type.Kind() == reflect.Struct {
			collectConfigLeaves(field.Type, path, idx, out)
			continue
		}
		*out = append(*out, configLeaf{path: path, index: idx})
	}
}

func TestScribeConfigSensitiveCoverage(t *testing.T) {
	var leaves []configLeaf
	collectConfigLeaves(reflect.TypeOf(ScribeConfig{}), "", nil, &leaves)
	if len(leaves) < 50 {
		t.Fatalf("only %d leaves — reflection walk broke?", len(leaves))
	}

	baseline := sensitiveFrom(&ScribeConfig{})
	seen := map[string]bool{}
	var undecided, stale []string

	for _, leaf := range leaves {
		seen[leaf.path] = true

		cfg := ScribeConfig{}
		v := reflect.ValueOf(&cfg).Elem().FieldByIndex(leaf.index)
		if !v.CanSet() || v.Kind() == reflect.Interface || v.Kind() == reflect.Func {
			// Unfillable (LoadErr's error interface) — still requires an
			// explicit allowlist decision.
			if !nonSensitiveAllowlist[leaf.path] {
				undecided = append(undecided, leaf.path)
			}
			continue
		}
		fillTrustValue(v)
		covered := !reflect.DeepEqual(sensitiveFrom(&cfg), baseline)

		switch {
		case covered && nonSensitiveAllowlist[leaf.path]:
			stale = append(stale, leaf.path)
		case !covered && !nonSensitiveAllowlist[leaf.path]:
			undecided = append(undecided, leaf.path)
		}
	}

	sort.Strings(undecided)
	for _, p := range undecided {
		t.Errorf("%s: new ScribeConfig leaf is neither trust-locked (sensitiveConfig) nor allowlisted — decide: does a pushed change to it widen ingestion, redirect data, or weaken a gate? Lock it in config_trust.go, or record the decision in nonSensitiveAllowlist", p)
	}
	sort.Strings(stale)
	for _, p := range stale {
		t.Errorf("%s: allowlisted as non-sensitive but it DOES alter the sensitive view — remove the stale allowlist entry", p)
	}

	// An allowlist entry for a leaf that no longer exists is a typo or a
	// leftover from a removed field — either way it must go.
	var unknown []string
	for p := range nonSensitiveAllowlist {
		if !seen[p] {
			unknown = append(unknown, p)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("nonSensitiveAllowlist entries match no ScribeConfig leaf: %s", strings.Join(unknown, ", "))
	}
}
