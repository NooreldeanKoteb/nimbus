//go:build windows

package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
)

func lookPath(name string) (string, error) { return exec.LookPath(name) }

// DetectPrivilege reports whether this process already runs elevated.
//
// Windows has no sudo-equivalent credential cache: a process is either
// elevated or it is not, decided when it was created. PrivSudo is reported
// when Windows' own `sudo` (11 24H2 and later) is available, since that is the
// only way to elevate a command without relaunching everything.
func DetectPrivilege() Privilege {
	if isElevated() {
		return PrivRoot
	}
	if _, err := exec.LookPath("sudo"); err == nil {
		return PrivSudo
	}
	return PrivNone
}

// isElevated reports whether the process token carries elevated rights.
//
// The token must be opened explicitly: IsElevated queries token information,
// so unlike IsMember it cannot be called on a null handle — doing so fails
// silently and would report every process as unelevated.
func isElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

// Elevate obtains administrator rights for this run.
//
// Unlike Unix, an already-running Windows process cannot gain privilege: UAC
// grants elevation only at process creation. So the options are Windows'
// built-in `sudo` where present, or relaunching nimbus through UAC. Callers
// that get ErrNeedsRelaunch should call RelaunchElevated.
func Elevate(ctx context.Context) (Privilege, error) {
	if isElevated() {
		return PrivRoot, nil
	}
	if _, err := exec.LookPath("sudo"); err == nil {
		// Windows 11's sudo prompts through UAC per command.
		return PrivSudo, nil
	}
	return PrivNone, ErrNeedsRelaunch
}

// ErrNeedsRelaunch signals that elevation requires starting a new process.
var ErrNeedsRelaunch = fmt.Errorf("administrator access requires relaunching nimbus")

// RelaunchElevated restarts nimbus with the same arguments through UAC.
//
// ShellExecute with the "runas" verb is the only supported way to trigger the
// consent prompt. The new process gets its own console, so the caller should
// report that and exit rather than continuing unprivileged.
func RelaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	args := strings.Join(os.Args[1:], " ")
	cwd, _ := os.Getwd()

	verb, _ := windows.UTF16PtrFromString("runas")
	exePtr, _ := windows.UTF16PtrFromString(exe)
	cwdPtr, _ := windows.UTF16PtrFromString(cwd)
	var argPtr *uint16
	if args != "" {
		argPtr, _ = windows.UTF16PtrFromString(args)
	}

	// SW_NORMAL keeps the elevated console visible so the user can see and
	// answer anything the run asks for.
	const swNormal = 1
	if err := windows.ShellExecute(0, verb, exePtr, argPtr, cwdPtr, swNormal); err != nil {
		return fmt.Errorf("elevation was declined or failed: %w", err)
	}
	return nil
}

// KeepAlive is a no-op on Windows: elevation is a property of the process, so
// there is no credential timestamp that can expire mid-run.
func KeepAlive(ctx context.Context, priv Privilege) func() {
	return func() {}
}

func runElevatedOutput(ctx context.Context, priv Privilege, pkgMgr string, argv []string) (string, error) {
	// PrivSudo on Windows means the built-in sudo is present; it raises its
	// own UAC prompt, so it must not be given a -n style flag.
	if priv == PrivSudo && needsElevation(pkgMgr) {
		argv = append([]string{"sudo"}, argv...)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()
	return string(out), err
}
