package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	requiredFields  = []string{"title", "type", "created", "updated", "domain", "confidence", "tags", "related", "sources"}
	validTypes      = map[string]bool{"project": true, "tool": true, "person": true, "decision": true, "pattern": true, "solution": true, "research": true}
	validConfidence = map[string]bool{"high": true, "medium": true, "low": true}
	validAuthority  = map[string]bool{"canonical": true, "contextual": true, "opinion": true}

	// validDomainsOverride lets tests inject a fixed domain set without
	// constructing a real scribe.yaml. When non-nil, validDomainsForRoot
	// returns it verbatim.
	validDomainsOverride map[string]bool

	typeFields = map[string]map[string]map[string]bool{
		"project":  {"status": {"active": true, "paused": true, "completed": true, "idea": true}},
		"tool":     {"verdict": {"use": true, "evaluate": true, "skip": true}},
		"decision": {"status": {"decided": true, "reconsidering": true, "superseded": true}},
		"research": {"status": {"active": true, "completed": true, "stale": true}, "depth": {"shallow": true, "moderate": true, "deep": true}},
	}

	dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// validDomainsForRoot returns the permitted domain set, derived from
// scribe.yaml at `root` (config `domains:` list plus the universal
// "personal" / "general" fallbacks). Tests can preempt the lookup by
// setting validDomainsOverride. An empty root falls back to universals
// only — enough for unit tests that pass an ad-hoc file path.
func validDomainsForRoot(root string) map[string]bool {
	if validDomainsOverride != nil {
		return validDomainsOverride
	}
	var domains []string
	if root != "" {
		domains = loadConfig(root).AllDomains()
	} else {
		domains = universalDomains
	}
	m := make(map[string]bool, len(domains))
	for _, d := range domains {
		m[d] = true
	}
	return m
}

type ValidateCmd struct {
	Files []string `arg:"" optional:"" help:"Files to validate. If empty, validates all wiki articles."`
}

func (v *ValidateCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	var files []string
	if len(v.Files) > 0 {
		files = v.Files
	} else {
		// Collect all wiki .md files
		err := walkArticles(root, func(path string, _ []byte) error {
			files = append(files, path)
			return nil
		})
		if err != nil {
			return err
		}
	}

	failed := 0
	for _, path := range files {
		errs := validateFile(root, path)
		if len(errs) > 0 {
			failed++
			for _, e := range errs {
				fmt.Printf("  %s: %s\n", relPath(root, path), e)
			}
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d file(s) with frontmatter errors", failed)
	}
	return nil
}

func validateFile(root, path string) []string {
	// Skip raw files
	if strings.Contains(path, "/raw/") || strings.HasPrefix(path, "raw/") {
		return nil
	}
	if !strings.HasSuffix(path, ".md") {
		return nil
	}
	// Skip meta files
	base := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		base = path[idx+1:]
	}
	if skipFiles[base] {
		return nil
	}
	// Underscore-prefixed files (_index.md, _hot.md) are generated meta files,
	// not articles. walkArticles skips them; mirror that for direct-path paths
	// reached via --changed or the pre-commit hook.
	if strings.HasPrefix(base, "_") {
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("cannot read: %v", err)}
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return []string{"empty file"}
	}

	raw, err := parseFrontmatterRaw(content)
	if err != nil {
		return []string{"missing or invalid YAML frontmatter"}
	}
	fm, err := parseFrontmatter(content)
	if err != nil {
		return []string{"missing or invalid YAML frontmatter"}
	}

	var errs []string

	// Check required fields
	var missing []string
	for _, f := range requiredFields {
		if _, ok := raw[f]; !ok {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		errs = append(errs, fmt.Sprintf("missing required fields: %s", strings.Join(missing, ", ")))
	}

	// Validate type
	if fm.Type != "" && !validTypes[fm.Type] {
		keys := sortedKeys(validTypes)
		errs = append(errs, fmt.Sprintf("invalid type: '%s' (expected: %s)", fm.Type, strings.Join(keys, ", ")))
	}

	// Validate domain against the KB's configured + universal domain set.
	domains := validDomainsForRoot(root)
	if fm.Domain != "" && !domains[fm.Domain] {
		keys := sortedKeys(domains)
		errs = append(errs, fmt.Sprintf("invalid domain: '%s' (expected: %s)", fm.Domain, strings.Join(keys, ", ")))
	}

	// Validate confidence
	if fm.Confidence != "" && !validConfidence[fm.Confidence] {
		keys := sortedKeys(validConfidence)
		errs = append(errs, fmt.Sprintf("invalid confidence: '%s' (expected: %s)", fm.Confidence, strings.Join(keys, ", ")))
	}

	// Validate authority (optional field — unset is fine)
	if fm.Authority != "" && !validAuthority[fm.Authority] {
		keys := sortedKeys(validAuthority)
		errs = append(errs, fmt.Sprintf("invalid authority: '%s' (expected: %s)", fm.Authority, strings.Join(keys, ", ")))
	}

	// Validate dates. Go's YAML parser auto-converts YYYY-MM-DD to time.Time,
	// so a time.Time value is already valid; only string values need regex
	// checking (they arrived as quoted YAML scalars).
	for _, field := range []string{"created", "updated"} {
		v, ok := raw[field]
		if !ok {
			continue
		}
		if _, isTime := v.(time.Time); isTime {
			continue
		}
		if s, isString := v.(string); isString {
			if !dateRE.MatchString(s) {
				errs = append(errs, fmt.Sprintf("%s not in YYYY-MM-DD format: '%s'", field, s))
			}
			continue
		}
		errs = append(errs, fmt.Sprintf("%s not in YYYY-MM-DD format: '%v'", field, v))
	}

	// Validate type-specific fields
	if fields, ok := typeFields[fm.Type]; ok {
		for field, validValues := range fields {
			if v, ok := raw[field]; ok {
				s := fmt.Sprint(v)
				if !validValues[s] {
					keys := sortedKeys(validValues)
					errs = append(errs, fmt.Sprintf("invalid %s: '%s' for type '%s' (expected: %s)", field, s, fm.Type, strings.Join(keys, ", ")))
				}
			}
		}
	}

	// Check list fields
	for _, field := range []string{"tags", "related", "sources"} {
		if v, ok := raw[field]; ok {
			if _, isList := v.([]any); !isList {
				errs = append(errs, fmt.Sprintf("%s should be a list, got: %T", field, v))
			}
		}
	}

	// Check title non-empty
	if _, ok := raw["title"]; ok && fm.Title == "" {
		errs = append(errs, "title is empty")
	}

	return errs
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
