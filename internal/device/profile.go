package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Profile is the full picture of one device, published to the state repo as
// nodes/<id>.json so any other node — or an operator — can read it.
type Profile struct {
	ID string `json:"id"`
	// Alias is this device's human-readable name, published so the rest of the
	// fleet can address it by something a person chose.
	Alias      string    `json:"alias,omitempty"`
	Hostname   string    `json:"hostname"`
	OS         OSInfo    `json:"os"`
	Hardware   Hardware  `json:"hardware"`
	PkgManager string    `json:"package_manager,omitempty"`
	Tools      []Tool    `json:"tools"`
	Missing    []string  `json:"missing,omitempty"`
	NimbusHome string    `json:"nimbus_home,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Detect builds a complete profile for the current machine.
func Detect(ctx context.Context, id *Identity, probes []Probe) *Profile {
	tools := DetectTools(ctx, probes)

	return &Profile{
		ID:         id.ID,
		Alias:      id.Alias,
		Hostname:   id.Hostname,
		OS:         DetectOS(),
		Hardware:   DetectHardware(),
		PkgManager: DetectPackageManager(),
		Tools:      tools,
		Missing:    MissingTools(tools),
		UpdatedAt:  time.Now().UTC(),
	}
}

// Has reports whether a named tool is present.
func (p *Profile) Has(name string) bool {
	t, ok := FindTool(p.Tools, name)
	return ok && t.Present
}

// Capabilities summarizes the profile as routing labels. Task affinity matches
// against these rather than re-deriving hardware facts at dispatch time.
func (p *Profile) Capabilities() []string {
	var caps []string

	caps = append(caps, "os:"+p.OS.Platform, "arch:"+p.OS.Arch)
	if p.OS.Distro != "" {
		caps = append(caps, "distro:"+p.OS.Distro)
	}
	if p.OS.Container {
		// Containers cannot reach host hardware or survive a host reboot, so
		// affinity rules need to be able to exclude them explicitly.
		caps = append(caps, "container")
	}
	if p.PkgManager != "" {
		caps = append(caps, "pkg:"+p.PkgManager)
	}
	if p.Hardware.HasScreen {
		caps = append(caps, "display")
	} else {
		caps = append(caps, "headless")
	}
	for _, gpu := range p.Hardware.GPUs {
		caps = append(caps, "gpu:"+gpu)
	}
	for _, t := range p.Tools {
		if t.Present {
			caps = append(caps, "tool:"+t.Name)
		}
	}
	return caps
}

// Save writes the profile into the state repo under nodes/<id>.json.
func (p *Profile) Save(repoPath string) (string, error) {
	dir := filepath.Join(repoPath, "nodes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("save profile: %w", err)
	}

	path := filepath.Join(dir, p.ID+".json")
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", fmt.Errorf("save profile: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("save profile: %w", err)
	}
	return path, nil
}

// LoadProfile reads one node's published profile.
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse profile %s: %w", filepath.Base(path), err)
	}
	return &p, nil
}

// Label is what a person should see for this device.
func (p *Profile) Label() string {
	if p.Alias != "" {
		return p.Alias
	}
	return ShortID(p.ID)
}

// Fleet is every device published to the state repo.
type Fleet []*Profile

// LoadFleet reads every published profile in the state repo.
// A single corrupt profile must not hide the rest of the fleet.
func LoadFleet(repoPath string) (Fleet, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, "nodes", "*.json"))
	if err != nil {
		return nil, err
	}

	profiles := make(Fleet, 0, len(matches))
	for _, path := range matches {
		p, err := LoadProfile(path)
		if err != nil {
			continue
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

// Label renders a node id the way a person should read it.
//
// Resolved at display time rather than stored alongside each reference, so
// renaming a device updates every task claim, progress entry, and audit record
// that mentions it — including ones written before the rename.
func (f Fleet) Label(nodeID string) string {
	for _, p := range f {
		if p.ID == nodeID {
			return p.Label()
		}
	}
	// A device that has never published, or was removed from the fleet.
	return ShortID(nodeID)
}

// ErrAmbiguous is returned when a reference matches more than one device.
var ErrAmbiguous = errors.New("ambiguous device reference")

// Resolve turns whatever the user typed into a node id: an alias, a full id, or
// an unambiguous id prefix.
//
// Exact matches win outright. Without that rule, a device aliased "build" could
// be shadowed by another whose id happens to start with "build", and which one
// you got would depend on directory order.
func (f Fleet) Resolve(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("no device specified")
	}
	for _, p := range f {
		if p.Alias == ref || p.ID == ref {
			return p.ID, nil
		}
	}

	var matches []string
	for _, p := range f {
		if strings.HasPrefix(p.ID, ref) || strings.HasPrefix(p.Alias, ref) {
			matches = append(matches, p.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no device named %q (run `nimbus fleet` to list them)", ref)
	default:
		labels := make([]string, len(matches))
		for i, id := range matches {
			labels[i] = f.Label(id)
		}
		return "", fmt.Errorf("%w %q: matches %s", ErrAmbiguous, ref, strings.Join(labels, ", "))
	}
}

// AliasTaken reports whether another device already answers to this alias.
// Two devices sharing an alias would make it useless for addressing either.
func (f Fleet) AliasTaken(alias, exceptID string) bool {
	for _, p := range f {
		if p.Alias == alias && p.ID != exceptID {
			return true
		}
	}
	return false
}

// Summary is a one-line description for fleet listings.
func (p *Profile) Summary() string {
	parts := []string{p.OS.Platform + "/" + p.OS.Arch}
	if p.OS.Distro != "" {
		distro := p.OS.Distro
		if p.OS.Release != "" {
			distro += " " + p.OS.Release
		}
		parts = append(parts, distro)
	}
	if p.Hardware.CPUs > 0 {
		parts = append(parts, fmt.Sprintf("%d cpu", p.Hardware.CPUs))
	}
	if p.Hardware.MemoryGB > 0 {
		parts = append(parts, fmt.Sprintf("%.0f GB", p.Hardware.MemoryGB))
	}
	if len(p.Hardware.GPUs) > 0 {
		parts = append(parts, strings.Join(p.Hardware.GPUs, "+")+" gpu")
	}
	return strings.Join(parts, ", ")
}
