package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Handoff is what one device leaves behind for the next one (DESIGN.md §7).
//
// The fields are chosen to answer the questions a person actually has when
// sitting down at a different machine: what is finished, what is half-done, what
// comes next, what is stuck, and what state exists that git will not carry.
// That last one is the field people forget and the one that wastes the most
// time — a running container, an unmigrated database, a manually edited config.
type Handoff struct {
	Node string `json:"node"`
	Host string `json:"host,omitempty"`
	// SessionID is the Claude Code conversation this device uses for this task.
	// Recorded so returning to the same machine resumes the real conversation
	// rather than starting over from the summary.
	SessionID string    `json:"session_id,omitempty"`
	At        time.Time `json:"at"`
	Branch    string    `json:"branch,omitempty"`
	Done      []string  `json:"done,omitempty"`
	InFlight  []string  `json:"in_flight,omitempty"`
	Next      []string  `json:"next,omitempty"`
	Blocked   []string  `json:"blocked,omitempty"`
	// Outside lists state that does not travel in git and must be recreated or
	// reconnected on the receiving device.
	Outside []string `json:"outside_git,omitempty"`
	Notes   string   `json:"notes,omitempty"`
}

// SessionFile is a device's own most recent handoff for a task.
func SessionFile(repoPath, id, nodeID string) string {
	return filepath.Join(Dir(repoPath, id), "sessions", nodeID+".json")
}

// SessionPattern matches every handoff a device owns, across all tasks.
func SessionPattern(nodeID string) string {
	return filepath.Join("tasks", "*", "sessions", nodeID+".json")
}

// Empty reports whether a handoff carries no information worth saving.
func (h *Handoff) Empty() bool {
	return len(h.Done) == 0 && len(h.InFlight) == 0 && len(h.Next) == 0 &&
		len(h.Blocked) == 0 && len(h.Outside) == 0 && strings.TrimSpace(h.Notes) == ""
}

// Save writes this device's handoff, replacing its previous one.
//
// One file per device rather than a single shared handoff.md: a shared file
// would be overwritten by whichever device pushed last, silently discarding a
// note the other device had just written.
func (h *Handoff) Save(repoPath, id string) error {
	if h.At.IsZero() {
		h.At = time.Now().UTC()
	}
	// Writing a handoff must not drop the session binding: they share a file,
	// and losing the id would silently downgrade every later resume on this
	// device from "continue the conversation" to "start over from the summary".
	if h.SessionID == "" {
		if existing, err := LoadSession(repoPath, id, h.Node); err == nil {
			h.SessionID = existing.SessionID
		}
	}

	path := SessionFile(repoPath, id, h.Node)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("save handoff: %w", err)
	}

	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("save handoff: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("save handoff: %w", err)
	}
	return nil
}

// Handoffs returns every device's handoff for a task, newest first.
func Handoffs(repoPath, id string) ([]*Handoff, error) {
	matches, err := filepath.Glob(filepath.Join(Dir(repoPath, id), "sessions", "*.json"))
	if err != nil {
		return nil, err
	}

	handoffs := make([]*Handoff, 0, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var h Handoff
		if err := json.Unmarshal(data, &h); err != nil {
			continue
		}
		handoffs = append(handoffs, &h)
	}

	for i := 1; i < len(handoffs); i++ {
		for j := i; j > 0 && handoffs[j].At.After(handoffs[j-1].At); j-- {
			handoffs[j], handoffs[j-1] = handoffs[j-1], handoffs[j]
		}
	}
	return handoffs, nil
}

// LatestHandoff returns the most recent handoff from any device other than
// exclude. Resuming on a device wants the note the *previous* device left, not
// the one this device wrote before it walked away.
func LatestHandoff(repoPath, id, exclude string) (*Handoff, error) {
	handoffs, err := Handoffs(repoPath, id)
	if err != nil {
		return nil, err
	}
	for _, h := range handoffs {
		if h.Node != exclude {
			return h, nil
		}
	}
	// Fall back to our own: continuing our own work on the same device is the
	// second most common case, and a stale note beats no note.
	if len(handoffs) > 0 {
		return handoffs[0], nil
	}
	return nil, ErrNotFound
}

// Render writes the handoff as markdown, which is how it reaches Claude.
func (h *Handoff) Render(b *strings.Builder, n Namer) {
	label := name(n, h.Node)
	fmt.Fprintf(b, "Left by %s", label)
	if h.Host != "" && h.Host != label {
		fmt.Fprintf(b, " (%s)", h.Host)
	}
	fmt.Fprintf(b, " at %s (%s ago)\n", h.At.Format("2006-01-02 15:04 MST"), roughly(time.Since(h.At)))
	if h.Branch != "" {
		fmt.Fprintf(b, "Branch: %s\n", h.Branch)
	}

	section(b, "Done", h.Done)
	section(b, "In flight", h.InFlight)
	section(b, "Next", h.Next)
	section(b, "Blocked", h.Blocked)
	section(b, "State outside git", h.Outside)

	if notes := strings.TrimSpace(h.Notes); notes != "" {
		fmt.Fprintf(b, "\n%s\n", notes)
	}
}

func section(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, item := range items {
		fmt.Fprintf(b, "  - %s\n", item)
	}
}

// roughly renders a duration the way a person would say it.
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
