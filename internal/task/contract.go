package task

import (
	"path/filepath"
	"strings"
)

// Contract is what a task is allowed to do when it comes back after a reboot.
//
// It exists because a task that triggered a reboot was, by definition, doing
// something invasive (DESIGN.md §10). Waking up with full latitude on a machine
// whose state you have not re-checked is how an unattended fix becomes an
// unattended outage.
type Contract struct {
	// OnBoot marks the task for pickup when the machine comes back.
	OnBoot bool `json:"on_boot,omitempty"`
	// AllowedFirst are the commands that re-establish state before anything
	// else runs. Nimbus runs these itself and hands the output to the session,
	// which is what makes "only these may run first" enforceable rather than
	// merely instructed.
	AllowedFirst []string `json:"allowed_first,omitempty"`
	// RequireHuman are patterns that always block for confirmation, at every
	// autonomy level including L3.
	RequireHuman []string `json:"require_human,omitempty"`
}

// NeedsHuman reports whether a command matches a require_human pattern,
// returning the pattern that matched.
//
// Matching is deliberately generous — glob against the whole command, glob
// against the program name, and plain substring — because the cost of an
// unnecessary confirmation prompt is a moment of a person's attention, and the
// cost of a missed one is the thing the contract existed to prevent.
func (c *Contract) NeedsHuman(command string) (string, bool) {
	if c == nil || strings.TrimSpace(command) == "" {
		return "", false
	}

	command = strings.TrimSpace(command)
	head := ""
	if fields := strings.Fields(command); len(fields) > 0 {
		head = filepath.Base(fields[0])
	}

	for _, pattern := range c.RequireHuman {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if glob(pattern, command) || glob(pattern, head) || strings.Contains(command, pattern) {
			return pattern, true
		}
	}
	return "", false
}

// glob matches a wildcard pattern against a command.
//
// Deliberately not filepath.Match: there, `*` stops at a path separator, so the
// obvious pattern `rm -rf *` would fail to match `rm -rf /var/lib/postgresql` —
// the exact command it was written to catch. A command line is not a path, and
// treating it as one turns a safety rule into a rule that quietly does nothing.
func glob(pattern, s string) bool {
	// Iterative rather than recursive so a pattern of many wildcards against a
	// long command cannot go quadratic on backtracking.
	var pi, si, star, mark int
	star = -1

	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			// Backtrack: let the last star swallow one more character.
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// ResumesOnBoot reports whether this task should be picked up after a reboot.
func (t *Task) ResumesOnBoot() bool {
	return t.Resume != nil && t.Resume.OnBoot && t.Status == StatusActive
}

// Contract returns the task's resume contract, never nil, so callers can ask it
// questions without a guard at every site.
func (t *Task) Contract() *Contract {
	if t == nil || t.Resume == nil {
		return &Contract{}
	}
	return t.Resume
}

// BootTask finds the task this device should pick up after a reboot: the
// active task it holds that asked to be resumed.
func BootTask(repoPath, nodeID string) (*Task, error) {
	tasks, err := List(repoPath)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.HeldBy(nodeID) && t.ResumesOnBoot() {
			return t, nil
		}
	}
	return nil, ErrNotFound
}
