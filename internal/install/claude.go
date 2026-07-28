package install

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Native installers, which have no Node dependency. Windows ships a separate
// PowerShell artifact rather than the sh script.
const (
	claudeInstallURL    = "https://claude.ai/install.sh"
	claudeInstallPS1URL = "https://claude.ai/install.ps1"
)

// Step is the outcome of one install action, for reporting and auditing.
type Step struct {
	Name    string
	Changed bool
	Skipped bool
	Detail  string
	// Rollback is the command that undoes this step, recorded before the step
	// runs so the audit trail can describe how to reverse it.
	Rollback string
	Err      error
}

// ClaudeCodePath returns the resolved claude binary, or "" when absent.
// The native installer targets ~/.local/bin, which is often not yet on PATH
// in a non-interactive shell, so check there too.
func ClaudeCodePath() string {
	if path, err := exec.LookPath("claude"); err == nil {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	for _, candidate := range []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".claude", "local", name),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// InstallClaudeCode installs Claude Code when it is not already present.
//
// The installer script is fetched over Go's HTTP client rather than piped from
// curl, because curl is not guaranteed to exist on a bare device.
func InstallClaudeCode(ctx context.Context, spec ClaudeCodeSpec) Step {
	step := Step{Name: "claude-code"}

	if !spec.Install {
		step.Skipped = true
		step.Detail = "disabled in manifest"
		return step
	}
	if path := ClaudeCodePath(); path != "" {
		step.Skipped = true
		step.Detail = "already installed at " + path
		return step
	}
	if runtime.GOOS == "windows" {
		return installClaudeWindows(ctx, spec)
	}

	script, err := fetch(ctx, claudeInstallURL)
	if err != nil {
		step.Err = fmt.Errorf("download installer: %w", err)
		return step
	}

	tmp, err := os.CreateTemp("", "claude-install-*.sh")
	if err != nil {
		step.Err = err
		return step
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(script); err != nil {
		tmp.Close()
		step.Err = err
		return step
	}
	tmp.Close()

	// The upstream installer is a bash script, not POSIX sh. Running it under
	// dash — which is /bin/sh on Debian and Ubuntu — fails on bash syntax, so
	// bash must be located explicitly rather than assumed.
	shell, err := exec.LookPath("bash")
	if err != nil {
		step.Err = fmt.Errorf("claude's installer requires bash, which is not installed " +
			"(install it with your package manager, then re-run `nimbus init`)")
		return step
	}

	args := []string{tmp.Name()}
	if spec.Version != "" && spec.Version != "stable" {
		args = append(args, spec.Version)
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(runCtx, shell, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		step.Err = fmt.Errorf("installer failed: %w: %s", err, lastLines(string(out), 5))
		return step
	}

	path := ClaudeCodePath()
	if path == "" {
		step.Err = fmt.Errorf("installer reported success but claude was not found on PATH or in ~/.local/bin")
		return step
	}

	step.Changed = true
	step.Detail = "installed at " + path
	return step
}

// InstallMCPServers registers each manifest server with Claude Code.
//
// `claude mcp add` is idempotent in effect but errors when a server already
// exists, so an existing entry is reported as skipped rather than failed.
func InstallMCPServers(ctx context.Context, servers []MCPServer) []Step {
	steps := make([]Step, 0, len(servers))

	claude := ClaudeCodePath()
	if claude == "" {
		for _, s := range servers {
			steps = append(steps, Step{
				Name: "mcp:" + s.Name, Skipped: true,
				Detail: "claude not installed",
			})
		}
		return steps
	}

	existing := existingMCPServers(ctx, claude)

	for _, s := range servers {
		step := Step{Name: "mcp:" + s.Name}

		if err := s.Validate(); err != nil {
			step.Err = err
			steps = append(steps, step)
			continue
		}
		if existing[s.Name] {
			step.Skipped = true
			step.Detail = "already registered"
			steps = append(steps, step)
			continue
		}

		args := mcpAddArgs(s)
		runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		cmd := exec.CommandContext(runCtx, claude, args...)
		out, err := cmd.CombinedOutput()
		cancel()

		if err != nil {
			step.Err = fmt.Errorf("%w: %s", err, lastLines(string(out), 3))
		} else {
			step.Changed = true
			step.Detail = s.Transport
		}
		steps = append(steps, step)
	}
	return steps
}

func mcpAddArgs(s MCPServer) []string {
	scope := s.Scope
	if scope == "" {
		// User scope is what makes a server available across every repo on
		// the device, which is the point of installing it from a manifest.
		scope = "user"
	}

	args := []string{"mcp", "add", "--scope", scope}
	switch s.Transport {
	case "http", "sse":
		args = append(args, "--transport", s.Transport, s.Name, s.URL)
	default:
		args = append(args, s.Name, "--", s.Command)
		args = append(args, s.Args...)
	}
	return args
}

// existingMCPServers lists already-registered servers. A failure here is not
// fatal: the worst case is an add that reports "already exists" as an error.
func existingMCPServers(ctx context.Context, claude string) map[string]bool {
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(listCtx, claude, "mcp", "list").CombinedOutput()
	if err != nil {
		return map[string]bool{}
	}

	found := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		// Output is "name: command" or "name: url"; the prefix is what matters.
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if name = strings.TrimSpace(name); name != "" {
			found[name] = true
		}
	}
	return found
}

// installClaudeWindows runs the PowerShell installer, which is a separate
// artifact from the sh script used on macOS and Linux.
func installClaudeWindows(ctx context.Context, spec ClaudeCodeSpec) Step {
	step := Step{Name: "claude-code"}

	// PowerShell 7 first, then the built-in Windows PowerShell 5.
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		shell, err = exec.LookPath("powershell")
		if err != nil {
			step.Err = fmt.Errorf("no PowerShell found to run the Claude Code installer")
			return step
		}
	}

	script, err := fetch(ctx, claudeInstallPS1URL)
	if err != nil {
		step.Err = fmt.Errorf("download installer: %w", err)
		return step
	}

	tmp, err := os.CreateTemp("", "claude-install-*.ps1")
	if err != nil {
		step.Err = err
		return step
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(script); err != nil {
		tmp.Close()
		step.Err = err
		return step
	}
	tmp.Close()

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// Bypass covers the common case of a machine whose execution policy would
	// otherwise refuse a downloaded script.
	args := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", tmp.Name()}
	if spec.Version != "" && spec.Version != "stable" {
		args = append(args, spec.Version)
	}

	out, err := exec.CommandContext(runCtx, shell, args...).CombinedOutput()
	if err != nil {
		step.Err = fmt.Errorf("installer failed: %w: %s", err, lastLines(string(out), 5))
		return step
	}

	path := ClaudeCodePath()
	if path == "" {
		step.Err = fmt.Errorf("installer reported success but claude was not found on PATH")
		return step
	}

	step.Changed = true
	step.Detail = "installed at " + path
	return step
}

// lastLines trims command output down to the tail, which is where the actual
// failure reason lives in almost every installer.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	// Cap the read so a redirected or hostile URL cannot exhaust memory.
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}
