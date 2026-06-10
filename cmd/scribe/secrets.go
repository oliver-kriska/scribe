package main

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Secret-scan gate for team KBs. Articles are LLM-written from session
// transcripts that routinely contain credentials, and in team mode a
// commit leaves the machine — so staged wiki articles are scanned with
// a curated ruleset before they can be committed, and offending files
// are held back (per-file, never a whole-run abort: sync runs on cron
// with nobody at the terminal).
//
// Design follows the precedents researched in
// .claude/research/2026-06-10-secret-scan-approaches.md:
//   - rules are distinctive-prefix regexes taken from gitleaks/TruffleHog
//     (the same curation Claude Code's team-memory scanner ships);
//     precision over recall, zero standalone entropy detection
//   - entropy is only a per-rule confirmation gate on the two
//     contextual rules, exactly how gitleaks uses it
//   - the matched text is NEVER stored, logged, or returned — hits
//     carry rule label + file:line only
//   - `scribe:allow` (or `gitleaks:allow`) anywhere on the line
//     suppresses it; placeholder-shaped values never fire

// SecretScanConfig tunes the team-mode credential gate. The gate is ON
// by default whenever team: true — defaults chosen so the safe path
// needs zero config.
type SecretScanConfig struct {
	// Disable turns the gate off entirely. Trust-locked in team mode.
	Disable bool `yaml:"disable"`
	// Generic enables the noisy generic key=value assignment rule
	// (entropy-gated). Default off — articles legitimately say things
	// like "set api_key: in scribe.yaml" all the time.
	Generic bool `yaml:"generic"`
	// AllowPaths exempts KB-relative path globs from the gate
	// (e.g. wiki/examples). Same pattern semantics as sources filters.
	AllowPaths []string `yaml:"allow_paths"`
}

// secretRule is one detection pattern. Group selects the submatch
// holding the secret for allowlist/entropy checks (0 = whole match).
type secretRule struct {
	ID         string
	Label      string
	Re         *regexp.Regexp
	Allow      []*regexp.Regexp
	MinEntropy float64
	Group      int
	MaxLine    int  // skip longer lines; 0 = 64KB default
	Generic    bool // only active with secret_scan.generic: true
}

// secretHit is one rule firing in one file. No secret bytes — ever.
type secretHit struct {
	RuleID string
	Label  string
	Line   int
}

// reBoundary is gitleaks' end-of-token guard; works per-line.
const reBoundary = `(?:[\x60'"\s;]|\\[nr]|$)`

