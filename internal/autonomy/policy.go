package autonomy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Policy is one device's autonomy setting, published to the state repo so the
// rest of the fleet can see what a machine is allowed to do before dispatching
// work to it.
//
// It is device-owned (DESIGN.md §4a): only this machine ever writes its own
// file, so raising the level here can never be undone by another device's sync.
type Policy struct {
	Node  string    `json:"node"`
	Level Level     `json:"level"`
	SetAt time.Time `json:"set_at"`
	// Note records why, which is the field a person reads six months later
	// wondering who put this laptop at L3.
	Note string `json:"note,omitempty"`
	// Exec is what a peer may run here (invariant 3). It is deliberately part
	// of this device's own file rather than something a sender attaches to a
	// request: an allowlist the caller can influence is not an allowlist. Empty
	// is the default and means no peer may run anything, so a device that has
	// never thought about this cannot be talked into running something.
	Exec []string `json:"exec,omitempty"`
}

// Allows reports whether a command is already on the peer-exec allowlist.
func (p *Policy) Allows(command string) bool {
	for _, c := range p.Exec {
		if c == command {
			return true
		}
	}
	return false
}

// Allow adds a command to the peer-exec allowlist, reporting whether anything
// changed so a caller can avoid writing a file and syncing for a no-op.
func (p *Policy) Allow(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || p.Allows(command) {
		return false
	}
	p.Exec = append(p.Exec, command)
	sort.Strings(p.Exec)
	return true
}

// Deny removes a command from the peer-exec allowlist.
func (p *Policy) Deny(command string) bool {
	command = strings.TrimSpace(command)
	for i, c := range p.Exec {
		if c == command {
			p.Exec = append(p.Exec[:i], p.Exec[i+1:]...)
			return true
		}
	}
	return false
}

// File is a device's policy inside the state repo.
func File(repoPath, nodeID string) string {
	return filepath.Join(repoPath, "policy", nodeID+".json")
}

// Pattern matches the policy files this device owns, for the sync layer.
func Pattern(nodeID string) string {
	return filepath.Join("policy", nodeID+".json")
}

// Load reads a device's policy, returning the default when none has been set.
//
// An unreadable or corrupt policy falls back to the default rather than
// failing: a device that cannot parse its own ceiling must assume the
// conservative one, never the last one it happened to remember.
func Load(repoPath, nodeID string) *Policy {
	fallback := &Policy{Node: nodeID, Level: Default}

	data, err := os.ReadFile(File(repoPath, nodeID))
	if err != nil {
		return fallback
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return fallback
	}
	if !p.Level.Valid() {
		return fallback
	}
	p.Node = nodeID
	return &p
}

// Save writes the policy into the state repo.
func (p *Policy) Save(repoPath string) error {
	if p.Node == "" {
		return errors.New("policy has no node id")
	}
	if !p.Level.Valid() {
		return fmt.Errorf("autonomy level %d is not on the ladder", int(p.Level))
	}
	p.SetAt = time.Now().UTC()

	path := File(repoPath, p.Node)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("save policy: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("save policy: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("save policy: %w", err)
	}
	return nil
}
