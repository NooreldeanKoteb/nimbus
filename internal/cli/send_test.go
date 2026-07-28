package cli

import (
	"strings"
	"testing"
)

// The capability the mesh exists for: one device asks another to do something,
// and the answer comes back.
func TestMessageRoundTripBetweenDevices(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("alias", "kali-thinkpad")
	laptop.run("doctor", "--publish")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)
	desktop.run("alias", "studio")
	desktop.run("doctor", "--publish")

	// Addressed by name, not by node id.
	laptop.run("state", "sync")
	sent := laptop.run("send", "studio", "the server build is failing on libssl")
	if !strings.Contains(sent, "studio") {
		t.Fatalf("send did not confirm the recipient:\n%s", sent)
	}

	inbox := desktop.run("inbox")
	if !strings.Contains(inbox, "libssl") {
		t.Fatalf("message did not reach the desktop:\n%s", inbox)
	}
	if !strings.Contains(inbox, "kali-thinkpad") {
		t.Errorf("sender is not named in the inbox:\n%s", inbox)
	}

	// Acknowledging carries an answer back to the sender.
	id := sessionIDFromInbox(t, inbox)
	desktop.run("ack", id, "--note", "installed libssl-dev, build is green")

	out := laptop.run("outbox")
	if !strings.Contains(out, "installed libssl-dev") {
		t.Errorf("the reply did not reach the sender:\n%s", out)
	}
	if !strings.Contains(out, "read ") {
		t.Errorf("outbox does not show the message as read:\n%s", out)
	}
}

// sessionIDFromInbox pulls the message id out of inbox output.
func sessionIDFromInbox(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if _, id, ok := strings.Cut(strings.TrimSpace(line), "id: "); ok {
			return strings.TrimSpace(id)
		}
	}
	t.Fatalf("no message id in output:\n%s", out)
	return ""
}

// Unread mail has to reach the session, or nobody reads it.
func TestUnreadMessagesAppearInContext(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("doctor", "--publish")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)
	desktop.run("doctor", "--publish")

	laptop.run("state", "sync")
	laptop.run("send", "desktop", "please rerun the integration suite")

	out := desktop.run("context")
	if !strings.Contains(out, "## Messages") {
		t.Fatalf("context has no messages section:\n%s", out)
	}
	if !strings.Contains(out, "rerun the integration suite") {
		t.Errorf("message body missing from context:\n%s", out)
	}

	// Once acknowledged it should stop crowding every future session.
	desktop.run("inbox", "--ack")
	if out := desktop.run("context"); strings.Contains(out, "## Messages") {
		t.Errorf("acknowledged message is still in the context:\n%s", out)
	}
}

// Dispatch is the "act as an agent" path: release here, ask there.
func TestDispatchHandsOffTaskAndClaim(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("doctor", "--publish")
	laptop.run("task", "new", "port-server", "--goal", "port the server")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)
	desktop.run("doctor", "--publish")

	laptop.run("state", "sync")
	out := laptop.run("dispatch", "desktop", "port-server", "--note", "you have the database")
	if !strings.Contains(out, "dispatched port-server") {
		t.Fatalf("dispatch did not confirm:\n%s", out)
	}

	// The claim must be released, or the target cannot pick it up without
	// forcing past a claim the sender no longer wants.
	shown := laptop.run("task", "show", "port-server")
	if !strings.Contains(shown, "held by  nobody") {
		t.Errorf("dispatch left the task claimed:\n%s", shown)
	}

	// The target sees the request and can resume without --force.
	if inbox := desktop.run("inbox"); !strings.Contains(inbox, "you have the database") {
		t.Errorf("dispatch note did not arrive:\n%s", inbox)
	}
	if _, err := desktop.try("resume", "port-server"); err != nil {
		t.Errorf("target could not resume a dispatched task: %v", err)
	}
}

// Routing on capability is why profiles are published: sending GPU work to a
// machine without one wastes a round trip and strands the task.
func TestDispatchRefusesIncapableDevice(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("doctor", "--publish")
	laptop.run("task", "new", "train", "--goal", "train it", "--needs", "gpu:nonexistent-vendor")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)
	desktop.run("doctor", "--publish")

	laptop.run("state", "sync")
	out, err := laptop.try("dispatch", "desktop", "train")
	if err == nil {
		t.Fatalf("dispatched GPU work to a device without one:\n%s", out)
	}
	if !strings.Contains(err.Error(), "gpu:nonexistent-vendor") {
		t.Errorf("error = %v, want it to name the missing capability", err)
	}

	if _, err := laptop.try("dispatch", "desktop", "train", "--force"); err != nil {
		t.Errorf("--force did not override: %v", err)
	}
}

func TestSendRejectsUnknownAndSelf(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("doctor", "--publish")

	if _, err := d.try("send", "nonexistent-device", "hello"); err == nil {
		t.Error("send invented a recipient")
	}
	if _, err := d.try("send", "solo", "talking to myself"); err == nil {
		t.Error("send allowed a message to this same device")
	}
	if _, err := d.try("send"); err == nil {
		t.Error("send accepted no arguments")
	}
}

func TestEmptyInboxAndOutbox(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	if out := d.run("inbox"); !strings.Contains(out, "no messages") {
		t.Errorf("empty inbox said %q", out)
	}
	if out := d.run("outbox"); !strings.Contains(out, "nothing sent") {
		t.Errorf("empty outbox said %q", out)
	}
}
