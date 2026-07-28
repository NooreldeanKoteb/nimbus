package cli

import (
	"runtime"
	"strings"
	"testing"
)

// twoDevices sets up a laptop and a desktop sharing one origin, which is the
// smallest arrangement in which peer execution means anything at all.
func twoDevices(t *testing.T) (laptop, desktop *fakeDevice) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the peer-exec tests run shell commands")
	}
	origin := bareRemote(t)

	laptop = newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("alias", "kali-thinkpad")
	laptop.run("doctor", "--publish")
	laptop.run("state", "sync")

	desktop = newDevice(t, "desktop")
	desktop.run("state", "clone", origin)
	desktop.run("alias", "studio")
	desktop.run("doctor", "--publish")
	desktop.run("state", "sync")

	laptop.run("state", "sync")
	return laptop, desktop
}

// execID pulls the request id out of `nimbus exec` output.
func execID(t *testing.T, out string) string {
	t.Helper()
	_, rest, ok := strings.Cut(out, "(")
	if !ok {
		t.Fatalf("no request id in output:\n%s", out)
	}
	id, _, ok := strings.Cut(rest, ")")
	if !ok {
		t.Fatalf("no request id in output:\n%s", out)
	}
	return strings.TrimSpace(id)
}

// The Phase 5 headline: one device asks another to run something, that device
// decides for itself whether to, runs it, and the output comes back.
func TestPeerExecutionEndToEnd(t *testing.T) {
	laptop, desktop := twoDevices(t)

	// The desktop decides what it will do for others. Nothing the laptop sends
	// can widen this.
	desktop.run("autonomy", "l3")
	desktop.run("autonomy", "allow", "echo")
	desktop.run("state", "sync")

	laptop.run("autonomy", "l2")
	out := laptop.run("exec", "studio", "echo mesh-is-alive")
	id := execID(t, out)
	laptop.run("state", "sync")

	// The desktop does the work. --no-sync omitted: this stands in for a person
	// running it, so it syncs on its own.
	desktop.run("state", "sync")
	worked := desktop.run("work")
	if !strings.Contains(worked, "exit 0") {
		t.Fatalf("the desktop did not run the command:\n%s", worked)
	}

	laptop.run("state", "sync")
	result := laptop.run("exec", "--follow", id, "--timeout", "0")
	if !strings.Contains(result, "mesh-is-alive") {
		t.Errorf("the output did not come back:\n%s", result)
	}
	if !strings.Contains(result, "exit 0") {
		t.Errorf("the result did not come back:\n%s", result)
	}
}

// Invariant 3, from the only angle that matters: the allowlist belongs to the
// device being asked. A sender at any level cannot talk a peer into running
// something that peer never allowed.
func TestAPeerRefusesWhatIsNotOnItsOwnAllowlist(t *testing.T) {
	laptop, desktop := twoDevices(t)

	desktop.run("autonomy", "l3")
	desktop.run("autonomy", "allow", "echo")
	desktop.run("state", "sync")

	laptop.run("autonomy", "l3")
	out := laptop.run("exec", "studio", "rm -rf /tmp/definitely-not-allowed")
	id := execID(t, out)
	laptop.run("state", "sync")

	desktop.run("state", "sync")
	desktop.run("work")

	laptop.run("state", "sync")
	result := laptop.run("exec", "--follow", id, "--timeout", "0")
	if !strings.Contains(result, "refused") {
		t.Fatalf("a command off the allowlist was not refused:\n%s", result)
	}
	if !strings.Contains(result, "invariant 3") {
		t.Errorf("the refusal does not name the invariant that made it:\n%s", result)
	}
}

