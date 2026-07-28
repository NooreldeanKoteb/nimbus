package state

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMarkerRoundTrips(t *testing.T) {
	repo := t.TempDir()

	m, err := NewMarker("personal", "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Save(repo); err != nil {
		t.Fatal(err)
	}

	loaded, err := ReadMarker(repo)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "personal" || loaded.ID != m.ID {
		t.Errorf("loaded %+v, want name and id preserved", loaded)
	}
	if loaded.Kind != MarkerKind || loaded.Version != MarkerVersion {
		t.Errorf("marker is not self-describing: %+v", loaded)
	}
}

// Two systems created back to back must not collide, or a picker cannot tell
// them apart and renaming one would appear to rename both.
func TestSystemIDsAreDistinct(t *testing.T) {
	seen := make(map[string]bool)
	for range 50 {
		m, err := NewMarker("personal", "node-a")
		if err != nil {
			t.Fatal(err)
		}
		if seen[m.ID] {
			t.Fatalf("duplicate system id %s", m.ID)
		}
		seen[m.ID] = true
	}
}

// This is the check that makes --remote safe. Without it, pointing nimbus at
// any repository clones it and starts writing device profiles into it.
func TestParseMarkerRefusesAnythingThatIsNotOne(t *testing.T) {
	cases := map[string]string{
		"not json":       "# My Project\n",
		"unrelated json": `{"name":"my-app","version":"1.0.0","dependencies":{}}`,
		"wrong kind":     `{"nimbus":"something-else","version":1,"name":"x"}`,
		"empty object":   `{}`,
		"a package.json": `{"name":"react","private":true,"scripts":{"build":"tsc"}}`,
		"missing kind":   `{"version":1,"id":"abc","name":"personal"}`,
	}
	for label, content := range cases {
		if _, err := ParseMarker([]byte(content)); err == nil {
			t.Errorf("%s was accepted as a nimbus system", label)
		} else if !errors.Is(err, ErrNotStateRepo) {
			t.Errorf("%s failed with %v, want ErrNotStateRepo", label, err)
		}
	}
}

// A device reading a format it does not understand has to say so, not guess.
// Guessing would mean writing a shape the newer devices then have to repair.
func TestParseMarkerRefusesANewerFormat(t *testing.T) {
	data, _ := json.Marshal(Marker{
		Kind: MarkerKind, Version: MarkerVersion + 1, ID: "abc", Name: "future",
	})
	_, err := ParseMarker(data)
	if err == nil {
		t.Fatal("a newer marker format was accepted")
	}
	if !strings.Contains(err.Error(), "upgrade nimbus") {
		t.Errorf("the error does not say what to do: %v", err)
	}
	// Not ErrNotStateRepo: it *is* a state repo, just a newer one. Conflating
	// the two would tell the user their system is not a system.
	if errors.Is(err, ErrNotStateRepo) {
		t.Error("a newer format was reported as not being a state repo at all")
	}
}

func TestReadMarkerOnAnOrdinaryRepo(t *testing.T) {
	_, err := ReadMarker(t.TempDir())
	if !errors.Is(err, ErrNotStateRepo) {
		t.Errorf("a directory with no marker gave %v, want ErrNotStateRepo", err)
	}
}

func TestSystemNamesAreValidated(t *testing.T) {
	for _, ok := range []string{"personal", "work laptop", "home-lab", "system_2"} {
		if err := ValidateSystemName(ok); err != nil {
			t.Errorf("%q was rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "../escape", "name/with/slash", strings.Repeat("x", 41)} {
		if err := ValidateSystemName(bad); err == nil {
			t.Errorf("%q was accepted as a system name", bad)
		}
	}
}

// A picker showing a blank row is worse than one showing an id.
func TestAnUnnamedMarkerStillLabelsItself(t *testing.T) {
	m, err := ParseMarker([]byte(`{"nimbus":"nimbus-state","version":1,"id":"0123456789abcdef"}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name == "" {
		t.Error("an unnamed system has no display name")
	}
	if !strings.Contains(m.Label(), "012345") {
		t.Errorf("label %q does not disambiguate by id", m.Label())
	}
}
