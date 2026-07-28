// Package config resolves on-disk locations for nimbus state.
//
// Everything nimbus writes lives under one of four roots, all overridable so
// tests and containers can run fully isolated:
//
//	config  ~/.config/nimbus      credentials, device identity
//	data    ~/.local/share/nimbus cached artifacts, installed helpers
//	state   ~/.local/state/nimbus logs, run state
//	repo    ~/.nimbus             the git-backed state repo (see DESIGN.md §4)
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoHome is returned when the user's home directory cannot be determined.
var ErrNoHome = errors.New("cannot determine home directory")

// Paths holds the resolved roots for this process.
type Paths struct {
	Config string
	Data   string
	State  string
	Repo   string
}

// Resolve computes paths from the environment, honoring XDG variables and the
// NIMBUS_HOME override that tests and containers use to sandbox everything.
func Resolve() (*Paths, error) {
	if root := os.Getenv("NIMBUS_HOME"); root != "" {
		return &Paths{
			Config: filepath.Join(root, "config"),
			Data:   filepath.Join(root, "data"),
			State:  filepath.Join(root, "state"),
			Repo:   filepath.Join(root, "repo"),
		}, nil
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, ErrNoHome
	}

	return &Paths{
		Config: xdg("XDG_CONFIG_HOME", home, ".config"),
		Data:   xdg("XDG_DATA_HOME", home, ".local", "share"),
		State:  xdg("XDG_STATE_HOME", home, ".local", "state"),
		Repo:   filepath.Join(home, ".nimbus"),
	}, nil
}

// xdg returns $VAR/nimbus when VAR is set to an absolute path, else the
// fallback under home. Relative XDG values are ignored per the spec.
func xdg(env, home string, fallback ...string) string {
	if v := os.Getenv(env); filepath.IsAbs(v) {
		return filepath.Join(v, "nimbus")
	}
	return filepath.Join(append([]string{home}, append(fallback, "nimbus")...)...)
}

// CredentialsFile is where provider tokens are persisted.
func (p *Paths) CredentialsFile() string {
	return filepath.Join(p.Config, "credentials.json")
}

// IdentityFile holds this device's stable node identity.
func (p *Paths) IdentityFile() string {
	return filepath.Join(p.Config, "node.json")
}

// RepoAuditFile is a device's audit trail inside the state repo, so the record
// travels with the fleet instead of staying on one machine.
func (p *Paths) RepoAuditFile(nodeID string) string {
	return filepath.Join(p.Repo, "audit", nodeID+".jsonl")
}

// RepoNodeFile is where a device publishes its profile for the fleet to read.
func (p *Paths) RepoNodeFile(nodeID string) string {
	return filepath.Join(p.Repo, "nodes", nodeID+".json")
}

// ManifestFile declares what should be installed on every device.
func (p *Paths) ManifestFile() string {
	return filepath.Join(p.Repo, "config", "manifest.json")
}

// RepoClaudeDir holds the Claude Code config that is shared across devices.
func (p *Paths) RepoClaudeDir() string {
	return filepath.Join(p.Repo, "config", "claude")
}

// EnsureDirs creates every root with owner-only permissions. Config holds
// tokens, so 0700 is a correctness requirement, not hardening.
func (p *Paths) EnsureDirs() error {
	for _, dir := range []string{p.Config, p.Data, p.State} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}
