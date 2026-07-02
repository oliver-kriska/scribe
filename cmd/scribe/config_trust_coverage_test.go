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
//   - mode/limits/num_ctx/timeout knobs: tuning that shapes HOW an op
//     runs, not where data goes. (provider/model ARE locked now — a named
//     hosted provider carries a built-in URL, so they redirect; see the
//     LLMProviders/LLMModels note in config_trust.go.)
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

	// Top-level LLM routing: provider/model/url/key-env are all locked
	// now (see LLMProviders/LLMModels). num_ctx is tuning; pricing is a
	// local cost-display table only — neither affects what's ingested,
	// where data goes, or any gate.
	"LLM.NumCtx":  true,
	"LLM.Pricing": true,

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

	// Priority-lane scheduling (issue #22): which already-queued/triaged
	// sessions mine first and how long a Normal entry waits before aging
	// into Hot. Pure ordering/scheduling tuning — it cannot widen what's
	// ingested, redirect data, or weaken a gate.
	"PriorityLanes.HotThreshold": true,
	"PriorityLanes.AgeDays":      true,

	// Absorb pipeline tuning (URL + provider + model leaves are locked;
	// these shape chunking/threshold/timeouts only).
	"Absorb.BriefThresholdWords":      true,
	"Absorb.BriefThresholdHeadings":   true,
	"Absorb.DenseThresholdWords":      true,
	"Absorb.DenseThresholdHeadings":   true,
	"Absorb.MaxPerRun":                true,
	"Absorb.Strictness":               true,
	"Absorb.SinglePassTimeoutMin":     true,
	"Absorb.SinglePassNumCtx":         true,
	"Absorb.Pass1TimeoutMin":          true,
	"Absorb.Pass2Mode":                true,
	"Absorb.Pass2TimeoutMin":          true,
	"Absorb.Pass2Parallel":            true,
	"Absorb.Pass2NumCtx":              true,
	"Absorb.ChapterAware":             true,
	"Absorb.ChapterThreshold":         true,
	"Absorb.ChapterParallel":          true,
	"Absorb.AtomicFacts":              true,
	"Absorb.FactsTimeoutMin":          true,
	"Absorb.Contextualize.Enabled":    true,
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

	// Per-op LLM routing: URL + provider + model are locked, the rest is
	// tuning.
	"SessionMine.Mode":               true,
	"SessionMine.TranscriptMaxChars": true,
	"SessionMine.TimeoutMin":         true,
	"SessionMine.NumCtx":             true,

	// Codex mining tuning (the Mine enable flag itself is locked).
	"Codex.SessionsMax":   true,
	"Codex.LookbackHours": true,
	"Codex.MinScore":      true,

	"Dream.Mode":       true,
	"Dream.TimeoutMin": true,
	"Dream.NumCtx":     true,

	"Assess.Mode":   true,
	"Assess.NumCtx": true,

	"DeepIngest.Mode":   true,
	"DeepIngest.NumCtx": true,

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

	// Stop-words commit gate (issue #25). Deliberately NOT trust-locked,
	// even though a pushed change could in principle weaken a filter —
	// two reasons: (1) the SOVEREIGN half of the list lives in each
	// member's ~/.config/scribe/config.yaml, which is never committed and
	// a push can never touch, so the hard privacy guarantee doesn't depend
	// on the shared list; (2) the shared list is convenience team policy
	// that members add words to constantly, and locking it would force
	// every teammate to re-trust on each addition — friction that would
	// discourage use of the very feature meant to protect them. The
	// shared list can only ADD protection a member also wants; it can't
	// widen ingestion, redirect data, or unlock the credential gate.
	"StopWords.Hold":      true,
	"StopWords.Mask":      true,
	"StopWords.Redaction": true,

	// Per-KB scheduler cadence (issue #26). A pushed change only shapes how
	// OFTEN `scribe each` runs a job in this KB — it cannot widen ingestion,
	// redirect data, or weaken a gate. Same class as the sync caps above.
	"Each.Cadence": true,
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