var secretRules = []secretRule{
	{
		ID: "aws-access-key-id", Label: "AWS Access Key ID",
		Re:    regexp.MustCompile(`\b((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16})\b`),
		Allow: []*regexp.Regexp{regexp.MustCompile(`.+EXAMPLE$`)},
	},
	{
		ID: "aws-secret-access-key", Label: "AWS Secret Access Key",
		Re: regexp.MustCompile(`(?i)\baws_?(?:secret)?_?(?:access)?_?key(?:[ \t\w.-]{0,20})['"\x60]?\s*(?:=|:|=>)\s*['"\x60]?([A-Za-z0-9/+=]{40})` + reBoundary),
	},
	{
		ID: "aws-bedrock-api-key", Label: "AWS Bedrock API Key",
		Re: regexp.MustCompile(`\b(ABSK[A-Za-z0-9+/]{109,269}={0,2})` + reBoundary),
	},
	{
		ID: "gcp-api-key", Label: "Google API Key",
		Re: regexp.MustCompile(`\b(AIza[\w-]{35})` + reBoundary),
	},
	{
		ID: "github-token", Label: "GitHub Token",
		Re: regexp.MustCompile(`\b((?:ghp|gho|ghu|ghs|ghr)_[0-9a-zA-Z]{36})\b`),
	},
	{
		ID: "github-fine-grained-pat", Label: "GitHub Fine-Grained PAT",
		Re: regexp.MustCompile(`\b(github_pat_\w{82})\b`),
	},
	{
		ID: "gitlab-token", Label: "GitLab Token",
		Re: regexp.MustCompile(`\b(gl(?:pat|dt)-[0-9a-zA-Z_\-]{20,380})`),
	},
	{
		ID: "slack-bot-token", Label: "Slack Bot Token",
		Re: regexp.MustCompile(`xoxb-[0-9]{10,13}-[0-9]{10,13}[a-zA-Z0-9-]*`),
	},
	{
		ID: "slack-user-token", Label: "Slack User Token",
		Re: regexp.MustCompile(`xox[pe](?:-[0-9]{10,13}){3}-[a-zA-Z0-9-]{28,34}`),
	},
	{
		ID: "slack-app-token", Label: "Slack App Token",
		Re: regexp.MustCompile(`(?i)xapp-\d-[A-Z0-9]+-\d+-[a-z0-9]+`),
	},
	{
		ID: "slack-webhook-url", Label: "Slack Webhook URL",
		Re: regexp.MustCompile(`hooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9+/]{43,56}`),
	},
	{
		ID: "stripe-secret-key", Label: "Stripe Secret Key",
		Re: regexp.MustCompile(`\b((?:sk|rk)_(?:test|live|prod)_[a-zA-Z0-9]{10,99})` + reBoundary),
	},
	{
		ID: "openai-api-key", Label: "OpenAI API Key",
		Re: regexp.MustCompile(`\b(sk-(?:proj|svcacct|admin)-(?:[A-Za-z0-9_-]{74}|[A-Za-z0-9_-]{58})T3BlbkFJ(?:[A-Za-z0-9_-]{74}|[A-Za-z0-9_-]{58})\b|sk-[a-zA-Z0-9]{20}T3BlbkFJ[a-zA-Z0-9]{20})` + reBoundary),
	},
	{
		ID: "anthropic-api-key", Label: "Anthropic API Key",
		Re: regexp.MustCompile(`\b(sk-ant-(?:api03|admin01)-[a-zA-Z0-9_\-]{93}AA)` + reBoundary),
	},
	{
		ID: "huggingface-token", Label: "Hugging Face Token",
		Re: regexp.MustCompile(`\b(hf_[a-zA-Z]{34})` + reBoundary),
	},
	{
		ID: "groq-api-key", Label: "Groq API Key",
		Re: regexp.MustCompile(`\b(gsk_[a-zA-Z0-9]{52})\b`),
	},
	{
		ID: "xai-api-key", Label: "xAI API Key",
		Re: regexp.MustCompile(`\b(xai-[0-9a-zA-Z_]{80})\b`),
	},
	{
		ID: "perplexity-api-key", Label: "Perplexity API Key",
		Re: regexp.MustCompile(`\b(pplx-[a-zA-Z0-9]{48})(?:[\x60'"\s;]|\\[nr]|$|\b)`),
	},
	{
		ID: "npm-access-token", Label: "npm Access Token",
		Re: regexp.MustCompile(`(?i)\b(npm_[a-z0-9]{36})` + reBoundary),
	},
	{
		ID: "pypi-upload-token", Label: "PyPI Upload Token",
		Re: regexp.MustCompile(`pypi-AgEIcHlwaS5vcmc[\w-]{50,1000}`),
	},
	{
		ID: "sendgrid-api-key", Label: "SendGrid API Key",
		Re: regexp.MustCompile(`\b(SG\.[a-zA-Z0-9=_\-.]{66})` + reBoundary),
	},
	{
		ID: "twilio-api-key", Label: "Twilio API Key",
		Re: regexp.MustCompile(`\bSK[0-9a-fA-F]{32}\b`),
	},
	{
		ID: "private-key-pem", Label: "Private Key (PEM)",
		Re: regexp.MustCompile(`(?i)-----BEGIN[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----`),
	},
	{
		ID: "jwt", Label: "JSON Web Token",
		Re: regexp.MustCompile(`\b(ey[a-zA-Z0-9]{17,}\.ey[a-zA-Z0-9/\\_-]{17,}\.(?:[a-zA-Z0-9/\\_-]{10,}={0,2})?)` + reBoundary),
	},

	// Tier 2 — contextual rules, guarded by placeholder filters and
	// (for azure) an entropy floor, mirroring gitleaks' own guards.
	{
		ID: "url-userinfo-password", Label: "Password in URL",
		Re:    regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]{1,15})://([^\s:/@'"\x60]{1,64}):([^\s@'"\x60]{3,})@[a-zA-Z0-9.%-]+`),
		Group: 3, MaxLine: 2048,
	},
	{
		ID: "azure-ad-client-secret", Label: "Azure AD Client Secret",
		Re:         regexp.MustCompile(`(?:^|[\\'"\x60\s>=:(,)])([a-zA-Z0-9_~.]{3}\dQ~[a-zA-Z0-9_~.-]{31,34})(?:$|[\\'"\x60\s<),])`),
		MinEntropy: 3.0, MaxLine: 2048,
	},

	// Optional generic assignment rule (secret_scan.generic: true).
	// gitleaks needs ~400 stopwords to keep this rule sane, and a KB
	// whose articles constantly say "set api_key: in scribe.yaml" makes
	// it worse — default off.
	{
		ID: "generic-credential-assignment", Label: "Generic Credential Assignment",
		Re:         regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|passw(?:or)?d|credential)[\w .-]{0,20}[\s'"]{0,3}(?:=|:{1,2}=?|=>)[\s'"\x60=]{0,5}([\w.=+/-]{16,128})` + reBoundary),
		Allow:      []*regexp.Regexp{regexp.MustCompile(`^[a-zA-Z_.-]+$`)}, // no digits → never a machine secret
		MinEntropy: 3.5, MaxLine: 2048, Generic: true,
	},
}

