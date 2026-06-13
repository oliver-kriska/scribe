package main

import "testing"

// TestProjectScopeAllowed pins the session-mining scope guard: the
// ccrider lane must honor the same ignore-list + sources filter as project
// discovery, so a session from an ignored or source-excluded project can't
// leak into a scoped KB. Regression guard for the gap where preFilterSessions
// only deferred KNOWN-but-pending projects and let entry==nil sail through.
func TestProjectScopeAllowed(t *testing.T) {
	const (
		allowed = "/Users/o/Projects/enaia"
		other   = "/Users/o/Projects/donat"
	)
	scoped := &ScribeConfig{Sources: SourcesConfig{Include: []string{"/Users/o/Projects/enaia"}}}
	excluded := &ScribeConfig{Sources: SourcesConfig{Exclude: []string{"/Users/o/Projects/donat"}}}
	openCfg := &ScribeConfig{} // no filters = allow all
	ignoring := &Manifest{IgnoredPaths: []string{other}}

	cases := []struct {
		name        string
		cfg         *ScribeConfig
		manifest    *Manifest
		projectPath string
		want        bool
	}{
		{"empty path passes (no provenance)", scoped, nil, "", true},
		{"in sources.include", scoped, nil, allowed, true},
		{"not in sources.include dropped", scoped, nil, other, false},
		{"in sources.exclude dropped", excluded, nil, other, false},
		{"ignored path dropped even when sources allow all", openCfg, ignoring, other, false},
		{"allowed-but-undiscovered passes (open config, not ignored)", openCfg, &Manifest{}, allowed, true},
		{"nil cfg fails open", nil, nil, allowed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectScopeAllowed(tc.cfg, tc.manifest, tc.projectPath); got != tc.want {
				t.Errorf("projectScopeAllowed(%q) = %v, want %v", tc.projectPath, got, tc.want)
			}
		})
	}
}
