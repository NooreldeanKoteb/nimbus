package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/nkoteb/nimbus/internal/device"
)

// fakeDevice runs nimbus commands as one machine in the fleet: its own
// NIMBUS_HOME, its own node id, sharing a remote with the others.
//
// Overriding the node id is what makes a two-device test possible on one host,
// since both would otherwise derive the same id from /etc/machine-id.
type fakeDevice struct {
	t    *testing.T
	home string
	id   string
}

func newDevice(t *testing.T, name string) *fakeDevice {
	t.Helper()
	return &fakeDevice{t: t, home: t.TempDir(), id: name}
}

// run executes a nimbus command and fails the test if it errors.
func (d *fakeDevice) run(args ...string) string {
	d.t.Helper()
	out, err := d.try(args...)
	if err != nil {
		d.t.Fatalf("%s: nimbus %s: %v\n%s", d.id, strings.Join(args, " "), err, out)
	}
	return out
}

// try executes a nimbus command and returns its error for inspection.
func (d *fakeDevice) try(args ...string) (string, error) {
	d.t.Helper()
	d.t.Setenv("NIMBUS_HOME", d.home)
	d.t.Setenv(device.NodeIDEnv, d.id)

	var buf bytes.Buffer
	err := Run(context.Background(), args, &buf, &buf)
	return buf.String(), err
}

// bareRemote is a shared origin standing in for the user's private git repo.
func bareRemote(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if _, err := git.PlainInit(path, true); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole point of Phase 2: write on one device, continue on another.
func TestHandoffBetweenTwoDevices(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("task", "new", "auth-refactor",
		"--goal", "move session auth to JWT",
		"--branch", "feat/jwt")
	laptop.run("task", "note", "client-side token refresh works")
	laptop.run("task", "handoff",
		"--done", "client token refresh",
		"--next", "server verification middleware",
		"--outside-git", "postgres in docker on :5432",
		"--release")

	// A second machine that has never seen this work clones and picks it up.
	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)

	listed := desktop.run("task", "list")
	if !strings.Contains(listed, "auth-refactor") {
		t.Fatalf("desktop cannot see the task:\n%s", listed)
	}

	resumed := desktop.run("resume", "auth-refactor")
	for _, want := range []string{
		"move session auth to JWT",        // the goal
		"feat/jwt",                        // where the code is
		"client token refresh",            // what the laptop finished
		"server verification middleware",  // what to do next
		"postgres in docker on :5432",     // state git will not carry
		"client-side token refresh works", // the progress timeline
	} {
		if !strings.Contains(resumed, want) {
			t.Errorf("resume output is missing %q:\n%s", want, resumed)
		}
	}

	// The claim must have moved, or the laptop still looks like the owner.
	shown := desktop.run("task", "show", "auth-refactor")
	if !strings.Contains(shown, "held by  desktop") {
		t.Errorf("desktop did not take the claim:\n%s", shown)
	}

	// And the laptop must see that when it syncs back.
	backOnLaptop := laptop.run("task", "list")
	if !strings.Contains(backOnLaptop, "held by desktop") {
		t.Errorf("laptop does not see the handoff:\n%s", backOnLaptop)
	}
}

// Coming back to the same machine should offer the real conversation; arriving
// on a new one cannot, because the transcript is not there.
func TestSessionBindingIsPerDevice(t *testing.T) {
	origin := bareRemote(t)
	claudeRoot := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeRoot)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("task", "new", "port", "--goal", "port the server")

	first := laptop.run("resume", "port")
	if !strings.Contains(first, "--session-id") {
		t.Fatalf("first resume did not assign a session:\n%s", first)
	}

	// Fabricate the transcript the session would have written.
	sessionID := sessionIDFrom(t, first)
	dir := filepath.Join(claudeRoot, "projects", "-home-someone")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	again := laptop.run("resume", "port")
	if !strings.Contains(again, "--resume "+sessionID) {
		t.Errorf("returning to the same device did not offer to resume:\n%s", again)
	}

	// A different machine has no transcript, so it must start fresh rather
	// than promise continuity it cannot deliver.
	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)

	out := desktop.run("resume", "port", "--force")
	if strings.Contains(out, "--resume "+sessionID) {
		t.Errorf("a new device offered to resume a transcript it does not have:\n%s", out)
	}
	if !strings.Contains(out, "--session-id") {
		t.Errorf("new device was not given a fresh session:\n%s", out)
	}
}

