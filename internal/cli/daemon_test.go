package cli

import (
	"strings"
	"testing"
)

// One cycle is the whole loop minus the timer, so it is what proves the daemon
// actually moves state rather than just staying alive.
func TestDaemonCycleSyncsAndReportsMail(t *testing.T) {
	origin := bareRemote(t)

	laptop := newDevice(t, "laptop")
	laptop.run("state", "init", "--remote", origin)
	laptop.run("doctor", "--publish")

	desktop := newDevice(t, "desktop")
	desktop.run("state", "clone", origin)
	desktop.run("doctor", "--publish")

	laptop.run("state", "sync")
	laptop.run("send", "desktop", "the integration suite is red")

	// The desktop has not pulled yet, so only a daemon cycle can surface this.
	out := desktop.run("daemon", "run", "--once")
	if !strings.Contains(out, "NEW MESSAGE") {
		t.Fatalf("daemon did not announce the new message:\n%s", out)
	}
	if !strings.Contains(out, "integration suite is red") {
		t.Errorf("announcement is missing the message body:\n%s", out)
	}

	// A second cycle must not re-announce what it already reported, or an
	// always-on device shouts the same message every minute forever.
	again := desktop.run("daemon", "run", "--once")
	if strings.Contains(again, "NEW MESSAGE") {
		t.Errorf("daemon re-announced an old message:\n%s", again)
	}
	if !strings.Contains(again, "unread") {
		t.Errorf("daemon lost track of the still-unread message:\n%s", again)
	}
}

// Being offline is the normal state of a laptop, not a failure.
func TestDaemonCycleToleratesNoOrigin(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	if _, err := d.try("daemon", "run", "--once"); err != nil {
		t.Errorf("daemon failed with no origin configured: %v", err)
	}
}

func TestDaemonRefusesWithoutStateRepo(t *testing.T) {
	d := newDevice(t, "solo")

	// Installing a service that starts at login only to fail every minute is
	// worse than not installing it.
	if _, err := d.try("daemon", "run", "--once"); err == nil {
		t.Error("daemon ran with no state repo")
	}
	if _, err := d.try("daemon", "install"); err == nil {
		t.Error("daemon install succeeded with no state repo")
	}
}

func TestDaemonStatusWhenNotInstalled(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	out := d.run("daemon", "status")
	if !strings.Contains(out, "not installed") {
		t.Errorf("status = %q, want it to say the daemon is not installed", out)
	}
}

func TestDaemonUsageErrors(t *testing.T) {
	d := newDevice(t, "solo")
	d.run("state", "init")

	if _, err := d.try("daemon"); err == nil {
		t.Error("daemon with no subcommand was accepted")
	}
	if _, err := d.try("daemon", "frobnicate"); err == nil {
		t.Error("unknown daemon subcommand was accepted")
	}
}
