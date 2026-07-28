package install

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// Privilege describes how (or whether) this process can install system packages.
type Privilege string

const (
	PrivRoot Privilege = "root"
	PrivSudo Privilege = "sudo"
	PrivNone Privilege = "none"
)

// stdinIsTerminal reports whether we can prompt the user at all. Detected via
// the file mode rather than a terminal library, to keep the dependency surface
// at zero.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// packageCommands maps a package manager to the argv that installs packages
// non-interactively, plus the argv that removes them again for rollback.
var packageCommands = map[string]struct {
	refresh []string
	install []string
	remove  []string
}{
	"apt":    {[]string{"apt-get", "update", "-qq"}, []string{"apt-get", "install", "-y", "-qq"}, []string{"apt-get", "remove", "-y"}},
	"dnf":    {nil, []string{"dnf", "install", "-y", "-q"}, []string{"dnf", "remove", "-y"}},
	"pacman": {nil, []string{"pacman", "-Sy", "--noconfirm"}, []string{"pacman", "-R", "--noconfirm"}},
	"zypper": {nil, []string{"zypper", "--non-interactive", "install"}, []string{"zypper", "--non-interactive", "remove"}},
	"apk":    {nil, []string{"apk", "add", "--no-cache"}, []string{"apk", "del"}},
	"brew":   {nil, []string{"brew", "install"}, []string{"brew", "uninstall"}},
	"winget": {nil,
		[]string{"winget", "install", "-e", "--silent", "--accept-package-agreements", "--accept-source-agreements", "--id"},
		[]string{"winget", "uninstall", "-e", "--silent", "--id"}},
	"choco": {nil, []string{"choco", "install", "-y"}, []string{"choco", "uninstall", "-y"}},
}

// needsElevation reports whether a manager requires administrator rights.
// brew refuses to run as root, and winget installs per-user by default.
func needsElevation(pkgMgr string) bool {
	switch pkgMgr {
	case "brew", "winget":
		return false
	default:
		return true
	}
}

// InstallPackages installs system packages using the detected package manager.
//
// The returned Step carries a Rollback command so the audit trail records how
// to undo the change before it is applied (DESIGN.md §9a).
func InstallPackages(ctx context.Context, pkgMgr string, packages []string) Step {
	step := Step{Name: "packages"}
	if len(packages) == 0 {
		step.Skipped = true
		step.Detail = "nothing to install"
		return step
	}

	step.Name = "pkg:" + strings.Join(packages, ",")

	cmds, ok := packageCommands[pkgMgr]
	if !ok {
		step.Skipped = true
		if pkgMgr == "" {
			step.Detail = "no package manager detected"
		} else {
			step.Detail = "unsupported package manager: " + pkgMgr
		}
		return step
	}

	priv := DetectPrivilege()
	if priv == PrivNone && needsElevation(pkgMgr) {
		step.Skipped = true
		step.Detail = fmt.Sprintf("need administrator access to install %s",
			strings.Join(packages, ", "))
		return step
	}

	// Package ids differ by ecosystem; translate before building the command.
	resolved := make([]string, 0, len(packages))
	for _, p := range packages {
		resolved = append(resolved, resolvePackageName(pkgMgr, p))
	}

	step.Rollback = strings.Join(append(cmds.remove, resolved...), " ")

	if cmds.refresh != nil {
		refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		// A failed refresh is not fatal; the install may still succeed from cache.
		_, _ = runElevatedOutput(refreshCtx, priv, pkgMgr, cmds.refresh)
		cancel()
	}

	installCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if out, err := runElevatedOutput(installCtx, priv, pkgMgr, append(cmds.install, resolved...)); err != nil {
		step.Err = fmt.Errorf("%w: %s", err, lastLines(out, 3))
		return step
	}

	step.Changed = true
	step.Detail = fmt.Sprintf("installed via %s (%s)", pkgMgr, priv)
	return step
}

// packageAliases maps nimbus's generic package names to ecosystem-specific
// ids. Only entries that actually differ are listed.
var packageAliases = map[string]map[string]string{
	"winget": {
		"curl":  "cURL.cURL",
		"unzip": "GnuWin32.UnZip",
		"git":   "Git.Git",
	},
}

func resolvePackageName(pkgMgr, name string) string {
	if aliases, ok := packageAliases[pkgMgr]; ok {
		if resolved, ok := aliases[name]; ok {
			return resolved
		}
	}
	return name
}

// claudePrereqs are what the upstream Claude Code installer itself needs.
// Nimbus promises a device with nothing installed will work, so it has to
// satisfy its dependencies' dependencies too.
var claudePrereqs = []struct {
	// satisfiedBy lists executables that each fulfil this requirement.
	satisfiedBy []string
	pkg         string
	// unixOnly marks requirements of the sh installer, which Windows does not use.
	unixOnly bool
}{
	{[]string{"curl", "wget"}, "curl", false},
	{[]string{"bash"}, "bash", true},
	{[]string{"unzip"}, "unzip", true},
}

// missingPrereqs lists the Claude installer requirements absent on this device.
func missingPrereqs() []string {
	var missing []string
	for _, req := range claudePrereqs {
		if req.unixOnly && runtime.GOOS == "windows" {
			continue
		}
		satisfied := false
		for _, bin := range req.satisfiedBy {
			if _, err := lookPath(bin); err == nil {
				satisfied = true
				break
			}
		}
		if !satisfied {
			missing = append(missing, req.pkg)
		}
	}
	return missing
}

// NeedsPrereqs reports whether anything must be installed at all. Callers use
// this to avoid prompting for a password they will not end up needing.
func NeedsPrereqs() bool {
	return len(missingPrereqs()) > 0 && ClaudeCodePath() == ""
}

// EnsureClaudePrereqs installs whatever the Claude installer needs and is
// missing. It is a no-op when everything is already present.
func EnsureClaudePrereqs(ctx context.Context, pkgMgr string) []Step {
	missing := missingPrereqs()
	if len(missing) == 0 {
		return []Step{{Name: "prereqs", Skipped: true, Detail: "all present"}}
	}
	return []Step{InstallPackages(ctx, pkgMgr, missing)}
}
