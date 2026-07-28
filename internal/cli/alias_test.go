package cli

import (
	"strings"
	"testing"
)

func TestAliasNamesThisDevice(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	d.run("alias", "kali-thinkpad")

	if out := d.run("alias"); !strings.Contains(out, "kali-thinkpad") {
		t.Errorf("alias = %q, want the name that was set", out)
	}
	if out := d.run("doctor"); !strings.Contains(out, "kali-thinkpad") {
		t.Errorf("doctor does not show the alias:\n%s", out)
	}
	if out := d.run("fleet"); !strings.Contains(out, "kali-thinkpad") {
		t.Errorf("fleet does not show the alias:\n%s", out)
	}
}

func TestAliasRejectsInvalidNames(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	for _, bad := range []string{"Has Caps", "has.dots", "way-too-long-an-alias-to-be-usable-in-a-listing"} {
		if _, err := d.try("alias", bad); err == nil {
			t.Errorf("alias accepted %q", bad)
		}
	}
}

// Two devices answering to one name makes the name useless for addressing
// either of them.
func TestAliasRefusesCollision(t *testing.T) {
	origin := bareRemote(t)

	first := newDevice(t, "aaaa1111")
	first.run("state", "init", "--remote", origin)
	first.run("alias", "studio")
	first.run("doctor", "--publish")

	second := newDevice(t, "bbbb2222")
	second.run("state", "clone", origin)

	out, err := second.try("alias", "studio")
	if err == nil {
		t.Fatalf("two devices were allowed the same alias:\n%s", out)
	}
	if !strings.Contains(err.Error(), "studio") {
		t.Errorf("error = %v, want it to name the conflict", err)
	}

	// --force is the deliberate override for a device being replaced.
	if _, err := second.try("alias", "studio", "--force"); err != nil {
		t.Errorf("alias --force = %v, want the override to work", err)
	}
}

// Renaming a device must not move its node id: the id is the key for every
// path in the state repo and is hashed into every audit entry.
func TestAliasDoesNotBreakTheAuditChain(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "work", "--goal", "something")

	d.run("alias", "renamed-box")

	if out := d.run("audit", "--verify"); !strings.Contains(out, "chain intact") {
		t.Errorf("renaming broke the audit chain:\n%s", out)
	}
	// And the rename itself is a recorded action.
	if out := d.run("audit", "-n", "0"); !strings.Contains(out, "device.alias") {
		t.Errorf("rename was not audited:\n%s", out)
	}
}

// Labels are resolved at render time, so a rename updates work that was
// recorded before the device had its new name.
func TestRenameShowsRetroactivelyOnOtherDevices(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "aaaa1111")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("alias", "old-name")
	laptop.run("task", "new", "port", "--goal", "port the server")
	laptop.run("task", "handoff", "--next", "finish the handler", "--release")

	desktop := newDevice(t, "bbbb2222")
	desktop.run("state", "clone", origin)
	desktop.run("alias", "workstation")

	if out := desktop.run("resume", "port", "--brief"); !strings.Contains(out, "old-name") {
		t.Fatalf("handoff is not attributed to the laptop's name:\n%s", out)
	}

	// The laptop is renamed after that work was already recorded.
	laptop.run("alias", "kali-thinkpad")
	desktop.run("state", "sync")

	out := desktop.run("resume", "port", "--brief")
	if !strings.Contains(out, "kali-thinkpad") {
		t.Errorf("rename did not reach the older handoff:\n%s", out)
	}
	if strings.Contains(out, "old-name") {
		t.Errorf("the previous name is still being shown:\n%s", out)
	}
}

// Addressing another device by name is the point of having names.
func TestAuditAcceptsAliasForNode(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "aaaa1111")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("alias", "kali-thinkpad")
	laptop.run("task", "new", "work", "--goal", "something")

	desktop := newDevice(t, "bbbb2222")
	desktop.run("state", "clone", origin)

	out := desktop.run("audit", "--node", "kali-thinkpad")
	if !strings.Contains(out, "task.new") {
		t.Errorf("could not read the laptop's log by alias:\n%s", out)
	}

	// An unambiguous prefix is enough.
	if _, err := desktop.try("audit", "--node", "kali"); err != nil {
		t.Errorf("prefix lookup failed: %v", err)
	}
	if _, err := desktop.try("audit", "--node", "nonexistent"); err == nil {
		t.Error("audit invented a device")
	}
}

func TestInitAcceptsAlias(t *testing.T) {
	d := newDevice(t, "solo")

	out := d.run("init", "--skip-claude", "--alias", "build-server")
	if !strings.Contains(out, "build-server") {
		t.Errorf("init did not report the alias:\n%s", out)
	}
	if out := d.run("alias"); !strings.Contains(out, "build-server") {
		t.Errorf("alias was not persisted by init:\n%s", out)
	}
}

// A device nobody named should still read as something, not a hex string.
func TestDeviceGetsADefaultName(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	out := d.run("alias")
	if strings.Contains(out, "no alias set") {
		t.Errorf("device was left unnamed:\n%s", out)
	}
}
