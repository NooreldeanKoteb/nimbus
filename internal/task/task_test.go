package task

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTask(t *testing.T, repo, id, goal string) *Task {
	t.Helper()
	task := &Task{ID: id, Goal: goal}
	if err := New(repo, task); err != nil {
		t.Fatalf("New(%s): %v", id, err)
	}
	return task
}

func TestNewAndLoadRoundTrip(t *testing.T) {
	repo := t.TempDir()
	created := &Task{
		ID: "auth-refactor", Goal: "move auth to JWT",
		Repo: "git@github.com:you/project.git", Branch: "feat/jwt",
		Needs: []string{"tool:docker"},
	}
	if err := New(repo, created); err != nil {
		t.Fatal(err)
	}

	got, err := Load(repo, "auth-refactor")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Goal != created.Goal || got.Branch != "feat/jwt" {
		t.Errorf("Load() = %+v, want the task that was written", got)
	}
	if got.Status != StatusActive {
		t.Errorf("Status = %q, want a new task to start active", got.Status)
	}
	if len(got.Needs) != 1 || got.Needs[0] != "tool:docker" {
		t.Errorf("Needs = %v, want requirements preserved", got.Needs)
	}
}

func TestNewRejectsDuplicate(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "dup", "first")

	if err := New(repo, &Task{ID: "dup", Goal: "second"}); err == nil {
		t.Error("New() overwrote an existing task")
	}
}

func TestNewRequiresGoal(t *testing.T) {
	if err := New(t.TempDir(), &Task{ID: "empty", Goal: "   "}); err == nil {
		t.Error("New() accepted a task with no goal")
	}
}

// A task id becomes a directory name, so it is a boundary that has to reject
// anything capable of escaping the tasks directory.
func TestValidateIDRejectsPathEscapes(t *testing.T) {
	bad := []string{
		"", "..", ".", "../etc", "a/b", `a\b`, "Upper", "has space",
		"trailing/", "/leading", "nul\x00byte",
	}
	for _, id := range bad {
		if err := ValidateID(id); err == nil {
			t.Errorf("ValidateID(%q) = nil, want rejection", id)
		}
	}

	for _, id := range []string{"a", "auth-refactor", "fix_bug_42", "x1"} {
		if err := ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) = %v, want accepted", id, err)
		}
	}
}

func TestLoadMissingTaskIsNotFound(t *testing.T) {
	_, err := Load(t.TempDir(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Load() error = %v, want ErrNotFound", err)
	}
}

func TestListOrdersByMostRecentlyUpdated(t *testing.T) {
	repo := t.TempDir()
	for _, id := range []string{"first", "second", "third"} {
		newTask(t, repo, id, "goal for "+id)
		// Timestamps come from the wall clock; without a gap the ordering is
		// arbitrary and the test proves nothing.
		time.Sleep(2 * time.Millisecond)
	}

	tasks, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("List() returned %d tasks, want 3", len(tasks))
	}
	if tasks[0].ID != "third" {
		t.Errorf("List()[0] = %s, want the most recently updated (third)", tasks[0].ID)
	}
}

// One unreadable task must not hide the fleet's other work.
func TestListSkipsCorruptTasks(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "good", "readable")

	broken := Dir(repo, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "task.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	tasks, err := List(repo)
	if err != nil {
		t.Fatalf("List() error = %v, want the corrupt file skipped", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "good" {
		t.Errorf("List() = %v, want only the readable task", tasks)
	}
}

func TestActiveFindsOnlyThisDevicesClaim(t *testing.T) {
	repo := t.TempDir()

	mine := newTask(t, repo, "mine", "my work")
	mine.Take("desktop", "desktop.local")
	if err := mine.Save(repo); err != nil {
		t.Fatal(err)
	}

	theirs := newTask(t, repo, "theirs", "their work")
	theirs.Take("laptop", "laptop.local")
	if err := theirs.Save(repo); err != nil {
		t.Fatal(err)
	}

	got, err := Active(repo, "desktop")
	if err != nil {
		t.Fatalf("Active() error = %v", err)
	}
	if got.ID != "mine" {
		t.Errorf("Active() = %s, want the task this device holds", got.ID)
	}

	if _, err := Active(repo, "server"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Active() for an idle device = %v, want ErrNotFound", err)
	}
}

// A finished task is not what a device should resume into, even while its
// claim is still recorded.
func TestActiveIgnoresDoneTasks(t *testing.T) {
	repo := t.TempDir()
	done := newTask(t, repo, "shipped", "already delivered")
	done.Take("desktop", "desktop.local")
	done.Status = StatusDone
	if err := done.Save(repo); err != nil {
		t.Fatal(err)
	}

	if _, err := Active(repo, "desktop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Active() = %v, want a done task ignored", err)
	}
}

func TestReleaseOnlyDropsOwnClaim(t *testing.T) {
	tk := &Task{ID: "x", Goal: "g"}
	tk.Take("laptop", "laptop.local")

	if tk.Release("desktop") {
		t.Error("Release() let a device drop another device's claim")
	}
	if !tk.HeldBy("laptop") {
		t.Error("the original claim was lost")
	}
	if !tk.Release("laptop") || tk.Claim != nil {
		t.Error("Release() did not drop the holder's own claim")
	}
}

func TestTakeRevivesPausedTask(t *testing.T) {
	tk := &Task{ID: "x", Goal: "g", Status: StatusPaused}
	tk.Take("desktop", "desktop.local")

	if tk.Status != StatusActive {
		t.Errorf("Status = %q, want claiming a paused task to reactivate it", tk.Status)
	}
}

// Affinity is what stops a GPU task silently resuming on the laptop that has
// no GPU (DESIGN.md §7).
func TestUnmetReportsMissingCapabilities(t *testing.T) {
	tk := &Task{ID: "x", Goal: "g", Needs: []string{"gpu:nvidia", "tool:docker", "display"}}
	have := []string{"os:linux", "tool:docker", "headless"}

	unmet := tk.Unmet(have)
	if len(unmet) != 2 || unmet[0] != "gpu:nvidia" || unmet[1] != "display" {
		t.Errorf("Unmet() = %v, want [gpu:nvidia display]", unmet)
	}

	if got := tk.Unmet([]string{"gpu:nvidia", "tool:docker", "display"}); got != nil {
		t.Errorf("Unmet() = %v, want nil when every requirement is met", got)
	}
	if got := (&Task{ID: "y", Goal: "g"}).Unmet(nil); got != nil {
		t.Errorf("Unmet() = %v, want nil for a task with no requirements", got)
	}
}
