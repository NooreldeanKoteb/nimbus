package task

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Event kinds. These are labels for a human reading the timeline, not a state
// machine — the task's own Status field is the authority on where it stands.
const (
	KindNote    = "note"
	KindClaim   = "claim"
	KindRelease = "release"
	KindHandoff = "handoff"
	KindBlocked = "blocked"
	KindDone    = "done"
	// KindSteal records a claim taken from a device whose lease had lapsed.
	// Distinct from KindClaim on purpose: "taken from someone" is a different
	// event to "picked up", and the timeline should not blur them.
	KindSteal = "steal"
	// KindBoot records a resume after the machine restarted.
	KindBoot = "boot"
	// KindProposal is what a device writes when its autonomy level permits it
	// to say what it would do but not to do it.
	KindProposal = "proposal"
	// KindSystem records an OS modification, cross-referencing the change
	// journal entry that carries its rollback command.
	KindSystem = "system"
)

// Event is one thing that happened to a task on one device.
type Event struct {
	Timestamp time.Time `json:"ts"`
	Node      string    `json:"node"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text,omitempty"`
}

// ProgressFile is a device's own append-only shard of a task's timeline.
func ProgressFile(repoPath, id, nodeID string) string {
	return filepath.Join(Dir(repoPath, id), "progress", nodeID+".jsonl")
}

// ProgressPattern matches every progress shard a device owns, across all tasks.
// Used by the sync layer to reapply this device's writes on top of origin.
func ProgressPattern(nodeID string) string {
	return filepath.Join("tasks", "*", "progress", nodeID+".jsonl")
}

// Record appends an event to this device's shard.
//
// Only ever appending to our own file is what makes concurrent work across
// devices conflict-free: no other device writes this path, so a diverged sync
// reapplies it wholesale instead of merging.
func Record(repoPath, id, nodeID, kind, text string) error {
	path := ProgressFile(repoPath, id, nodeID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("record progress: %w", err)
	}

	line, err := json.Marshal(Event{
		Timestamp: time.Now().UTC(),
		Node:      nodeID,
		Kind:      kind,
		Text:      strings.TrimSpace(text),
	})
	if err != nil {
		return fmt.Errorf("record progress: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("record progress: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("record progress: %w", err)
	}
	return nil
}

// Progress merges every device's shard into one chronological timeline.
//
// A malformed line is skipped rather than failing the read: a corrupt shard on
// one device must not make the task unreadable on every other.
func Progress(repoPath, id string) ([]Event, error) {
	matches, err := filepath.Glob(filepath.Join(Dir(repoPath, id), "progress", "*.jsonl"))
	if err != nil {
		return nil, err
	}

	var events []Event
	for _, path := range matches {
		shard, err := readShard(path)
		if err != nil {
			continue
		}
		events = append(events, shard...)
	}

	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Timestamp.Equal(events[j].Timestamp) {
			// Two devices can stamp the same instant; order deterministically
			// so the same repo renders identically everywhere.
			return events[i].Node < events[j].Node
		}
		return events[i].Timestamp.Before(events[j].Timestamp)
	})
	return events, nil
}

func readShard(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events, scanner.Err()
}
