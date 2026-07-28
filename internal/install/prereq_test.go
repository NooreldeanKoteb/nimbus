package install

import (
	"strings"
	"testing"
)

func TestInstallPackagesNoopOnEmptyList(t *testing.T) {
	step := InstallPackages(t.Context(), "apt", nil)
	if !step.Skipped {
		t.Errorf("step = %+v, want skipped", step)
	}
	if step.Err != nil {
		t.Errorf("empty install should not error, got %v", step.Err)
	}
}

func TestInstallPackagesRejectsUnknownManager(t *testing.T) {
	step := InstallPackages(t.Context(), "sillypkg", []string{"curl"})
	if !step.Skipped || step.Err != nil {
		t.Fatalf("step = %+v, want a clean skip", step)
	}
	if !strings.Contains(step.Detail, "unsupported") {
		t.Errorf("detail = %q, want it to name the problem", step.Detail)
	}
}

func TestInstallPackagesReportsMissingPackageManager(t *testing.T) {
	step := InstallPackages(t.Context(), "", []string{"curl"})
	if !step.Skipped {
		t.Fatalf("step = %+v, want skipped", step)
	}
	if !strings.Contains(step.Detail, "no package manager") {
		t.Errorf("detail = %q", step.Detail)
	}
}

// An unprivileged device must say why it cannot install, not fail obscurely.
func TestInstallPackagesExplainsMissingPrivilege(t *testing.T) {
	if DetectPrivilege() != PrivNone {
		t.Skip("test runner has root or passwordless sudo")
	}

	step := InstallPackages(t.Context(), "apt", []string{"curl"})
	if !step.Skipped {
		t.Fatalf("step = %+v, want skipped without privilege", step)
	}
	// Wording is deliberately platform-neutral: the same path covers sudo on
	// Unix and UAC on Windows.
	if !strings.Contains(step.Detail, "administrator") {
		t.Errorf("detail = %q, want it to explain the privilege requirement", step.Detail)
	}
	if !strings.Contains(step.Detail, "curl") {
		t.Errorf("detail = %q, want it to name the blocked package", step.Detail)
	}
}

// brew refuses to run as root and winget installs per-user, so demanding
// elevation for them would break setup on machines that need neither.
func TestNeedsElevationByManager(t *testing.T) {
	for _, mgr := range []string{"apt", "dnf", "pacman", "zypper", "apk", "choco"} {
		if !needsElevation(mgr) {
			t.Errorf("needsElevation(%q) = false, want true", mgr)
		}
	}
	for _, mgr := range []string{"brew", "winget"} {
		if needsElevation(mgr) {
			t.Errorf("needsElevation(%q) = true, want false", mgr)
		}
	}
}

// Package ids differ per ecosystem; an untranslated name would install nothing
// on Windows while reporting success.
func TestResolvePackageName(t *testing.T) {
	if got := resolvePackageName("winget", "curl"); got != "cURL.cURL" {
		t.Errorf("resolvePackageName(winget, curl) = %q, want cURL.cURL", got)
	}
	if got := resolvePackageName("apt", "curl"); got != "curl" {
		t.Errorf("resolvePackageName(apt, curl) = %q, want curl", got)
	}
	if got := resolvePackageName("winget", "unmapped"); got != "unmapped" {
		t.Errorf("unmapped name should pass through, got %q", got)
	}
}

// Windows uses the PowerShell installer, so bash and unzip must not be
// demanded there — they would fail to install and block setup for no reason.
func TestUnixOnlyPrereqsAreMarked(t *testing.T) {
	var bashUnixOnly, curlUnixOnly bool
	for _, req := range claudePrereqs {
		for _, bin := range req.satisfiedBy {
			switch bin {
			case "bash":
				bashUnixOnly = req.unixOnly
			case "curl":
				curlUnixOnly = req.unixOnly
			}
		}
	}
	if !bashUnixOnly {
		t.Error("bash should be marked unix-only; Windows uses install.ps1")
	}
	if curlUnixOnly {
		t.Error("curl is needed on every platform and must not be unix-only")
	}
}

func TestNeedsPrereqsIsFalseWhenClaudePresent(t *testing.T) {
	// A device that already has Claude never needs a password prompt, which
	// is what keeps a second `nimbus init` non-interactive.
	if ClaudeCodePath() != "" && NeedsPrereqs() {
		t.Error("NeedsPrereqs() = true even though claude is already installed")
	}
}

func TestDetectPrivilegeReturnsKnownValue(t *testing.T) {
	switch got := DetectPrivilege(); got {
	case PrivRoot, PrivSudo, PrivNone:
	default:
		t.Errorf("DetectPrivilege() = %q, want root, sudo, or none", got)
	}
}

// Every supported manager needs both an install and a remove form, since the
// remove form is what the audit trail records as the rollback.
func TestPackageCommandsAreComplete(t *testing.T) {
	for name, cmds := range packageCommands {
		if len(cmds.install) == 0 {
			t.Errorf("%s: no install command", name)
		}
		if len(cmds.remove) == 0 {
			t.Errorf("%s: no remove command, so rollback cannot be recorded", name)
		}
	}
}

// Every manager that DetectPackageManager can return must be installable here,
// or a device reports a manager nimbus then refuses to use.
func TestEveryDetectedManagerIsSupported(t *testing.T) {
	for _, name := range []string{"apt", "dnf", "pacman", "zypper", "apk", "brew"} {
		if _, ok := packageCommands[name]; !ok {
			t.Errorf("package manager %q is detectable but has no install commands", name)
		}
	}
}

// bash and curl are what the upstream Claude installer itself needs; dropping
// either from this list silently breaks bare-device bootstrap.
func TestClaudePrereqsCoverInstallerNeeds(t *testing.T) {
	needed := map[string]bool{"curl": false, "bash": false}
	for _, req := range claudePrereqs {
		for _, bin := range req.satisfiedBy {
			if _, ok := needed[bin]; ok {
				needed[bin] = true
			}
		}
	}
	for bin, found := range needed {
		if !found {
			t.Errorf("claudePrereqs does not account for %q", bin)
		}
	}
}

func TestEnsureClaudePrereqsIsNoopWhenSatisfied(t *testing.T) {
	// The test host has bash and curl or wget; on a machine that does not,
	// this correctly reports something to install instead.
	steps := EnsureClaudePrereqs(t.Context(), "")
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	if steps[0].Err != nil {
		t.Errorf("prereq check errored: %v", steps[0].Err)
	}
}

// Rollback must be populated before the install runs, so an operator can undo
// a change made by an automated setup they did not watch. Conversely a step
// that never ran must not advertise an undo for a change that never happened.
func TestInstallPackagesRecordsRollbackOnlyWhenItActs(t *testing.T) {
	step := InstallPackages(t.Context(), "apt", []string{"curl"})

	if step.Skipped {
		if step.Rollback != "" {
			t.Errorf("skipped step carries rollback %q", step.Rollback)
		}
		return
	}

	if step.Rollback == "" {
		t.Fatal("no rollback recorded for an attempted package install")
	}
	if !strings.Contains(step.Rollback, "curl") {
		t.Errorf("rollback = %q, want it to name the package", step.Rollback)
	}
	if !strings.Contains(step.Rollback, "remove") {
		t.Errorf("rollback = %q, want it to be a removal command", step.Rollback)
	}
}
