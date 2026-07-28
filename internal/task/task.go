// Package task models a unit of work that outlives any one device.
//
// A task is the unit that moves between machines (DESIGN.md §7): it carries the
// goal, the repository and branch it applies to, the capabilities a device must
// have to work on it, and a claim naming whichever device holds it now.
//
// Files inside the state repo:
//
//	tasks/<id>/task.json              shared metadata and claim
//	tasks/<id>/progress/<node>.jsonl  append-only, one shard per device
//	tasks/<id>/sessions/<node>.json   that device's most recent handoff
//
// The sharding is what makes concurrent work safe. Two devices appending to one
// progress file would conflict on every sync; two devices appending to their own
// shards never do, and a merged read reconstructs the single timeline.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Status values for a task.
const (
	StatusActive = "active"
	StatusPaused = "paused"
	StatusDone   = "done"
)

// ErrNotFound is returned when no task exists with the given id.
var ErrNotFound = errors.New("task not found")

// Claim records which device currently holds a task. Only one device should be
// working a task at a time; the claim is how the others find out.
type Claim struct {
	Node string    `json:"node"`
	Host string    `json:"host,omitempty"`
	At   time.Time `json:"at"`
}

// Task is one piece of work, portable across the fleet.
type Task struct {
	ID     string `json:"id"`
	Goal   string `json:"goal"`
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	Status string `json:"status"`
	// Needs lists capability labels a device must publish before it can work
	// this task — "gpu:nvidia", "display", "tool:docker". Matched against
	// device.Profile.Capabilities().
	Needs     []string  `json:"needs,omitempty"`
	CreatedOn string    `json:"created_on,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Claim     *Claim    `json:"claim,omitempty"`
	// Resume governs what happens to this task across a reboot (§10). Nil is
	// the normal case: most work does not survive the machine restarting and
	// should not pretend to.
	Resume *Contract `json:"resume,omitempty"`
}

// Dir is a task's directory inside the state repo.
func Dir(repoPath, id string) string {
	return filepath.Join(repoPath, "tasks", id)
}

// File is a task's metadata file inside the state repo.
func File(repoPath, id string) string {
	return filepath.Join(Dir(repoPath, id), "task.json")
}

// ValidateID rejects anything that would escape the tasks directory or produce
// a path that does not survive a round trip through git on another OS.
func ValidateID(id string) error {
	if id == "" {
		return errors.New("task id is empty")
	}
	if len(id) > 64 {
		return fmt.Errorf("task id %q is longer than 64 characters", id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("task id %q is reserved", id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return fmt.Errorf("task id %q: use lowercase letters, digits, - and _ only", id)
		}
	}
	return nil
}

// New creates a task and writes it into the state repo.
func New(repoPath string, t *Task) error {
	if err := ValidateID(t.ID); err != nil {
		return err
	}
	if strings.TrimSpace(t.Goal) == "" {
		return errors.New("a task needs a goal")
	}
	if _, err := os.Stat(File(repoPath, t.ID)); err == nil {
		return fmt.Errorf("task %q already exists", t.ID)
	}

	now := time.Now().UTC()
	t.CreatedAt = now
	if t.Status == "" {
		t.Status = StatusActive
	}
	return t.Save(repoPath)
}

// Save writes the task, stamping UpdatedAt.
func (t *Task) Save(repoPath string) error {
	if err := ValidateID(t.ID); err != nil {
		return err
	}
	t.UpdatedAt = time.Now().UTC()

	if err := os.MkdirAll(Dir(repoPath, t.ID), 0o755); err != nil {
		return fmt.Errorf("save task: %w", err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("save task: %w", err)
	}
	// Trailing newline keeps git diffs readable, matching device profiles.
	if err := os.WriteFile(File(repoPath, t.ID), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("save task: %w", err)
	}
	return nil
}

// Load reads one task.
func Load(repoPath, id string) (*Task, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(File(repoPath, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}

	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse task %s: %w", id, err)
	}
	return &t, nil
}

// List returns every task in the repo, most recently updated first. A single
// unreadable task must not hide the rest, matching device.LoadFleet.
func List(repoPath string) ([]*Task, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, "tasks", "*", "task.json"))
	if err != nil {
		return nil, err
	}

	tasks := make([]*Task, 0, len(matches))
	for _, path := range matches {
		t, err := Load(repoPath, filepath.Base(filepath.Dir(path)))
		if err != nil {
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
	})
	return tasks, nil
}

// Active returns the task this device is currently working on: the most
// recently updated task it holds a claim on.
//
// Deriving this rather than storing a pointer means there is no per-device
// "current task" file to keep in sync, and no way for the pointer to disagree
// with the claim it refers to.
func Active(repoPath, nodeID string) (*Task, error) {
	tasks, err := List(repoPath)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.Status == StatusActive && t.HeldBy(nodeID) {
			return t, nil
		}
	}
	return nil, ErrNotFound
}

// HeldBy reports whether this device currently holds the task.
func (t *Task) HeldBy(nodeID string) bool {
	return t.Claim != nil && t.Claim.Node == nodeID
}

// Take claims the task for a device. The write is local; whether it survives is
// decided by the next sync, since task.json is shared and origin wins on a
// diverged push. Callers reload after syncing to learn who actually won.
func (t *Task) Take(nodeID, hostname string) {
	t.Claim = &Claim{Node: nodeID, Host: hostname, At: time.Now().UTC()}
	if t.Status == StatusPaused {
		t.Status = StatusActive
	}
}

// Release drops this device's claim, leaving another device free to pick it up.
// Releasing a task held by someone else is a no-op rather than a steal.
func (t *Task) Release(nodeID string) bool {
	if !t.HeldBy(nodeID) {
		return false
	}
	t.Claim = nil
	return true
}

// Unmet returns the capabilities this task requires that a device lacks.
//
// This is what makes cross-device handoff correct rather than merely possible:
// a task needing a GPU should not silently resume on the laptop that has none.
func (t *Task) Unmet(capabilities []string) []string {
	if len(t.Needs) == 0 {
		return nil
	}
	have := make(map[string]bool, len(capabilities))
	for _, c := range capabilities {
		have[c] = true
	}

	var missing []string
	for _, need := range t.Needs {
		if !have[need] {
			missing = append(missing, need)
		}
	}
	return missing
}

// Namer turns a node id into the name a person should see. device.Fleet
// implements it; a nil Namer falls back to raw ids.
//
// Passed in rather than stored on the task so that renaming a device updates
// every claim and timeline entry that mentions it, including ones written
// before the rename.
type Namer interface {
	Label(nodeID string) string
}

func name(n Namer, nodeID string) string {
	if n == nil || nodeID == "" {
		return nodeID
	}
	return n.Label(nodeID)
}

// Summary is a one-line description for task listings.
func (t *Task) Summary(n Namer) string {
	parts := []string{t.Status}
	if t.Branch != "" {
		parts = append(parts, t.Branch)
	}
	if t.Claim != nil {
		parts = append(parts, "held by "+name(n, t.Claim.Node))
	} else {
		parts = append(parts, "unclaimed")
	}
	return strings.Join(parts, ", ")
}