// secretPlaceholderRes filter the CAPTURED GROUP (gitleaks
// regexTarget="secret" semantics): obvious placeholders never fire.
var secretPlaceholderRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:x{3,}|\*{3,}|\.{3,}|0{8,}|(?:your|my|sample|test|dummy|fake|example|placeholder|changeme|redacted|todo)[\w-]*)$`),
	regexp.MustCompile(`^<[^>]{1,64}>$`),                   // <your-api-key>
	regexp.MustCompile(`^\$\{?[A-Za-z_][A-Za-z0-9_]*\}?$`), // $API_KEY / ${API_KEY}
	regexp.MustCompile(`^\{\{[ \t]*[\w ().|]+[ \t]*\}\}$`), // {{ template }}
}

// secretStopwords are exact known-fake values (the canonical AWS doc
// credentials git-secrets ships as built-in alloweds).
var secretStopwords = map[string]bool{
	"AKIAIOSFODNN7EXAMPLE":                     true,
	"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY": true,
}

const defaultSecretMaxLine = 64 * 1024

// scanContentForSecrets runs the ruleset over content line-by-line.
// One hit per rule per content (dedupe like Claude Code's scanner) at
// the first offending line. The matched bytes never leave this
// function.
func scanContentForSecrets(content []byte, includeGeneric bool) []secretHit {
	var hits []secretHit
	fired := map[string]bool{}
	allowMarkers := [][]byte{[]byte("scribe:allow"), []byte("gitleaks:allow")}

	lineNo := 0
	for start := 0; start < len(content); {
		end := bytes.IndexByte(content[start:], '\n')
		var line []byte
		if end < 0 {
			line = content[start:]
			start = len(content)
		} else {
			line = content[start : start+end]
			start += end + 1
		}
		lineNo++
		if len(line) == 0 {
			continue
		}
		skip := false
		for _, m := range allowMarkers {
			if bytes.Contains(line, m) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		for i := range secretRules {
			r := &secretRules[i]
			if r.Generic && !includeGeneric {
				continue
			}
			if fired[r.ID] {
				continue
			}
			maxLine := r.MaxLine
			if maxLine == 0 {
				maxLine = defaultSecretMaxLine
			}
			if len(line) > maxLine {
				continue
			}
			m := r.Re.FindSubmatch(line)
			if m == nil {
				continue
			}
			if secretValueAllowed(secretFromMatch(m, r.Group), r) {
				continue
			}
			fired[r.ID] = true
			hits = append(hits, secretHit{RuleID: r.ID, Label: r.Label, Line: lineNo})
		}
	}
	return hits
}

// secretFromMatch extracts the secret submatch for filtering.
func secretFromMatch(m [][]byte, group int) []byte {
	idx := group
	if idx == 0 && len(m) > 1 && m[1] != nil {
		idx = 1
	}
	if idx < len(m) && m[idx] != nil {
		return m[idx]
	}
	return m[0]
}

// secretValueAllowed reports whether the captured value is a known
// placeholder/stopword, matches a per-rule allowlist, or fails the
// rule's entropy floor.
func secretValueAllowed(secret []byte, r *secretRule) bool {
	s := string(secret)
	if secretStopwords[s] {
		return true
	}
	for _, re := range secretPlaceholderRes {
		if re.MatchString(s) {
			return true
		}
	}
	for _, re := range r.Allow {
		if re.MatchString(s) {
			return true
		}
	}
	if r.MinEntropy > 0 && shannonEntropy(s) < r.MinEntropy {
		return true
	}
	return false
}

// shannonEntropy is bits-per-character entropy, gitleaks-style.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[rune]int{}
	total := 0
	for _, r := range s {
		counts[r]++
		total++
	}
	var e float64
	for _, c := range counts {
		p := float64(c) / float64(total)
		e -= p * math.Log2(p)
	}
	return e
}

// holdSecretFiles is the commit gate: scan the STAGED KB markdown
// and unstage any file with a hit, with a loud (secret-free) log line
// per finding. Per-file hold, never a whole-run abort — sync runs on
// cron and one quoted token must not wedge the pipeline. Held files
// stay dirty in the working tree, so doctor keeps flagging them until
// a human resolves (rewrite the line or add `scribe:allow`). Active
// only for team KBs; secret_scan.disable: false is trust-locked, so a
// pushed config change can't switch the gate off.
func holdSecretFiles(root string, cfg *ScribeConfig) {
	if cfg == nil || !cfg.Team || cfg.SecretScan.Disable {
		return
	}
	for _, rel := range stagedKBMarkdown(root) {
		if secretScanPathExempt(cfg, rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		hits := scanContentForSecrets(data, cfg.SecretScan.Generic)
		if len(hits) == 0 {
			continue
		}
		// `git reset -q --` rather than `restore --staged`: identical on a
		// normal repo, but it also works on an unborn branch (fresh KB
		// before its first commit), where restore can't resolve HEAD.
		if _, err := runCmdErr(root, "git", "reset", "-q", "--", rel); err != nil {
			logMsg("git", "SECRET HELD: %s — unstage failed (%v), commit may include it; resolve immediately", rel, err)
			continue
		}
		for _, h := range hits {
			logMsg("git", "SECRET HELD: %s:%d [%s] — file held back from commit; rewrite the line or add 'scribe:allow' if it's a placeholder", rel, h.Line, h.Label)
		}
	}
}

// stagedKBMarkdown lists staged .md files under the wiki content dirs
// AND raw/ (added/copied/modified). raw/ matters: `scribe commit`
// stages everything except output/, and URL absorbs + inbox ingests
// land under raw/articles — without it a credential inside absorbed
// content would commit unscanned.
func stagedKBMarkdown(root string) []string {
	args := make([]string, 0, 6+len(wikiDirs))
	args = append(args, "diff", "--cached", "--name-only", "--diff-filter=ACM", "--")
	args = append(args, wikiDirs...)
	args = append(args, "raw")
	out := runCmd(root, "git", args...)
	if out == "" {
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		if l := strings.TrimSpace(line); l != "" && strings.HasSuffix(l, ".md") {
			files = append(files, l)
		}
	}
	return files
}

// secretScanPathExempt applies secret_scan.allow_paths (KB-relative
// globs; same pattern semantics as sources filters).
func secretScanPathExempt(cfg *ScribeConfig, rel string) bool {
	for _, pattern := range cfg.SecretScan.AllowPaths {
		if sourcePatternMatches(pattern, rel) {
			return true
		}
	}
	return false
}

// findSecretsInKB scans all KB articles (wiki dirs + raw/articles) for
// doctor — committed leaks AND held-back files both live on disk.
func findSecretsInKB(root string, includeGeneric bool) []string {
	var findings []string
	record := func(path string, content []byte) error {
		rel := relPath(root, path)
		for _, h := range scanContentForSecrets(content, includeGeneric) {
			findings = append(findings, rel+":"+strconv.Itoa(h.Line)+" ["+h.Label+"]")
		}
		return nil
	}
	_ = walkAllMarkdown(root, record)
	rawDir := filepath.Join(root, "raw", "articles")
	_ = filepath.Walk(rawDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		content, rerr := os.ReadFile(path) //nolint:gosec // user-supplied KB root, deliberate walk
		if rerr != nil {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		return record(path, content)
	})
	return findings
}
