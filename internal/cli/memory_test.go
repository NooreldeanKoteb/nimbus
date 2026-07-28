package cli

import (
	"strings"
	"testing"
)

func TestMemoryAddAndList(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	out := d.run("memory", "add", "evdi-dkms breaks on every kernel upgrade")
	if !strings.Contains(out, "scratch") {
		t.Errorf("memory add did not report the tier:\n%s", out)
	}
	// Scratch expires, and the user has to be told how to keep it.
	if !strings.Contains(out, "promote") {
		t.Errorf("no hint about promoting before expiry:\n%s", out)
	}

	listed := d.run("memory", "list")
	if !strings.Contains(listed, "evdi-dkms") {
		t.Errorf("memory list is missing the entry:\n%s", listed)
	}
}

func TestMemoryAddRequiresText(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	if _, err := d.try("memory", "add"); err == nil {
		t.Error("memory add accepted an empty memory")
	}
}

// Promotion is the deliberate act that makes something survive.
func TestMemoryPromoteStopsExpiry(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("memory", "add", "nouveau only on this box")

	out := d.run("memory", "promote", "nouveau-only-on-this-box")
	if !strings.Contains(out, "long-term") || !strings.Contains(out, "not expire") {
		t.Errorf("promote did not report durability:\n%s", out)
	}

	shown := d.run("memory", "show", "nouveau-only-on-this-box")
	if !strings.Contains(shown, "long-term") {
		t.Errorf("memory is not long-term after promotion:\n%s", shown)
	}
	if strings.Contains(shown, "expires") {
		t.Errorf("promoted memory still has an expiry:\n%s", shown)
	}
}

// Long-term memory is the tier meant to be shared, so it has to actually
// reach the other machine.
func TestLongTermMemoryReachesOtherDevices(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("memory", "add", "--long-term", "the build needs 16GB of RAM")
	laptop.run("memory", "add", "a passing thought")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)

	listed := desktop.run("memory", "list", "--tier", "long-term")
	if !strings.Contains(listed, "16GB") {
		t.Errorf("durable fact did not reach the other device:\n%s", listed)
	}
}

func TestMemorySearch(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("memory", "add", "--tags", "gpu,driver", "nouveau is the only safe driver here")
	d.run("memory", "add", "unrelated note about coffee")

	found := d.run("memory", "search", "gpu")
	if !strings.Contains(found, "nouveau") {
		t.Errorf("tag search did not find the entry:\n%s", found)
	}
	if strings.Contains(found, "coffee") {
		t.Errorf("search returned an unrelated entry:\n%s", found)
	}
}

// A note taken while working a task should be findable by that task without
// the user having to say so.
func TestMemoryInheritsActiveTask(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "port-server", "--goal", "port it")
	d.run("memory", "add", "the handler is stubbed out")

	shown := d.run("memory", "show", "the-handler-is-stubbed-out")
	if !strings.Contains(shown, "port-server") {
		t.Errorf("memory was not attributed to the active task:\n%s", shown)
	}
}

// Memory is only useful if it reaches the session.
func TestMemoryAppearsInContext(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("task", "new", "port-server", "--goal", "port it")
	d.run("memory", "add", "--long-term", "postgres runs on port 5433 here")
	d.run("memory", "add", "the handler is stubbed out")

	out := d.run("context")
	if !strings.Contains(out, "## Memory") {
		t.Fatalf("context has no memory section:\n%s", out)
	}
	if !strings.Contains(out, "postgres runs on port 5433") {
		t.Errorf("durable fact missing from context:\n%s", out)
	}
	if !strings.Contains(out, "the handler is stubbed out") {
		t.Errorf("task note missing from context:\n%s", out)
	}
}

func TestMemoryForget(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")
	d.run("memory", "add", "temporary thought")

	d.run("memory", "forget", "temporary-thought")

	if listed := d.run("memory", "list"); strings.Contains(listed, "temporary thought") {
		t.Errorf("forgotten memory is still listed:\n%s", listed)
	}
	// A deletion is the one change the files can no longer explain, so it has
	// to be in the audit log.
	if out := d.run("audit", "-n", "0"); !strings.Contains(out, "memory.forget") {
		t.Errorf("deletion was not audited:\n%s", out)
	}
}

func TestMemoryShowMissing(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	if _, err := d.try("memory", "show", "never-written"); err == nil {
		t.Error("memory show invented an entry")
	}
	if _, err := d.try("memory", "show", "../escape"); err == nil {
		t.Error("memory show accepted a path escape")
	}
}

func TestMemoryExpireOnEmptyStore(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	if out := d.run("memory", "expire"); !strings.Contains(out, "nothing has expired") {
		t.Errorf("expire on an empty store said %q", out)
	}
}
