package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ServiceLabel identifies the installed unit on every platform.
const ServiceLabel = "nimbus"

// Service describes an installed background service.
type Service struct {
	Path      string
	Installed bool
	Manager   string
	// Enable is what the user runs to start it, when nimbus cannot do that
	// itself — a headless SSH session has no user D-Bus to talk to systemd on.
	Enable string
}

// Install writes a per-user service unit that runs `nimbus daemon run`.
//
// Per-user, never system-wide: the daemon reads the user's credentials and
// writes the user's state repo, so running it as root would either fail on
// permissions or, worse, succeed and leave root-owned files in a home
// directory.
//
// The unit is also the boot-resume path (DESIGN.md §10). It is what the service
// manager starts at login, so it is the only thing present to notice that a
// task was in flight when the machine went down. act promotes that from writing
// a proposal to starting the session, and is refused anyway below L3.
func Install(interval string, act bool) (*Service, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate nimbus: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, fmt.Errorf("resolve nimbus path: %w", err)
	}

	args := "daemon run --interval " + interval
	if act {
		args += " --act"
	}

	switch runtime.GOOS {
	case "linux":
		return installSystemd(exe, args)
	case "darwin":
		return installLaunchd(exe, args)
	default:
		return nil, fmt.Errorf("no service integration for %s yet — "+
			"run `nimbus daemon run` from a terminal or your own supervisor", runtime.GOOS)
	}
}

func installSystemd(exe, args string) (*Service, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// Restart=always with a delay: the common failure is a laptop waking with
	// no network, which resolves itself.
	unit := fmt.Sprintf(`[Unit]
Description=Nimbus state sync
After=network-online.target

[Service]
Type=simple
ExecStart=%s %s
Restart=always
RestartSec=30

[Install]
WantedBy=default.target
`, exe, args)

	path := filepath.Join(dir, ServiceLabel+".service")
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return nil, err
	}

	svc := &Service{
		Path: path, Installed: true, Manager: "systemd",
		Enable: "systemctl --user enable --now " + ServiceLabel,
	}
	// Best effort: a headless session often has no user D-Bus, and failing to
	// start is not a reason to have failed to install.
	if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err == nil {
		if err := exec.Command("systemctl", "--user", "enable", "--now", ServiceLabel).Run(); err == nil {
			svc.Enable = ""
		}
	}
	return svc, nil
}

func installLaunchd(exe, args string) (*Service, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	label := "dev." + ServiceLabel + ".sync"
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
`, label, plistArgs(exe, args))

	path := filepath.Join(dir, label+".plist")
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return nil, err
	}

	svc := &Service{
		Path: path, Installed: true, Manager: "launchd",
		Enable: "launchctl load " + path,
	}
	if err := exec.Command("launchctl", "load", path).Run(); err == nil {
		svc.Enable = ""
	}
	return svc, nil
}

// plistArgs renders argv as launchd's ProgramArguments elements. launchd takes
// an array rather than a command line, so the flags have to be split back apart
// rather than handed over as one string — which launchd would treat as a single
// argument containing spaces, and exec would then fail to find.
func plistArgs(exe, args string) string {
	elements := []string{"    <string>" + exe + "</string>"}
	for _, arg := range strings.Fields(args) {
		elements = append(elements, "    <string>"+arg+"</string>")
	}
	return strings.Join(elements, "\n")
}

// Status reports whether a unit is installed and what the manager says.
func Status() *Service {
	home, err := os.UserHomeDir()
	if err != nil {
		return &Service{}
	}

	switch runtime.GOOS {
	case "linux":
		path := filepath.Join(home, ".config", "systemd", "user", ServiceLabel+".service")
		svc := &Service{Path: path, Manager: "systemd"}
		if _, err := os.Stat(path); err == nil {
			svc.Installed = true
			out, _ := exec.Command("systemctl", "--user", "is-active", ServiceLabel).Output()
			svc.Enable = strings.TrimSpace(string(out))
		}
		return svc

	case "darwin":
		path := filepath.Join(home, "Library", "LaunchAgents", "dev."+ServiceLabel+".sync.plist")
		svc := &Service{Path: path, Manager: "launchd"}
		if _, err := os.Stat(path); err == nil {
			svc.Installed = true
		}
		return svc
	}
	return &Service{Manager: runtime.GOOS}
}

// Uninstall removes the unit, stopping it first where possible.
func Uninstall() (string, error) {
	svc := Status()
	if !svc.Installed {
		return "", nil
	}

	switch svc.Manager {
	case "systemd":
		_ = exec.Command("systemctl", "--user", "disable", "--now", ServiceLabel).Run()
	case "launchd":
		_ = exec.Command("launchctl", "unload", svc.Path).Run()
	}
	if err := os.Remove(svc.Path); err != nil {
		return "", err
	}
	return svc.Path, nil
}
