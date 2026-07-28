package device

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAlias(t *testing.T) {
	for _, ok := range []string{"laptop", "kali-thinkpad", "build_server_2", "a", "x1"} {
		if err := ValidateAlias(ok); err != nil {
			t.Errorf("ValidateAlias(%q) = %v, want accepted", ok, err)
		}
	}

	bad := []string{
		"", "Laptop", "my laptop", "has.dot", "a/b",
		"-leading-dash", // would be read as a flag on the command line
		"this-alias-is-far-too-long-to-fit-in-a-listing",
	}
	for _, name := range bad {
		if err := ValidateAlias(name); err == nil {
			t.Errorf("ValidateAlias(%q) = nil, want rejection", name)
		}
	}
}

func TestDefaultAliasFromHostname(t *testing.T) {
	cases := map[string]string{
		"nkoteb-desktop":  "nkoteb-desktop",
		"studio.local":    "studio",
		"MacBook-Pro.lan": "macbook-pro",
		"my pc":           "my-pc",
		"host_01":         "host_01",
		"--weird--":       "weird",
		"":                "",
		"...":             "",
		"a-really-long-hostname-that-exceeds-the-limit": "a-really-long-hostname-that-exce",
	}
	for hostname, want := range cases {
		if got := DefaultAlias(hostname); got != want {
			t.Errorf("DefaultAlias(%q) = %q, want %q", hostname, got, want)
		}
	}

	// Whatever comes out must itself be a legal alias, or setting it would
	// fail validation on the very next command.
	for hostname := range cases {
		if alias := DefaultAlias(hostname); alias != "" {
			if err := ValidateAlias(alias); err != nil {
				t.Errorf("DefaultAlias(%q) produced an invalid alias: %v", hostname, err)
			}
		}
	}
}

func TestIdentityGetsAliasOnCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")

	id, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	// A device should never be presented as a hex string just because nobody
	// named it yet.
	if id.Label() == "" {
		t.Error("new identity has no label at all")
	}
	if id.Alias == "" && id.Hostname != "" {
		t.Errorf("no alias derived from hostname %q", id.Hostname)
	}
}

func TestSetAliasPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	id, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := id.SetAlias(path, "kali-thinkpad"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Alias != "kali-thinkpad" {
		t.Errorf("Alias = %q after reload, want it persisted", reloaded.Alias)
	}
	if reloaded.Label() != "kali-thinkpad" {
		t.Errorf("Label() = %q, want the alias", reloaded.Label())
	}

	// The id is the key for every repo path and every audit hash, so renaming
	// must never move it.
	if reloaded.ID != id.ID {
		t.Errorf("ID changed from %q to %q on rename", id.ID, reloaded.ID)
	}
}

func TestSetAliasRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	id, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	before := id.Alias

	if err := id.SetAlias(path, "Not Valid"); err == nil {
		t.Fatal("SetAlias accepted an invalid alias")
	}
	if id.Alias != before {
		t.Errorf("Alias = %q after a rejected rename, want %q", id.Alias, before)
	}
}

// The override exists for VM clones, which by definition already have a stored
// identity copied from the original. Seeding only new identities would make it
// useless in the one situation it was built for.
func TestNodeIDOverrideBeatsStoredIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")

	original, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(NodeIDEnv, "cloned-vm")
	overridden, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if overridden.ID != "cloned-vm" {
		t.Fatalf("ID = %q, want the override to win over %q", overridden.ID, original.ID)
	}

	// Not persisted: an env var must not permanently rebrand the device as a
	// side effect of whatever command happened to be running.
	os.Unsetenv(NodeIDEnv)
	reloaded, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ID != original.ID {
		t.Errorf("ID = %q after unsetting the override, want the stored %q back",
			reloaded.ID, original.ID)
	}
}

// Regression: sanitizeID never returns an empty string — it returns the
// "node-unknown" placeholder. Sanitizing before checking for an unset variable
// made every device look like a deliberate override to that placeholder, which
// would point every profile and every audit log at the same files.
func TestUnsetNodeIDDoesNotOverrideAnything(t *testing.T) {
	os.Unsetenv(NodeIDEnv)

	if got := nodeIDOverride(); got != "" {
		t.Errorf("nodeIDOverride() = %q with the variable unset, want empty", got)
	}
	for _, blank := range []string{"", "   ", "\t"} {
		t.Setenv(NodeIDEnv, blank)
		if got := nodeIDOverride(); got != "" {
			t.Errorf("nodeIDOverride() = %q for %q, want empty", got, blank)
		}
	}

	// And an id, once assigned, must survive reload untouched.
	os.Unsetenv(NodeIDEnv)
	path := filepath.Join(t.TempDir(), "node.json")
	first, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("id changed across reloads: %q then %q", first.ID, second.ID)
	}
}