// sessionIDFrom pulls the assigned uuid out of resume's output.
func sessionIDFrom(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`[0-9a-f-]{36}`).FindString(out)
	if m == "" {
		t.Fatalf("no session id in output:\n%s", out)
	}
	return m
}

// A device that lacks what the task needs must not silently pick it up.
func TestResumeRefusesWhenRequirementsAreUnmet(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("task", "new", "train-model",
		"--goal", "train the ranking model",
		"--needs", "gpu:nonexistent-vendor")
	laptop.run("task", "release")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)

	out, err := desktop.try("resume", "train-model")
	if err == nil {
		t.Fatalf("resume succeeded on a device missing the requirement:\n%s", out)
	}
	if !strings.Contains(err.Error(), "gpu:nonexistent-vendor") {
		t.Errorf("error = %v, want it to name the missing capability", err)
	}

	// --force is the deliberate override, and must work.
	if _, err := desktop.try("resume", "train-model", "--force"); err != nil {
		t.Errorf("resume --force = %v, want the override to work", err)
	}
}

// Taking a task another device is actively holding needs an explicit decision.
func TestResumeRefusesToStealActiveClaim(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("task", "new", "held", "--goal", "work in progress")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)

	out, err := desktop.try("resume", "held")
	if err == nil {
		t.Fatalf("resume stole a live claim without being asked:\n%s", out)
	}
	if !strings.Contains(err.Error(), "laptop") {
		t.Errorf("error = %v, want it to name the holding device", err)
	}

	if _, err := desktop.try("resume", "held", "--force"); err != nil {
		t.Errorf("resume --force = %v, want the override to work", err)
	}
}

// --brief is the read-only path: it must not move the claim.
func TestResumeBriefDoesNotClaim(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("task", "new", "held", "--goal", "work in progress")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)

	brief := desktop.run("resume", "held", "--brief")
	if !strings.Contains(brief, "work in progress") {
		t.Errorf("--brief did not print the task:\n%s", brief)
	}

	shown := desktop.run("task", "show", "held")
	if !strings.Contains(shown, "held by  laptop") {
		t.Errorf("--brief moved the claim:\n%s", shown)
	}
}

// The SessionStart hook runs on every Claude session. If it can fail, it turns
// every session on the device into an error report.
func TestContextNeverFails(t *testing.T) {
	d := newDevice(t, "solo")

	// No state repo at all — the worst case, on a device mid-setup.
	out, err := d.try("context")
	if err != nil {
		t.Errorf("context on a bare device = %v, want success\n%s", err, out)
	}
	if !strings.Contains(out, "Nimbus context") {
		t.Errorf("context printed nothing usable:\n%s", out)
	}

	// With a repo but no task, it must still describe the device.
	d.run("state", "init")
	out = d.run("context")
	if !strings.Contains(out, "No task claimed") {
		t.Errorf("context did not report the absence of a task:\n%s", out)
	}
	if !strings.Contains(out, "## Device") {
		t.Errorf("context dropped the device section:\n%s", out)
	}
}

func TestTaskNoteRequiresText(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "x", "--goal", "g")

	if _, err := d.try("task", "note"); err == nil {
		t.Error("task note accepted an empty note")
	}
}

// Without a claim there is no way to guess what the command applies to, and
// guessing wrong writes to the wrong task.
func TestTaskCommandsNeedATaskWhenNoneIsHeld(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "x", "--goal", "g")
	d.run("task", "release")

	out, err := d.try("task", "note", "something")
	if err == nil {
		t.Fatalf("note was recorded with no task held:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--task") {
		t.Errorf("error = %v, want it to suggest --task", err)
	}
}

func TestTaskDoneClosesAndUnclaims(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "ship-it", "--goal", "ship the thing")
	d.run("task", "done")

	if listed := d.run("task", "list"); strings.Contains(listed, "ship-it") {
		t.Errorf("a finished task still shows as open:\n%s", listed)
	}
	if listed := d.run("task", "list", "--all"); !strings.Contains(listed, "ship-it") {
		t.Errorf("--all did not include the finished task:\n%s", listed)
	}
}

func TestTaskNewRejectsBadID(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	if out, err := d.try("task", "new", "../escape", "--goal", "g"); err == nil {
		t.Errorf("accepted a task id that escapes the tasks directory:\n%s", out)
	}
}
