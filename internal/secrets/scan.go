package secrets

import (
	"fmt"
	"regexp"
	"strings"
)

// Finding is one credential spotted in content about to be written to git.
//
// The value is deliberately never carried: a leak report that quotes the leak
// gets copied into issue trackers and chat logs, which is how a credential that
// was caught ends up published anyway.
type Finding struct {
	Kind string
	Line int
}

func (f Finding) String() string {
	return fmt.Sprintf("%s on line %d", f.Kind, f.Line)
}

// pattern is one credential shape. Patterns are deliberately narrow.
//
// A false positive here does not warn — it refuses a commit, and a device that
// cannot commit cannot sync, which would strand the machine over a string that
// merely looked wrong. So every pattern matches a documented, prefixed token
// format and nothing generic: no "password =", no high-entropy heuristic.
type pattern struct {
	kind string
	re   *regexp.Regexp
}

var patterns = []pattern{
	{"a GitHub token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
	{"a GitHub fine-grained token", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{50,}`)},
	{"an AWS access key id", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"an Anthropic API key", regexp.MustCompile(`sk-ant-[A-Za-z0-9\-_]{24,}`)},
	{"an OpenAI API key", regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9]{32,}`)},
	{"a Slack token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"a Google API key", regexp.MustCompile(`AIza[0-9A-Za-z\-_]{35}`)},
	{"a private key", regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----`)},
	{"an age private key", regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Z]{50,}`)},
}

// Scan reports credentials found in content.
//
// Binary content is skipped rather than scanned: a NUL byte means the line
// numbers would be meaningless, and no credential format above is binary.
func Scan(content []byte) []Finding {
	if len(content) == 0 || hasNUL(content) {
		return nil
	}

	var findings []Finding
	seen := make(map[string]bool)
	for i, line := range strings.Split(string(content), "\n") {
		for _, p := range patterns {
			if !p.re.MatchString(line) {
				continue
			}
			// One finding per kind per file: a leaked key repeated on forty
			// lines is one problem to fix, not forty.
			if seen[p.kind] {
				continue
			}
			seen[p.kind] = true
			findings = append(findings, Finding{Kind: p.kind, Line: i + 1})
		}
	}
	return findings
}

// Encrypted reports whether content is age ciphertext, which is exactly what
// the state repo's secrets/ directory is supposed to contain and must never be
// mistaken for a leak.
func Encrypted(content []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(content)), "-----BEGIN AGE ENCRYPTED FILE-----")
}

func hasNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