// A refusal that is never reported is indistinguishable from a device that is
// still working, and would leave the sender waiting on a decision already made.
func TestARefusalTravelsBackRatherThanBeingSwallowed(t *testing.T) {
	laptop, desktop := twoDevices(t)

	// L1: below peer.exec, and with an empty allowlist besides.
	desktop.run("autonomy", "l1")
	desktop.run("state", "sync")

	laptop.run("autonomy", "l2")
	out := laptop.run("exec", "studio", "echo hello")
	id := execID(t, out)
	laptop.run("state", "sync")

	desktop.run("state", "sync")
	desktop.run("work")

	laptop.run("state", "sync")
	result := laptop.run("exec", "--follow", id, "--timeout", "0")
	if !strings.Contains(result, "refused") {
		t.Fatalf("the sender was left waiting on a refusal:\n%s", result)
	}
}

// Being on the allowlist is necessary, not sufficient. A device that listed a
// command but never raised its level has not volunteered to run anything.
func TestTheAllowlistDoesNotByItselfPermitExecution(t *testing.T) {
	laptop, desktop := twoDevices(t)

	desktop.run("autonomy", "allow", "echo")
	warned := desktop.run("autonomy", "allow", "hostname")
	if !strings.Contains(warned, "L3") {
		t.Errorf("allowing a command below L3 does not say the level is still missing:\n%s", warned)
	}
	desktop.run("state", "sync")

	laptop.run("autonomy", "l2")
	out := laptop.run("exec", "studio", "echo hello")
	id := execID(t, out)
	laptop.run("state", "sync")

	desktop.run("state", "sync")
	desktop.run("work")

	laptop.run("state", "sync")
	result := laptop.run("exec", "--follow", id, "--timeout", "0")
	if !strings.Contains(result, "refused") {
		t.Fatalf("an allowlisted command ran below L3:\n%s", result)
	}
	if !strings.Contains(result, "peer.exec") {
		t.Errorf("the refusal does not say the level is what blocked it:\n%s", result)
	}
}

// A person typing `nimbus work` did not choose these commands — the sender did.
// Treating them as attended would let anybody on the bus borrow that presence.
func TestRunningWorkByHandDoesNotMakePeerCommandsAttended(t *testing.T) {
	laptop, desktop := twoDevices(t)

	desktop.run("autonomy", "l1")
	desktop.run("state", "sync")

	laptop.run("autonomy", "l2")
	out := laptop.run("exec", "studio", "echo hello")
	id := execID(t, out)
	laptop.run("state", "sync")

	desktop.run("state", "sync")
	// Run by a person at a terminal — Attended is true for the CLI.
	desktop.run("work")

	laptop.run("state", "sync")
	result := laptop.run("exec", "--follow", id, "--timeout", "0")
	if !strings.Contains(result, "refused") {
		t.Fatalf("a person running `nimbus work` lifted the ladder for a peer's command:\n%s", result)
	}
}

// A command with side effects must not run twice because the device died
// between running it and reporting. Acknowledging first makes that at-most-once.
func TestAnExecRequestIsNotRunTwice(t *testing.T) {
	laptop, desktop := twoDevices(t)

	desktop.run("autonomy", "l3")
	desktop.run("autonomy", "allow", "echo")
	desktop.run("state", "sync")

	laptop.run("autonomy", "l2")
	laptop.run("exec", "studio", "echo once")
	laptop.run("state", "sync")

	desktop.run("state", "sync")
	desktop.run("work")

	// Second drain: the request is acknowledged, so there is nothing left.
	again := desktop.run("work")
	if !strings.Contains(again, "nothing to do") {
		t.Errorf("the same request was picked up twice:\n%s", again)
	}
}

