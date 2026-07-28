package cli

import (
	"strings"
	"testing"
)

func TestAutonomyDefaultsAndReportsTheLadder(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	out := d.run("autonomy")
	if !strings.Contains(out, "L1") {
		t.Errorf("a fresh device does not report the default level:\n%s", out)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("output does not say the level was never set:\n%s", out)
	}
	// The invariants are what make raising the level safe, so they belong in
	// front of anyone reading this.
	for _, want := range []string{"force-push", "unencrypted secrets", "rollback"} {
		if !strings.Contains(out, want) {
			t.Errorf("the invariants are not shown (%q missing):\n%s", want, out)
		}
	}
}

func TestAutonomySetsAndPersistsTheLevel(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	out := d.run("autonomy", "l3", "--note", "my own desktop")
	if !strings.Contains(out, "L3") {
		t.Fatalf("setting the level did not confirm it:\n%s", out)
	}
	if !strings.Contains(out, "raised from L1") {
		t.Errorf("raising the ceiling was not called out:\n%s", out)
	}

	shown := d.run("autonomy")
	if !strings.Contains(shown, "my own desktop") {
		t.Errorf("the note did not persist:\n%s", shown)
	}
	if _, err := d.try("autonomy", "l9"); err == nil {
		t.Error("l9 was accepted; there is no L9")
	}
}

// The ladder governs unattended work. A tool call is Claude acting, so it is
// unattended; a person typing the same command is not.
func TestSystemChangesAreRefusedUnattendedBelowL3(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	out, ok := d.call(t, "nimbus_system_apply", map[string]any{
		"command": "touch " + d.home + "/marker", "rollback": "rm " + d.home + "/marker",
	})
	if ok {
		t.Fatalf("an L1 device made a system change unattended:\n%s", out)
	}
	if !strings.Contains(out, "nimbus autonomy") {
		t.Errorf("the refusal does not say how to permit it:\n%s", out)
	}

	// Same command, person at the keyboard: permitted.
	if out := d.run("system", "apply", "true", "--rollback", "true"); !strings.Contains(out, "applied") {
		t.Errorf("a person could not make a system change on their own machine:\n%s", out)
	}

	// And at L3 the tool call goes through.
	d.run("autonomy", "l3")
	if out, ok := d.call(t, "nimbus_system_apply", map[string]any{
		"command": "true", "rollback": "true",
	}); !ok {
		t.Errorf("an L3 device refused a system change:\n%s", out)
	}
}

// Invariant 4: the rollback is not optional, and the refusal cannot be lifted
// by raising the level.
func TestSystemApplyDemandsARollbackAtEveryLevel(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("autonomy", "l3")

	out, err := d.try("system", "apply", "rm -rf /etc/nginx")
	if err == nil {
		t.Fatalf("a change with no rollback was applied:\n%s", out)
	}
	if !strings.Contains(err.Error(), "invariant 4") {
		t.Errorf("the refusal does not cite the invariant: %v", err)
	}
	if strings.Contains(err.Error(), "nimbus autonomy") {
		t.Errorf("the refusal suggests raising the level, which cannot help: %v", err)
	}
}

// The point of the journal is that somebody who did not watch the change happen
// can still undo it.
func TestSystemChangesAreJournaledAndReversible(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	marker := d.home + "/changed"

	d.run("system", "apply", "touch "+marker,
		"--rollback", "rm -f "+marker, "--kind", "config", "--target", "marker file")

	listed := d.run("system", "list")
	if !strings.Contains(listed, "touch "+marker) {
		t.Fatalf("the change is not in the journal:\n%s", listed)
	}
	if !strings.Contains(listed, "rm -f "+marker) {
		t.Errorf("the journal does not carry the rollback:\n%s", listed)
	}

	id := firstField(t, listed)
	if out := d.run("system", "rollback", id); !strings.Contains(out, "rolled-back") {
		t.Errorf("rollback did not report success:\n%s", out)
	}
	if out := d.run("system", "show", id); !strings.Contains(out, "rolled-back") {
		t.Errorf("the journal does not reflect the rollback:\n%s", out)
	}
}

// A task can forbid a command outright, and that holds at L3 — the level is a
// ceiling on what may happen unattended, not a licence.
func TestRequireHumanBlocksACommandAtEveryLevel(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("autonomy", "l3")
	d.run("task", "new", "fix-boot",
		"--goal", "repair the bootloader",
		"--require-human", "mkfs*")

	out, ok := d.call(t, "nimbus_system_apply", map[string]any{
		"command": "mkfs.ext4 /dev/sdb1", "rollback": "true",
	})
	if ok {
		t.Fatalf("a require_human command ran unattended at L3:\n%s", out)
	}
	if !strings.Contains(out, "require_human") {
		t.Errorf("the refusal does not name the contract:\n%s", out)
	}

	// A person typing it is not the confirmation either. Naming a pattern in
	// require_human means someone has to say "yes, this one" about that
	// specific command, not merely be present.
	if _, err := d.try("system", "apply", "mkfs.ext4 /dev/sdb1", "--rollback", "true"); err == nil {
		t.Error("a require_human command ran from the CLI with no confirmation")
	} else if !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("the refusal does not say how to confirm: %v", err)
	}

	if out := d.run("system", "apply", "true", "--rollback", "true", "--confirm"); !strings.Contains(out, "applied") {
		t.Errorf("--confirm broke an ordinary change:\n%s", out)
	}
}

// Claude has to know its ceiling without discovering it by being refused.
func TestTheBriefStatesTheAutonomyLevel(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	out := d.run("context")
	if !strings.Contains(out, "Autonomy: **L1**") {
		t.Errorf("the brief does not state the level:\n%s", out)
	}
	if !strings.Contains(out, "nimbus system apply") {
		t.Errorf("the brief does not point at the safe way to change the system:\n%s", out)
	}

	d.run("autonomy", "l3")
	if out := d.run("context"); !strings.Contains(out, "Autonomy: **L3**") {
		t.Errorf("the brief did not follow the level change:\n%s", out)
	}
}

// firstField returns the first whitespace-separated token of the first
// non-empty line, which is the change id in `system list` output.
func firstField(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			return fields[0]
		}
	}
	t.Fatalf("no id in output:\n%s", out)
	return ""
}
