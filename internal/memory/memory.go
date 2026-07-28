// Package memory implements the three-tier memory model (DESIGN.md §8).
//
//	scratch      per-device working notes, expire after a TTL
//	long-term    curated durable facts, promoted from scratch
//
// Files in the state repo:
//
//	memory/scratch/<node>/<id>.md   device-owned, never conflicts, TTL'd
//	memory/long-term/<id>.md        shared, one file per fact
//	memory/journal/<node>.jsonl     device-owned record of what was remembered
//
// One file per memory rather than one big store: two devices adding different
// facts then touch different paths and never collide, which is what lets
// long-term memory be shared without a merge strategy.
//
// Decay is the point. Without expiry the repo grows without bound and injecting
// it into a session degrades context instead of improving it, so scratch that
// is never promoted goes away on its own.
package memory

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Tiers. Task-scoped memory is expressed as scratch tagged with a task rather
// than as a third directory: it has the same owner, the same conflict
// behaviour, and the same reason to expire.
const (
	TierScratch  = "scratch"
	TierLongTerm = "long-term"
)

// DefaultTTL is how long unpromoted scratch survives.
const DefaultTTL = 7 * 24 * time.Hour

// ErrNotFound is returned when no memory has the given id.
var ErrNotFound = errors.New("memory not found")

// Entry is one remembered fact.
type Entry struct {
	ID      string    `json:"id"`
	Tier    string    `json:"tier"`
	Text    string    `json:"text"`
	Tags    []string  `json:"tags,omitempty"`
	Task    string    `json:"task,omitempty"`
	Node    string    `json:"node"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Expires is set on scratch only. A long-term memory has no expiry, which
	// is the whole difference between the tiers.
	Expires time.Time `json:"expires,omitempty"`
}

// ScratchDir is one device's own working memory.
func ScratchDir(repoPath, nodeID string) string {
	return filepath.Join(repoPath, "memory", "scratch", nodeID)
}

// LongTermDir holds curated memory shared by the whole fleet.
func LongTermDir(repoPath string) string {
	return filepath.Join(repoPath, "memory", "long-term")
}

// ScratchPattern matches every scratch file a device owns.
func ScratchPattern(nodeID string) string {
	return filepath.Join("memory", "scratch", nodeID, "*.md")
}

// JournalPattern matches a device's own memory journal.
func JournalPattern(nodeID string) string {
	return filepath.Join("memory", "journal", nodeID+".jsonl")
}

// Path is where an entry lives, which depends on its tier: scratch is filed
// under the device that wrote it, long-term is shared.
func (e *Entry) Path(repoPath string) string {
	if e.Tier == TierLongTerm {
		return filepath.Join(LongTermDir(repoPath), e.ID+".md")
	}
	return filepath.Join(ScratchDir(repoPath, e.Node), e.ID+".md")
}

// Expired reports whether this entry has outlived its TTL.
func (e *Entry) Expired(now time.Time) bool {
	return !e.Expires.IsZero() && now.After(e.Expires)
}

// Matches reports whether a query appears in the entry's text, tags, task, or
// id. Substring rather than token matching: memories are short, and someone
// searching "dkms" should find "evdi-dkms".
func (e *Entry) Matches(query string) bool {
	if query == "" {
		return true
	}
	q := strings.ToLower(query)
	if strings.Contains(strings.ToLower(e.Text), q) ||
		strings.Contains(strings.ToLower(e.ID), q) ||
		strings.Contains(strings.ToLower(e.Task), q) {
		return true
	}
	for _, tag := range e.Tags {
		if strings.Contains(strings.ToLower(tag), q) {
			return true
		}
	}
	return false
}

// Summary is a one-line rendering for listings.
func (e *Entry) Summary(width int) string {
	text := strings.ReplaceAll(e.Text, "\n", " ")
	if width > 0 && len(text) > width {
		text = text[:width-1] + "…"
	}
	return text
}

// slugMaxLen bounds a generated id so paths stay reasonable everywhere.
const slugMaxLen = 48

// Slug derives a stable, filesystem-safe id from the memory's own text.
//
// Deriving rather than requiring one keeps `nimbus memory add "..."` a
// single-argument command, which matters because the thing writing most
// memories is Claude, mid-session, non-interactively.
func Slug(text string) string {
	var b strings.Builder
	var lastDash bool

	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case b.Len() > 0 && !lastDash:
			b.WriteRune('-')
			lastDash = true
		}
		if b.Len() >= slugMaxLen {
			break
		}
	}

	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "note"
	}
	return slug
}

// ValidateID rejects ids that would escape the memory directory.
func ValidateID(id string) error {
	if id == "" {
		return errors.New("memory id is empty")
	}
	if len(id) > 64 {
		return fmt.Errorf("memory id %q is longer than 64 characters", id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("memory id %q is reserved", id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("memory id %q: use lowercase letters, digits, - and _ only", id)
		}
	}
	return nil
}
