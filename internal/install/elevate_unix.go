//go:build !windows

package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// lookPath is indirected so the Windows build can apply its own resolution.
func lookPath(name string) (string, error) { return exec.LookPath(name) }

// DetectPrivilege reports how system packages can be installed here.
//
// `sudo -n` is used deliberately: a password prompt in a non-interactive check
// would hang, so this only reports privilege already available without one.
// Call Elevate to obtain it interactively.
func DetectPrivilege() Privilege {
	if os.Geteuid() == 0 {
		return PrivRoot
	}
	if path, err := exec.LookPath("sudo"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, path, "-n", "true").Run(); err == nil {
			return PrivSudo
		}
	}
	return PrivNone
}

// Elevate obtains privilege for this run, prompting for the system password
// once if needed. Works identically on Linux and macOS.
//
// The password is never read, stored, or transported by nimbus. `sudo -v`
// prompts through sudo's own PAM path with the terminal attached, and sudo
// then caches the credential for its configured timeout — after which every
// `sudo -n` call in this package simply succeeds. That is what reusing the
// system password means here: nimbus never handles the secret itself.
func Elevate(ctx context.Context) (Privilege, error) {
	if os.Geteuid() == 0 {
		return PrivRoot, nil
	}

	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return PrivNone, fmt.Errorf("sudo is not installed; run as root or install sudo")
	}

	// Already root-equivalent via NOPASSWD or a live cache: no prompt needed.
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = exec.CommandContext(checkCtx, sudoPath, "-n", "true").Run()
	cancel()
	if err == nil {
		return PrivSudo, nil
	}

	if !stdinIsTerminal() {
		return PrivNone, fmt.Errorf("sudo needs a password but there is no terminal to prompt on " +
			"(configure passwordless sudo for unattended runs)")
	}

	fmt.Fprintln(os.Stderr, "nimbus needs administrator access to install system packages.")

	// Inherit stdio so sudo owns the prompt, including disabling echo.
	promptCtx, cancelPrompt := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelPrompt()

	cmd := exec.CommandContext(promptCtx, sudoPath, "-v")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return PrivNone, fmt.Errorf("sudo authentication failed: %w", err)
	}

	// Confirm the cache is actually usable rather than trusting the exit code.
	verifyCtx, cancelVerify := context.WithTimeout(ctx, 5*time.Second)
	defer cancelVerify()
	if err := exec.CommandContext(verifyCtx, sudoPath, "-n", "true").Run(); err != nil {
		return PrivNone, fmt.Errorf("sudo credential did not persist: %w", err)
	}
	return PrivSudo, nil
}

// KeepAlive refreshes the sudo credential until the returned stop function is
// called. A long install must not fail partway through because sudo's default
// timeout elapsed between steps.
func KeepAlive(ctx context.Context, priv Privilege) func() {
	if priv != PrivSudo {
		return func() {}
	}
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return func() {}
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshCtx, c := context.WithTimeout(ctx, 5*time.Second)
				_ = exec.CommandContext(refreshCtx, sudoPath, "-n", "-v").Run()
				c()
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

func runElevatedOutput(ctx context.Context, priv Privilege, pkgMgr string, argv []string) (string, error) {
	if priv == PrivSudo && needsElevation(pkgMgr) {
		argv = append([]string{"sudo", "-n"}, argv...)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Non-interactive by construction: any tool that still tries to prompt
	// gets a closed stdin rather than blocking an unattended setup forever.
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")

	out, err := cmd.CombinedOutput()
	return string(out), err
}
