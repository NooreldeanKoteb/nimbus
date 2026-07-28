package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Journal actions.
const (
	ActionAdd     = "add"
	ActionPromote = "promote"
	ActionForget  = "forget"
	ActionExpire  = "expire"
)

// Event records something that happened to memory on one device.
//
// The journal exists because memory is the one part of the state repo that
// deletes things. When a fact you expected is missing, the journal is what
// tells you whether it expired, was promoted somewhere else, or was never
// written — none of which the absence of a file can distinguish.
type Event struct {
	Timestamp time.Time `json:"ts"`
	Node      string    `json:"node"`
	Action    string    `json:"action"`
	ID        string    `json:"id"`
	Tier      string    `json:"tier,omitempty"`
	Text      string    `json:"text,omitempty"`
}

// JournalFile is a device's own append-only memory log.
func JournalFile(repoPath, nodeID string) string {
	return filepath.Join(repoPath, "memory", "journal", nodeID+".jsonl")
}

// Record appends to this device's journal shard. Sharded per device for the
// same reason as task progress: two machines appending to one file conflict on
// every sync, and to their own files never do.
func Record(repoPath, nodeID string, e Event) error {
	e.Timestamp = time.Now().UTC()
	e.Node = nodeID

	path := JournalFile(repoPath, nodeID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("journal: %w", err)
	}

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	return nil
}
