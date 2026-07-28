// Package system journals OS modifications with the command that undoes them.
//
// Invariant 4 (DESIGN.md §12): every system change is journaled with a rollback
// *before* it is applied. The ordering is the whole point. A change recorded
// after the fact is a change that is missing exactly when the machine failed to
// come back — and since nimbus exists partly to fix OS problems unattended, the
// person reading this journal is often someone who did not watch it happen.
//
// The file is append-only and device-owned:
//
//	system/<node>.changes.jsonl
//
// A change's status is updated by appending another record with the same id, not
// by rewriting the first. Rewriting would break both the append-only property
// and the sync model that depends on it.
package system

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Status values for a change.
const (
	StatusPlanned    = "planned"
	StatusApplied    = "applied"
	StatusFailed     = "failed"
	StatusRolledBack = "rolled-back"
)

// ErrNotFound is returned when no change exists with the given id.
var ErrNotFound = errors.New("change not found")

// Change is one modification to a machine.
type Change struct {
	ID     string    `json:"id"`
	Node   string    `json:"node"`
	At     time.Time `json:"at"`
	Status string    `json:"status"`
	// Kind is a label for the reader: package, service, config, command.
	Kind    string `json:"kind,omitempty"`
	Target  string `json:"target,omitempty"`
	Command string `json:"command"`
	// Rollback undoes Command. Empty is only valid alongside Irreversible.
	Rollback     string `json:"rollback,omitempty"`
	Irreversible bool   `json:"irreversible,omitempty"`
	// Reason explains an irreversible change, so "no rollback" is visibly a
	// decision somebody made rather than a field somebody forgot.
	Reason string `json:"reason,omitempty"`
	// Task ties the change to the work that caused it.
	Task   string `json:"task,omitempty"`
	Output string `json:"output,omitempty"`
	Err    string `json:"err,omitempty"`
}

// Applied reports whether the change is currently in effect.
func (c Change) Applied() bool { return c.Status == StatusApplied }

// Summary is the one-line form used in listings.
func (c Change) Summary() string {
	undo := c.Rollback
	if undo == "" {
		undo = "irreversible"
		if c.Reason != "" {
			undo += ": " + c.Reason
		}
	}
	return fmt.Sprintf("%s  %-11s %s  (undo: %s)", c.ID, c.Status, c.Command, undo)
}

// File is a device's change journal inside the state repo.
func File(repoPath, nodeID string) string {
	return filepath.Join(repoPath, "system", nodeID+".changes.jsonl")
}

// Pattern matches the journals this device owns, for the sync layer.
func Pattern(nodeID string) string {
	return filepath.Join("system", nodeID+".changes.jsonl")
}

// NewID returns a time-ordered identifier, so a listing sorted by id is also
// sorted by when the change happened.
func NewID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// Append records a change. Every state transition goes through here, including
// the initial plan.
func Append(repoPath, nodeID string, c Change) error {
	if c.ID == "" {
		return errors.New("change has no id")
	}
	c.Node = nodeID
	if c.At.IsZero() {
		c.At = time.Now().UTC()
	}
	if c.Status == "" {
		c.Status = StatusPlanned
	}

	path := File(repoPath, nodeID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("journal change: %w", err)
	}
	line, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("journal change: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("journal change: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("journal change: %w", err)
	}
	// Durability matters more than speed here: the record has to survive the
	// machine not coming back from the change it is about to describe.
	return f.Sync()
}

// History returns every change known to the fleet, newest first, with each
// change folded to its most recent record.
func History(repoPath string) ([]Change, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, "system", "*.changes.jsonl"))
	if err != nil {
		return nil, err
	}

	latest := make(map[string]Change)
	for _, path := range matches {
		records, err := read(path)
		if err != nil {
			// One unreadable device's journal must not hide the rest.
			continue
		}
		for _, c := range records {
			// Records are appended in order, so later ones win. Output and
			// rollback are carried forward: a "failed" record written by a
			// crashed run would otherwise drop how to undo the change.
			if prev, ok := latest[c.ID]; ok {
				c = merge(prev, c)
			}
			latest[c.ID] = c
		}
	}

	changes := make([]Change, 0, len(latest))
	for _, c := range latest {
		changes = append(changes, c)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].ID > changes[j].ID })
	return changes, nil
}

// Load finds one change by id, or by an unambiguous prefix of one.
func Load(repoPath, id string) (*Change, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("no change id given")
	}
	changes, err := History(repoPath)
	if err != nil {
		return nil, err
	}

	var matched []Change
	for _, c := range changes {
		if c.ID == id {
			return &c, nil
		}
		if strings.HasPrefix(c.ID, id) {
			matched = append(matched, c)
		}
	}
	switch len(matched) {
	case 0:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	case 1:
		return &matched[0], nil
	default:
		ids := make([]string, 0, len(matched))
		for _, c := range matched {
			ids = append(ids, c.ID)
		}
		return nil, fmt.Errorf("%q matches %s", id, strings.Join(ids, ", "))
	}
}

// merge carries forward fields a later record does not restate.
func merge(prev, next Change) Change {
	if next.Command == "" {
		next.Command = prev.Command
	}
	if next.Rollback == "" {
		next.Rollback = prev.Rollback
	}
	if next.Kind == "" {
		next.Kind = prev.Kind
	}
	if next.Target == "" {
		next.Target = prev.Target
	}
	if next.Task == "" {
		next.Task = prev.Task
	}
	if next.Reason == "" {
		next.Reason = prev.Reason
	}
	if !next.Irreversible {
		next.Irreversible = prev.Irreversible
	}
	return next
}

func read(path string) ([]Change, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var changes []Change
	scanner := bufio.NewScanner(f)
	// Records carry command output, so lines can be long.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var c Change
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			// A truncated final line is what a machine dying mid-write leaves
			// behind. Everything before it is still evidence.
			continue
		}
		changes = append(changes, c)
	}
	return changes, scanner.Err()
}
