package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Markdown with a small frontmatter block, rather than JSON, because these
// files are read and written by Claude and by people. The parser below is
// deliberately not YAML: the format is ours, the keys are known, and a real
// YAML dependency would buy nothing.
const delimiter = "---"

// Encode renders an entry as the file that gets committed.
func (e *Entry) Encode() []byte {
	var b strings.Builder
	b.WriteString(delimiter + "\n")
	fmt.Fprintf(&b, "id: %s\n", e.ID)
	fmt.Fprintf(&b, "tier: %s\n", e.Tier)
	fmt.Fprintf(&b, "node: %s\n", e.Node)
	if e.Task != "" {
		fmt.Fprintf(&b, "task: %s\n", e.Task)
	}
	if len(e.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(e.Tags, ", "))
	}
	fmt.Fprintf(&b, "created: %s\n", e.Created.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "updated: %s\n", e.Updated.UTC().Format(time.RFC3339))
	if !e.Expires.IsZero() {
		fmt.Fprintf(&b, "expires: %s\n", e.Expires.UTC().Format(time.RFC3339))
	}
	b.WriteString(delimiter + "\n\n")
	b.WriteString(strings.TrimSpace(e.Text) + "\n")
	return []byte(b.String())
}

// Decode parses a memory file. A file that has been hand-edited into something
// unparseable is reported rather than silently treated as empty.
func Decode(data []byte) (*Entry, error) {
	text := string(data)
	if !strings.HasPrefix(text, delimiter) {
		return nil, errors.New("missing frontmatter")
	}

	rest := strings.TrimPrefix(text, delimiter)
	rest = strings.TrimPrefix(rest, "\n")
	head, body, found := strings.Cut(rest, "\n"+delimiter)
	if !found {
		return nil, errors.New("unterminated frontmatter")
	}

	e := &Entry{Text: strings.TrimSpace(body)}
	for _, line := range strings.Split(head, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)

		switch key {
		case "id":
			e.ID = value
		case "tier":
			e.Tier = value
		case "node":
			e.Node = value
		case "task":
			e.Task = value
		case "tags":
			for _, tag := range strings.Split(value, ",") {
				if tag = strings.TrimSpace(tag); tag != "" {
					e.Tags = append(e.Tags, tag)
				}
			}
		case "created", "updated", "expires":
			ts, err := time.Parse(time.RFC3339, value)
			if err != nil {
				continue
			}
			switch key {
			case "created":
				e.Created = ts
			case "updated":
				e.Updated = ts
			case "expires":
				e.Expires = ts
			}
		}
	}

	if e.ID == "" {
		return nil, errors.New("frontmatter has no id")
	}
	return e, nil
}

// Add writes a new memory, giving it a unique id derived from its text.
func Add(repoPath string, e *Entry, ttl time.Duration) (*Entry, error) {
	if strings.TrimSpace(e.Text) == "" {
		return nil, errors.New("a memory needs some text")
	}
	if e.Tier == "" {
		e.Tier = TierScratch
	}
	if e.Tier != TierScratch && e.Tier != TierLongTerm {
		return nil, fmt.Errorf("unknown tier %q (use scratch or long-term)", e.Tier)
	}

	if e.ID == "" {
		e.ID = uniqueID(repoPath, e, Slug(e.Text))
	}
	if err := ValidateID(e.ID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	e.Created, e.Updated = now, now
	if e.Tier == TierScratch && ttl > 0 {
		e.Expires = now.Add(ttl)
	}
	return e, write(repoPath, e)
}

// uniqueID appends a counter when a slug is already taken, so two similar
// notes on the same day do not overwrite each other.
func uniqueID(repoPath string, e *Entry, base string) string {
	candidate := base
	probe := *e
	for i := 2; i < 100; i++ {
		probe.ID = candidate
		if _, err := os.Stat(probe.Path(repoPath)); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}

func write(repoPath string, e *Entry) error {
	path := e.Path(repoPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("save memory: %w", err)
	}
	if err := os.WriteFile(path, e.Encode(), 0o644); err != nil {
		return fmt.Errorf("save memory: %w", err)
	}
	return nil
}

// Load finds one memory by id, in either tier.
func Load(repoPath, nodeID, id string) (*Entry, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	for _, path := range []string{
		filepath.Join(LongTermDir(repoPath), id+".md"),
		filepath.Join(ScratchDir(repoPath, nodeID), id+".md"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return Decode(data)
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Query narrows a listing.
type Query struct {
	Tier string
	Task string
	Text string
	// Node limits scratch to one device. Empty means every device's scratch,
	// which is what a fleet-wide listing wants.
	Node string
	// IncludeExpired surfaces entries past their TTL that have not been swept
	// yet, so `memory list` can explain a file the user can still see on disk.
	IncludeExpired bool
}

// List returns matching memories, newest first.
//
// One unreadable file must not hide the rest, matching how profiles and tasks
// already behave.
func List(repoPath string, q Query) ([]*Entry, error) {
	var patterns []string
	if q.Tier != TierScratch {
		patterns = append(patterns, filepath.Join(LongTermDir(repoPath), "*.md"))
	}
	if q.Tier != TierLongTerm {
		node := q.Node
		if node == "" {
			node = "*"
		}
		patterns = append(patterns, filepath.Join(repoPath, "memory", "scratch", node, "*.md"))
	}

	now := time.Now().UTC()
	var entries []*Entry
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, path := range matches {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			e, err := Decode(data)
			if err != nil {
				continue
			}
			if !q.IncludeExpired && e.Expired(now) {
				continue
			}
			if q.Task != "" && e.Task != q.Task {
				continue
			}
			if !e.Matches(q.Text) {
				continue
			}
			entries = append(entries, e)
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Updated.After(entries[j].Updated)
	})
	return entries, nil
}

// Promote moves a scratch memory into long-term, where it stops expiring.
//
// This is the deliberate step the design turns on: everything else about
// scratch is designed to disappear, so keeping something has to be a choice
// somebody made.
func Promote(repoPath, nodeID, id string) (*Entry, error) {
	e, err := Load(repoPath, nodeID, id)
	if err != nil {
		return nil, err
	}
	if e.Tier == TierLongTerm {
		return e, nil
	}

	old := e.Path(repoPath)
	e.Tier = TierLongTerm
	e.Expires = time.Time{}
	e.Updated = time.Now().UTC()

	if err := write(repoPath, e); err != nil {
		return nil, err
	}
	// Remove the scratch copy only after the durable one is on disk, so a
	// crash between the two loses nothing.
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("promote: %w", err)
	}
	return e, nil
}

// Forget deletes a memory.
func Forget(repoPath, nodeID, id string) (*Entry, error) {
	e, err := Load(repoPath, nodeID, id)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(e.Path(repoPath)); err != nil {
		return nil, fmt.Errorf("forget: %w", err)
	}
	return e, nil
}

// Expire sweeps this device's own expired scratch and reports what went.
//
// Only this device's scratch: those files are owned, so deleting them cannot
// race another machine's writes. Another device's expired scratch is its own
// to sweep.
func Expire(repoPath, nodeID string) ([]*Entry, error) {
	entries, err := List(repoPath, Query{
		Tier: TierScratch, Node: nodeID, IncludeExpired: true,
	})
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var swept []*Entry
	for _, e := range entries {
		if !e.Expired(now) {
			continue
		}
		if err := os.Remove(e.Path(repoPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return swept, fmt.Errorf("expire: %w", err)
		}
		swept = append(swept, e)
	}
	return swept, nil
}
