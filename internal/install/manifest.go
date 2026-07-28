// Package install brings a device up to the state declared in the manifest.
//
// This is Tier 0 of the bootstrap chain (DESIGN.md §2a): the deterministic
// things nimbus installs itself, before Claude exists to make judgment calls.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Manifest declares what every device in the fleet should have.
type Manifest struct {
	ClaudeCode ClaudeCodeSpec `json:"claude_code"`
	MCPServers []MCPServer    `json:"mcp_servers,omitempty"`
	// LinkConfig lists paths under config/claude/ in the state repo that are
	// linked into the local ~/.claude directory.
	LinkConfig []string `json:"link_config,omitempty"`
}

// ClaudeCodeSpec controls Claude Code installation.
type ClaudeCodeSpec struct {
	Install bool `json:"install"`
	// Version is "stable", "latest", or an exact version the installer accepts.
	Version string `json:"version,omitempty"`
}

// MCPServer is one MCP server to register with Claude Code.
type MCPServer struct {
	Name string `json:"name"`
	// Transport is stdio, http, or sse.
	Transport string   `json:"transport"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	URL       string   `json:"url,omitempty"`
	// Scope is user or project; user scope is what makes a server available
	// everywhere on the device rather than in one repo.
	Scope string `json:"scope,omitempty"`
}

// Validate reports whether a server definition is internally consistent.
func (m MCPServer) Validate() error {
	if m.Name == "" {
		return errors.New("mcp server has no name")
	}
	switch m.Transport {
	case "stdio":
		if m.Command == "" {
			return fmt.Errorf("mcp server %q: stdio transport needs a command", m.Name)
		}
	case "http", "sse":
		if m.URL == "" {
			return fmt.Errorf("mcp server %q: %s transport needs a url", m.Name, m.Transport)
		}
	case "":
		return fmt.Errorf("mcp server %q: no transport specified", m.Name)
	default:
		return fmt.Errorf("mcp server %q: unknown transport %q", m.Name, m.Transport)
	}
	return nil
}

// DefaultManifest is written on first init so a new fleet starts with
// something coherent rather than an empty file.
func DefaultManifest() *Manifest {
	return &Manifest{
		ClaudeCode: ClaudeCodeSpec{Install: true, Version: "stable"},
		LinkConfig: []string{"CLAUDE.md", "settings.json", "skills", "agents", "commands"},
		// Nimbus registers itself, so every device that runs `nimbus init`
		// ends up with the mesh reachable from inside Claude. The bare command
		// rather than an absolute path, for the same reason as the session
		// hook: the manifest travels to machines with different layouts.
		MCPServers: []MCPServer{NimbusMCPServer()},
	}
}

// NimbusMCPServer is nimbus registering itself as a tool server for Claude.
func NimbusMCPServer() MCPServer {
	return MCPServer{
		Name: "nimbus", Transport: "stdio",
		Command: "nimbus", Args: []string{"mcp"}, Scope: "user",
	}
}

// EnsureSelfRegistered adds the nimbus MCP server to a manifest that predates
// it, reporting whether anything changed.
//
// Without this, only brand-new fleets ever get the mesh tools: LoadManifest
// falls back to the default solely when no file exists, so every device that
// enrolled before the MCP server shipped would silently never see it.
func (m *Manifest) EnsureSelfRegistered() bool {
	for _, s := range m.MCPServers {
		if s.Name == "nimbus" {
			return false
		}
	}
	m.MCPServers = append(m.MCPServers, NimbusMCPServer())
	return true
}

// LoadManifest reads a manifest, returning the default when none exists yet.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultManifest(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	for _, s := range m.MCPServers {
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
	}
	return &m, nil
}

// Save writes the manifest to the state repo.
func (m *Manifest) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	return nil
}
