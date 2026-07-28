package system

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// The invariant is about ordering, not about record-keeping: the rollback has
// to be on disk before the command runs, because the command is what might stop
// the machine coming back.
func TestRollbackIsRecordedBeforeTheCommandRuns(t *testing.T) {
	repo := t.TempDir()

	var journalDuringRun string
	run := func(_ context.Context, _ string) (string, error) {
		data, err := os.ReadFile(File(repo, "node-a"))
		if err != nil {
			t.Fatalf("nothing was journaled before the command ran: %v", err)
		}
		journalDuringRun = string(data)
		return "installed", nil
	}

	c := Change{Kind: "package", Target: "ripgrep",
		Command: "apt install -y ripgrep", Rollback: "apt remove -y ripgrep"}
	applied, err := Apply(context.Background(), repo, "node-a", c, run)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(journalDuringRun, "apt remove -y ripgrep") {
		t.Errorf("the rollback was not on disk while the command ran:\n%s", journalDuringRun)
	}
	if !strings.Contains(journalDuringRun, StatusPlanned) {
		t.Errorf("the pre-run record is not marked planned:\n%s", journalDuringRun)
	}
	if applied.Status != StatusApplied {
		t.Errorf("status = %q, want %q", applied.Status, StatusApplied)
	}
}

// A run that dies mid-flight has to be visible as one, because that is exactly
// the state somebody recovering the machine is looking for.
func TestAChangeThatNeverFinishedStaysPlanned(t *testing.T) {
	repo := t.TempDir()

	_, err := Apply(context.Background(), repo, "node-a", Change{
		Command: "reboot", Rollback: "true",
	}, func(context.Context, string) (string, error) {
		return "", errors.New("connection lost")
	})
	if err == nil {
		t.Fatal("a failed command was reported as success")
	}

	history, err := History(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("got %d changes, want 1 folded record", len(history))
	}
	if history[0].Status != StatusFailed {
		t.Errorf("status = %q, want %q", history[0].Status, StatusFailed)
	}
	// The folded record must still carry how to undo it: a failure is when the
	// rollback matters most, and the failing record does not restate it.
	if history[0].Rollback != "true" {
		t.Errorf("the rollback was lost when the change failed: %+v", history[0])
	}
}

func TestApplyRefusesAChangeItCannotUndo(t *testing.T) {
	repo := t.TempDir()
	ran := false
	run := func(context.Context, string) (string, error) { ran = true; return "", nil }

	if _, err := Apply(context.Background(), repo, "node-a", Change{Command: "rm -rf /etc"}, run); err == nil {
		t.Error("a change with no rollback was applied")
	}
	if _, err := Apply(context.Background(), repo, "node-a", Change{
		Command: "mkfs /dev/sdb", Irreversible: true,
	}, run); err == nil {
		t.Error("an irreversible change with no stated reason was applied")
	}
	if ran {
		t.Error("a refused change still ran the command")
	}
}

func TestRollbackRunsTheRecordedUndo(t *testing.T) {
	repo := t.TempDir()
	var ran []string
	run := func(_ context.Context, command string) (string, error) {
		ran = append(ran, command)
		return "ok", nil
	}

	applied, err := Apply(context.Background(), repo, "node-a", Change{
		Command: "systemctl enable nginx", Rollback: "systemctl disable nginx",
	}, run)
	if err != nil {
		t.Fatal(err)
	}

	undone, err := Rollback(context.Background(), repo, "node-a", applied.ID, run)
	if err != nil {
		t.Fatal(err)
	}
	if undone.Status != StatusRolledBack {
		t.Errorf("status = %q, want %q", undone.Status, StatusRolledBack)
	}
	if len(ran) != 2 || ran[1] != "systemctl disable nginx" {
		t.Errorf("rollback ran %v, want the undo command second", ran)
	}

	// Rolling back twice is a mistake worth naming rather than repeating.
	if _, err := Rollback(context.Background(), repo, "node-a", applied.ID, run); err == nil {
		t.Error("a change was rolled back twice")
	}
}

// The rollback undoes a change to a specific machine's packages and services.
// Running it anywhere else does nothing useful and may do something harmful.
func TestRollbackRefusesAnotherDevicesChange(t *testing.T) {
	repo := t.TempDir()
	run := func(context.Context, string) (string, error) { return "", nil }

	applied, err := Apply(context.Background(), repo, "desktop", Change{
		Command: "apt install -y cuda", Rollback: "apt remove -y cuda",
	}, run)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Rollback(context.Background(), repo, "laptop", applied.ID, run); err == nil {
		t.Error("the laptop rolled back a change made on the desktop")
	}
}

func TestIrreversibleChangesCannotBeRolledBack(t *testing.T) {
	repo := t.TempDir()
	run := func(context.Context, string) (string, error) { return "", nil }

	applied, err := Apply(context.Background(), repo, "node-a", Change{
		Command: "mkfs.ext4 /dev/sdb1", Irreversible: true, Reason: "the filesystem is gone",
	}, run)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Rollback(context.Background(), repo, "node-a", applied.ID, run)
	if err == nil {
		t.Fatal("an irreversible change was rolled back")
	}
	if !strings.Contains(err.Error(), "the filesystem is gone") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// History merges every device's journal, which is what makes the record useful
// to an operator who is not sitting at the machine that changed.
func TestHistoryMergesDevicesAndFoldsToLatest(t *testing.T) {
	repo := t.TempDir()
	run := func(context.Context, string) (string, error) { return "", nil }

	for _, node := range []string{"laptop", "desktop"} {
		if _, err := Apply(context.Background(), repo, node, Change{
			Command: "touch /tmp/" + node, Rollback: "rm /tmp/" + node,
		}, run); err != nil {
			t.Fatal(err)
		}
	}

	history, err := History(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("got %d changes, want one per device", len(history))
	}
	for _, c := range history {
		if c.Status != StatusApplied {
			t.Errorf("%s folded to %q rather than its latest record", c.ID, c.Status)
		}
	}
}

// A machine that died mid-write leaves a truncated final line. Everything
// before it is still evidence and must still be readable.
func TestATruncatedJournalStillReads(t *testing.T) {
	repo := t.TempDir()
	run := func(context.Context, string) (string, error) { return "", nil }

	if _, err := Apply(context.Background(), repo, "node-a", Change{
		Command: "apt install -y foo", Rollback: "apt remove -y foo",
	}, run); err != nil {
		t.Fatal(err)
	}

	path := File(repo, "node-a")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte(`{"id":"trunc","stat`)...), 0o644); err != nil {
		t.Fatal(err)
	}

	history, err := History(repo)
	if err != nil {
		t.Fatalf("a truncated line made the whole journal unreadable: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("got %d changes, want the one complete record", len(history))
	}
}

func TestLoadAcceptsAnUnambiguousPrefix(t *testing.T) {
	repo := t.TempDir()
	run := func(context.Context, string) (string, error) { return "", nil }

	applied, err := Apply(context.Background(), repo, "node-a", Change{
		Command: "true", Rollback: "true",
	}, run)
	if err != nil {
		t.Fatal(err)
	}

	found, err := Load(repo, applied.ID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != applied.ID {
		t.Errorf("prefix resolved to %s, want %s", found.ID, applied.ID)
	}
	if _, err := Load(repo, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown id returned %v, want ErrNotFound", err)
	}
}