// A dry run has to answer "would this be permitted" without doing it, or the
// only way to find out is to find out the hard way.
func TestDryRunShowsTheVerdictWithoutRunning(t *testing.T) {
	laptop, desktop := twoDevices(t)

	desktop.run("autonomy", "l3")
	desktop.run("autonomy", "allow", "echo")
	desktop.run("state", "sync")

	laptop.run("autonomy", "l2")
	laptop.run("exec", "studio", "echo dry")
	laptop.run("state", "sync")

	desktop.run("state", "sync")
	dry := desktop.run("work", "--dry-run")
	if !strings.Contains(dry, "would run") {
		t.Fatalf("dry run did not report a verdict:\n%s", dry)
	}
	if !strings.Contains(dry, "echo dry") {
		t.Errorf("dry run did not name the command:\n%s", dry)
	}

	// Still pending: a dry run must not acknowledge anything.
	real := desktop.run("work")
	if !strings.Contains(real, "exit 0") {
		t.Errorf("the dry run consumed the request:\n%s", real)
	}
}

// Sending is a dispatch (L2), and the ladder caps what happens *unattended*.
// A person typing this is the confirmation the ladder would have asked for, so
// the case that must be blocked is Claude reaching for a peer on its own.
func TestClaudeCannotQueueWorkOnAPeerBelowL2(t *testing.T) {
	laptop, _ := twoDevices(t)

	laptop.run("autonomy", "l1")

	// Attended false: exactly what the MCP server passes for a tool call.
	out, err := laptop.tryUnattended("exec", "studio", "echo hello")
	if err == nil {
		t.Fatalf("an L1 device queued work on a peer with nobody watching:\n%s", out)
	}
	if !strings.Contains(err.Error(), "peer.dispatch") {
		t.Errorf("the refusal does not name what was needed: %v", err)
	}

	// The same command with a person at the keyboard is not capped, which is
	// the whole distinction the ladder is built on.
	laptop.run("exec", "studio", "echo hello")
}

// A dispatched task starts a Claude session, which no allowlist bounds. That is
// a much bigger grant than running one named command, so it takes --act on top
// of the level rather than following from the level alone.
//
// Both cases here stop before a session could start, deliberately: a test that
// reached that far would launch a real Claude on whatever machine ran it.
func TestADispatchedSessionNeedsMoreThanTheLevel(t *testing.T) {
	laptop, desktop := twoDevices(t)

	laptop.run("task", "new", "fix-build", "--goal", "the nightly build is red")
	laptop.run("state", "sync")
	desktop.run("state", "sync")
	laptop.run("dispatch", "studio", "fix-build", "--note", "you have the toolchain")
	laptop.run("state", "sync")
	desktop.run("state", "sync")

	// Without --act, a dispatch is left for a person even at L3.
	desktop.run("autonomy", "l3")
	left := desktop.run("work")
	if !strings.Contains(left, "nothing to do") {
		t.Fatalf("a dispatch was acted on without --act:\n%s", left)
	}

	// With --act but below L3, it is refused and the sender is told why.
	desktop.run("autonomy", "l2")
	desktop.run("work", "--act")
	desktop.run("state", "sync")
	laptop.run("state", "sync")

	out := laptop.run("inbox", "--all")
	if !strings.Contains(out, "peer.exec") {
		t.Errorf("the refusal did not travel back to the device that dispatched:\n%s", out)
	}
}

// The allowlist is state, and state that cannot be inspected is state nobody
// trusts. It shows up where the level does.
func TestTheAllowlistIsVisibleAndReversible(t *testing.T) {
	_, desktop := twoDevices(t)

	// Not the command the empty-list hint suggests, so a match here is the
	// allowlist rather than the placeholder text.
	desktop.run("autonomy", "allow", "journalctl -u nimbus")
	shown := desktop.run("autonomy")
	if !strings.Contains(shown, "journalctl -u nimbus") {
		t.Fatalf("the allowlist is not shown with the level:\n%s", shown)
	}

	desktop.run("autonomy", "deny", "journalctl -u nimbus")
	after := desktop.run("autonomy")
	if strings.Contains(after, "journalctl -u nimbus") {
		t.Errorf("denying a command did not remove it:\n%s", after)
	}
	if !strings.Contains(after, "nothing") {
		t.Errorf("an empty allowlist does not say so:\n%s", after)
	}
}
