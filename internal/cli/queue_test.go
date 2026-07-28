package cli

import (
	"strings"
	"testing"
)

// The failure work-stealing exists to prevent: a device goes offline holding a
// task, and nothing else in the fleet can continue it.
func TestALapsedClaimCanBeTakenByAnotherDevice(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("alias", "kali-thinkpad")
	laptop.run("doctor", "--publish")
	laptop.run("task", "new", "port-server", "--goal", "port the server")
	laptop.run("state", "sync")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)
	desktop.run("alias", "studio")
	desktop.run("doctor", "--publish")

	// A live claim is not up for grabs, however much the other device wants it.
	if out, err := desktop.try("task", "steal", "port-server"); err == nil {
		t.Fatalf("a live claim was stolen:\n%s", out)
	} else if !strings.Contains(err.Error(), "release") {
		t.Errorf("the refusal does not say how to hand it over properly: %v", err)
	}

	// With a lease short enough to have lapsed, it is available.
	queue := desktop.run("task", "queue", "--lease", "1ns")
	if !strings.Contains(queue, "port-server") {
		t.Fatalf("the lapsed task is not in the queue:\n%s", queue)
	}
	if !strings.Contains(queue, "kali-thinkpad") {
		t.Errorf("the queue does not say who has gone quiet:\n%s", queue)
	}

	out := desktop.run("task", "steal", "port-server", "--lease", "1ns")
	if !strings.Contains(out, "taken from kali-thinkpad") {
		t.Fatalf("the steal did not report where the work came from:\n%s", out)
	}

	// And the timeline distinguishes a steal from an ordinary pickup, because a
	// person reading it later needs to tell the two apart.
	shown := desktop.run("task", "show", "port-server")
	if !strings.Contains(shown, "steal") {
		t.Errorf("the steal is not in the timeline:\n%s", shown)
	}
	if !strings.Contains(shown, "held by  studio") {
		t.Errorf("the claim did not move:\n%s", shown)
	}
}

// Offering GPU work to a laptop only produces a claim that has to be handed
// back, so the queue is filtered by what this device can actually run.
func TestTheQueueOnlyOffersWorkThisDeviceCanDo(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "train-model", "--goal", "train it", "--needs", "gpu:nonexistent-vendor")
	d.run("task", "release")

	if out := d.run("task", "queue"); strings.Contains(out, "train-model") {
		t.Errorf("work this device cannot run was offered:\n%s", out)
	}
	if out := d.run("task", "queue", "--any"); !strings.Contains(out, "train-model") {
		t.Errorf("--any did not show the whole fleet's open work:\n%s", out)
	}
}

// Claude reaches the queue through the mesh tools, not only through the CLI.
func TestQueueAndStealAreReachableFromTheMCPTools(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "port-server", "--goal", "port the server")
	d.run("task", "release")

	out, ok := d.call(t, "nimbus_task_queue", nil)
	if !ok {
		t.Fatalf("task_queue failed: %s", out)
	}
	if !strings.Contains(out, "port-server") {
		t.Fatalf("the unclaimed task is not in the queue:\n%s", out)
	}

	if out, ok := d.call(t, "nimbus_task_steal", map[string]any{"task": "port-server"}); !ok {
		t.Fatalf("task_steal failed: %s", out)
	}
	if shown := d.run("task", "show", "port-server"); !strings.Contains(shown, "held by  solo") {
		t.Errorf("the claim did not move:\n%s", shown)
	}
}
