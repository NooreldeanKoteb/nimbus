package cli

import (
	"runtime"
	"strings"
	"testing"
)

// Most work does not survive a restart and should not pretend to.
func TestBootDoesNothingWithoutAContract(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "port-server", "--goal", "port the server")

	out := d.run("boot")
	if !strings.Contains(out, "nothing to resume") {
		t.Errorf("boot picked up a task that never asked to survive a reboot:\n%s", out)
	}
	if !strings.Contains(out, "--on-boot") {
		t.Errorf("boot does not say how to opt a task in:\n%s", out)
	}
}

// The design says to ship boot resume in propose-only mode first, and that is
// what a device below L3 does: it says what it would do and stops.
func TestBootProposesRatherThanActsBelowL3(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the allowed_first commands in this test are POSIX shell")
	}
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "fix-boot",
		"--goal", "repair the bootloader",
		"--on-boot",
		"--allowed-first", "echo bootloader entry present",
		"--require-human", "mkfs*")

	out := d.run("boot", "--act")
	if !strings.Contains(out, "not starting a session") {
		t.Fatalf("an L1 device started an unattended session:\n%s", out)
	}
	if !strings.Contains(out, "L1") {
		t.Errorf("the refusal does not name the level holding it back:\n%s", out)
	}

	// The proposal is on the timeline, so the next session — or the next
	// person — sees that the machine came back and was ready to continue.
	shown := d.run("task", "show", "fix-boot")
	if !strings.Contains(shown, "proposal") {
		t.Errorf("no proposal was recorded:\n%s", shown)
	}
}

// allowed_first is enforced by nimbus running the checks itself. Instructing
// the session to run them first would be a request, not a rule.
func TestBootRunsTheAllowedFirstChecksAndFeedsThemToTheSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the allowed_first commands in this test are POSIX shell")
	}
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "fix-boot",
		"--goal", "repair the bootloader",
		"--on-boot",
		"--allowed-first", "echo rEFInd entry intact",
		"--allowed-first", "false")

	out := d.run("boot")
	if !strings.Contains(out, "rEFInd entry intact") {
		t.Fatalf("the check output is not in the brief:\n%s", out)
	}
	if !strings.Contains(out, "After the reboot") {
		t.Errorf("the brief does not tell the session the machine restarted:\n%s", out)
	}
	// A failed check is the most important one to surface.
	if !strings.Contains(out, "failed") {
		t.Errorf("a failing check was not reported:\n%s", out)
	}
	// And the contract's hard blocks are stated up front rather than discovered.
	if !strings.Contains(out, "nimbus system apply") {
		t.Errorf("the brief does not point at the journaled way to change things:\n%s", out)
	}
}

// It runs from a service unit at boot, where a failure is a red unit nobody
// reads rather than an error anybody sees.
func TestBootNeverFails(t *testing.T) {
	d := newDevice(t, "solo")

	// No state repo at all: the worst case a boot can find.
	if out, err := d.try("boot"); err != nil {
		t.Errorf("boot failed with no state repo: %v\n%s", err, out)
	}
}