func TestShortID(t *testing.T) {
	if got := ShortID("8f3a2b1c9d4e5f60718293a4b5c6d7e8"); got != "8f3a2b1c" {
		t.Errorf("ShortID() = %q, want the first 8 characters", got)
	}
	if got := ShortID("short"); got != "short" {
		t.Errorf("ShortID() = %q, want a short id left alone", got)
	}
}

func fleetOf(t *testing.T, pairs ...string) Fleet {
	t.Helper()
	var f Fleet
	for i := 0; i < len(pairs); i += 2 {
		f = append(f, &Profile{ID: pairs[i], Alias: pairs[i+1]})
	}
	return f
}

func TestFleetLabel(t *testing.T) {
	f := fleetOf(t, "8f3a2b1c9d4e", "kali-thinkpad", "aaaabbbbcccc", "")

	if got := f.Label("8f3a2b1c9d4e"); got != "kali-thinkpad" {
		t.Errorf("Label() = %q, want the alias", got)
	}
	// An unnamed device still needs something readable.
	if got := f.Label("aaaabbbbcccc"); got != "aaaabbbb" {
		t.Errorf("Label() = %q, want a short id", got)
	}
	// A device that never published, or was removed, must not break rendering.
	if got := f.Label("ffffffffffff"); got != "ffffffff" {
		t.Errorf("Label() for an unknown node = %q, want a short id", got)
	}
}

func TestFleetResolve(t *testing.T) {
	f := fleetOf(t, "8f3a2b1c9d4e", "kali-thinkpad", "aaaabbbbcccc", "studio")

	cases := map[string]string{
		"kali-thinkpad": "8f3a2b1c9d4e", // by alias
		"studio":        "aaaabbbbcccc",
		"8f3a2b1c9d4e":  "8f3a2b1c9d4e", // by full id
		"8f3a":          "8f3a2b1c9d4e", // by id prefix
		"kali":          "8f3a2b1c9d4e", // by alias prefix
	}
	for ref, want := range cases {
		got, err := f.Resolve(ref)
		if err != nil {
			t.Errorf("Resolve(%q) = %v", ref, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", ref, got, want)
		}
	}

	if _, err := f.Resolve("nonexistent"); err == nil {
		t.Error("Resolve() invented a device")
	}
	if _, err := f.Resolve(""); err == nil {
		t.Error("Resolve(\"\") returned a device")
	}
}

// Picking one of several matches by directory order would make the same
// command mean different things on different machines.
func TestFleetResolveRejectsAmbiguity(t *testing.T) {
	f := fleetOf(t, "aaaa1111", "build-linux", "bbbb2222", "build-mac")

	_, err := f.Resolve("build")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Resolve() error = %v, want ErrAmbiguous", err)
	}
	// The message has to name the candidates, or the user cannot disambiguate.
	for _, want := range []string{"build-linux", "build-mac"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// An exact alias must not be shadowed by another device whose id happens to
// start with the same characters.
func TestFleetResolvePrefersExactMatch(t *testing.T) {
	f := fleetOf(t, "aaaa1111", "build", "buildxxxx", "other")

	got, err := f.Resolve("build")
	if err != nil {
		t.Fatalf("Resolve() error = %v, want the exact alias match", err)
	}
	if got != "aaaa1111" {
		t.Errorf("Resolve() = %q, want the exactly-named device", got)
	}
}

func TestFleetAliasTaken(t *testing.T) {
	f := fleetOf(t, "aaaa1111", "studio", "bbbb2222", "laptop")

	if !f.AliasTaken("studio", "bbbb2222") {
		t.Error("AliasTaken() = false for a name another device holds")
	}
	// Renaming a device to what it is already called is not a collision.
	if f.AliasTaken("studio", "aaaa1111") {
		t.Error("AliasTaken() = true for the device's own current alias")
	}
	if f.AliasTaken("unused", "aaaa1111") {
		t.Error("AliasTaken() = true for an unused name")
	}
}
