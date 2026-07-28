package device

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Tool is one discovered (or missing) executable.
type Tool struct {
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Present bool   `json:"present"`
}

// Probe describes how to detect one tool's version.
type Probe struct {
	Name string
	Args []string
}

// DefaultProbes covers the toolchains a support operator most often needs to
// reason about. Order is display order.
var DefaultProbes = []Probe{
	{"claude", []string{"--version"}},
	{"git", []string{"--version"}},
	{"node", []string{"--version"}},
	{"npm", []string{"--version"}},
	{"python3", []string{"--version"}},
	{"go", []string{"version"}},
	{"rustc", []string{"--version"}},
	{"docker", []string{"--version"}},
	{"podman", []string{"--version"}},
	{"curl", []string{"--version"}},
	{"ssh", []string{"-V"}},
	{"tmux", []string{"-V"}},
}

// packageManagers maps an executable to the manager it represents, in priority
// order: the first one present wins.
var packageManagers = []struct{ bin, name string }{
	{"apt-get", "apt"},
	{"dnf", "dnf"},
	{"pacman", "pacman"},
	{"zypper", "zypper"},
	{"apk", "apk"},
	{"brew", "brew"},
	{"winget", "winget"},
	{"choco", "choco"},
}

// DetectTools probes for each tool concurrently. Probing is bounded by ctx
// because a wedged binary must not hang device profiling.
func DetectTools(ctx context.Context, probes []Probe) []Tool {
	if probes == nil {
		probes = DefaultProbes
	}

	tools := make([]Tool, len(probes))
	var wg sync.WaitGroup

	for i, p := range probes {
		wg.Add(1)
		go func(i int, p Probe) {
			defer wg.Done()
			tools[i] = detectTool(ctx, p)
		}(i, p)
	}
	wg.Wait()
	return tools
}

func detectTool(ctx context.Context, p Probe) Tool {
	path, err := exec.LookPath(p.Name)
	if err != nil {
		return Tool{Name: p.Name, Present: false}
	}

	t := Tool{Name: p.Name, Path: path, Present: true}

	// Version probes execute third-party binaries, so cap them tightly.
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, path, p.Args...)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		// Present but unprobeable is still useful information.
		return t
	}
	t.Version = firstVersionLine(string(out))
	return t
}

// firstVersionLine reduces multi-line banners (ssh -V, go version) to one line.
func firstVersionLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 120 {
				line = line[:120]
			}
			return line
		}
	}
	return ""
}

// DetectPackageManager returns the primary package manager, or "" if none.
func DetectPackageManager() string {
	for _, pm := range packageManagers {
		if _, err := exec.LookPath(pm.bin); err == nil {
			return pm.name
		}
	}
	if runtime.GOOS == "darwin" {
		// brew is the expected manager on macOS even when not yet installed;
		// reporting it tells the installer what to bootstrap.
		return "brew"
	}
	return ""
}

// MissingTools returns the names of tools that were not found.
func MissingTools(tools []Tool) []string {
	var out []string
	for _, t := range tools {
		if !t.Present {
			out = append(out, t.Name)
		}
	}
	return out
}

// FindTool returns the named tool from a profile's tool list.
func FindTool(tools []Tool, name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}
