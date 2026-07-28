package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MarkerFile identifies a repository as nimbus state. It sits at the root so
// that reading it costs one API call rather than a clone.
const MarkerFile = "nimbus.json"

// MarkerKind is the value that makes the file self-describing. A repo is state
// because this says so, not because of its name — names get changed, forked,
// and reused, and guessing from one is how nimbus would end up writing device
// profiles into somebody's unrelated project.
const MarkerKind = "nimbus-state"

// MarkerVersion is the on-disk format. A device reading a newer marker than it
// understands should say so rather than guess.
const MarkerVersion = 1

// Topic is the provider-side index used to find state repos without opening
// them. It is a hint, never the authority: topics can be removed, and a repo
// shared as a link may never have carried one. The marker file decides.
const Topic = "nimbus-state"

// ErrNotStateRepo means a repository exists but is not nimbus state.
var ErrNotStateRepo = errors.New("not a nimbus state repo")

// Marker is the contents of nimbus.json.
type Marker struct {
	Kind    string `json:"nimbus"`
	Version int    `json:"version"`
	// ID is stable for the life of the system. The name is what people read and
	// therefore what people change; the id is what anything else should refer
	// to, so a rename does not orphan history.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Created records when the system came into being, which is the field that
	// tells two similarly named systems apart in a picker.
	Created   time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// NewMarker builds a marker for a new system.
func NewMarker(name, createdBy string) (*Marker, error) {
	if err := ValidateSystemName(name); err != nil {
		return nil, err
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("new system id: %w", err)
	}
	return &Marker{
		Kind: MarkerKind, Version: MarkerVersion,
		ID: hex.EncodeToString(raw[:]), Name: name,
		Created: time.Now().UTC(), CreatedBy: createdBy,
	}, nil
}

// ValidateSystemName keeps a name readable in a picker and safe in a path.
func ValidateSystemName(name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return errors.New("a system needs a name")
	case len(name) > 40:
		return fmt.Errorf("system name %q is longer than 40 characters", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == ' ':
		default:
			return fmt.Errorf("system name %q: use letters, digits, spaces, - and _ only", name)
		}
	}
	return nil
}

// ParseMarker reads a marker and refuses anything that is not one.
//
// This is the check that makes `--remote <url>` safe. Without it, pointing
// nimbus at any repository at all would clone it and begin writing device
// profiles and an audit trail into somebody's unrelated project.
func ParseMarker(data []byte) (*Marker, error) {
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: %s is not valid JSON", ErrNotStateRepo, MarkerFile)
	}
	if m.Kind != MarkerKind {
		return nil, fmt.Errorf("%w: %s does not identify one", ErrNotStateRepo, MarkerFile)
	}
	if m.Version > MarkerVersion {
		return nil, fmt.Errorf("%s was created by a newer nimbus (format %d, this build understands %d) — upgrade nimbus",
			m.Name, m.Version, MarkerVersion)
	}
	if m.Name == "" {
		// Readable rather than correct-but-blank: a picker showing an empty row
		// is worse than one showing an id.
		m.Name = m.ID
	}
	return &m, nil
}

// MarkerPath is the marker's location inside a checked-out repo.
func MarkerPath(repoPath string) string {
	return filepath.Join(repoPath, MarkerFile)
}

// ReadMarker loads the marker from a local checkout.
func ReadMarker(repoPath string) (*Marker, error) {
	data, err := os.ReadFile(MarkerPath(repoPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: no %s", ErrNotStateRepo, MarkerFile)
	}
	if err != nil {
		return nil, err
	}
	return ParseMarker(data)
}

// Save writes the marker into a local checkout.
func (m *Marker) Save(repoPath string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("save %s: %w", MarkerFile, err)
	}
	if err := os.WriteFile(MarkerPath(repoPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("save %s: %w", MarkerFile, err)
	}
	return nil
}

// Label is how a system is named in output: the name, with enough of the id to
// disambiguate two systems a user gave the same name on different accounts.
func (m *Marker) Label() string {
	if m == nil {
		return "unknown"
	}
	if len(m.ID) >= 6 {
		return fmt.Sprintf("%s (%s)", m.Name, m.ID[:6])
	}
	return m.Name
}
