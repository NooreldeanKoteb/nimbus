package device

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadOrCreateIdentityPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "node.json")

	first, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity() error = %v", err)
	}
	if first.ID == "" {
		t.Fatal("generated identity has empty ID")
	}

	second, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("second LoadOrCreateIdentity() error = %v", err)
	}
	// Task affinity and audit records key off this, so it must be stable.
	if second.ID != first.ID {
		t.Errorf("ID changed across loads: %q then %q", first.ID, second.ID)
	}
}

func TestLoadOrCreateIdentityRecoversFromCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A device that cannot identify itself is useless, so this regenerates
	// rather than failing.
	id, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity() on corrupt file error = %v", err)
	}
	if id.ID == "" {
		t.Error("no identity generated after corruption")
	}
}

func TestIdentityFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	if _, err := LoadOrCreateIdentity(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("identity mode = %o, want 600", perm)
	}
}

func TestSanitizeID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ABC-123", "abc-123"},
		{"a1b2:c3/d4", "a1b2c3d4"},
		{"  spaced  ", "spaced"},
		{"!!!", "node-unknown"},
		{"", "node-unknown"},
		{strings.Repeat("a", 64), strings.Repeat("a", 32)},
	}
	for _, tc := range cases {
		if got := sanitizeID(tc.in); got != tc.want {
			t.Errorf("sanitizeID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseOSRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "os-release")
	content := `# a comment

NAME="Pop!_OS"
ID=pop
VERSION_ID="22.04"
PRETTY_NAME='Pop!_OS 22.04 LTS'
MALFORMED
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	rel := parseOSRelease(path)
	for key, want := range map[string]string{
		"NAME": "Pop!_OS", "ID": "pop", "VERSION_ID": "22.04",
		"PRETTY_NAME": "Pop!_OS 22.04 LTS",
	} {
		if got := rel[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestParseOSReleaseMissingFile(t *testing.T) {
	rel := parseOSRelease(filepath.Join(t.TempDir(), "absent"))
	if len(rel) != 0 {
		t.Errorf("parseOSRelease(absent) = %v, want empty", rel)
	}
}

func TestDetectToolsFindsPresentAndAbsent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tools := DetectTools(ctx, []Probe{
		{"sh", []string{"-c", "echo probe-ok"}},
		{"definitely-not-a-real-binary-xyz", []string{"--version"}},
	})
	if len(tools) != 2 {
		t.Fatalf("DetectTools() = %d tools, want 2", len(tools))
	}

	if !tools[0].Present {
		t.Error("sh reported absent")
	}
	if tools[0].Path == "" {
		t.Error("sh has no resolved path")
	}
	if tools[1].Present {
		t.Error("nonexistent binary reported present")
	}

	missing := MissingTools(tools)
	if len(missing) != 1 || missing[0] != "definitely-not-a-real-binary-xyz" {
		t.Errorf("MissingTools() = %v", missing)
	}
}

// Probing runs third-party binaries; a wedged one must not hang profiling.
func TestDetectToolsRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		DetectTools(ctx, []Probe{{"sh", []string{"-c", "sleep 30"}}})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DetectTools did not return with a cancelled context")
	}
}

func TestFindTool(t *testing.T) {
	tools := []Tool{{Name: "git", Present: true}, {Name: "node", Present: false}}

	if got, ok := FindTool(tools, "git"); !ok || !got.Present {
		t.Error("FindTool(git) did not return the present tool")
	}
	if _, ok := FindTool(tools, "absent"); ok {
		t.Error("FindTool(absent) reported found")
	}
}

func testProfile() *Profile {
	return &Profile{
		ID:       "desktop",
		Hostname: "pop-os",
		OS:       OSInfo{Platform: "linux", Distro: "pop", Release: "22.04", Arch: "amd64"},
		Hardware: Hardware{CPUs: 16, MemoryGB: 64, GPUs: []string{"nvidia"}, HasScreen: true},
		Tools:    []Tool{{Name: "git", Present: true}, {Name: "docker", Present: false}},
		Missing:  []string{"docker"},
	}
}

func TestCapabilitiesReflectHardwareAndTools(t *testing.T) {
	caps := testProfile().Capabilities()

	for _, want := range []string{"os:linux", "arch:amd64", "distro:pop", "display", "gpu:nvidia", "tool:git"} {
		if !slices.Contains(caps, want) {
			t.Errorf("Capabilities() missing %q; got %v", want, caps)
		}
	}
	// Absent tools must not appear, or affinity routing sends work nowhere.
	if slices.Contains(caps, "tool:docker") {
		t.Error("Capabilities() included an absent tool")
	}
	if slices.Contains(caps, "headless") {
		t.Error("Capabilities() reported both display and headless")
	}
}

// Containers share the host's /sys, so hardware probing there is misleading;
// the capability must be published so affinity rules can exclude them.
func TestCapabilitiesMarkContainer(t *testing.T) {
	p := testProfile()
	if slices.Contains(p.Capabilities(), "container") {
		t.Error("bare-metal profile reported the container capability")
	}

	p.OS.Container = true
	if !slices.Contains(p.Capabilities(), "container") {
		t.Errorf("Capabilities() = %v, want container", p.Capabilities())
	}
}

func TestCapabilitiesMarkHeadless(t *testing.T) {
	p := testProfile()
	p.Hardware.HasScreen = false

	caps := p.Capabilities()
	if !slices.Contains(caps, "headless") {
		t.Errorf("Capabilities() = %v, want headless", caps)
	}
}

func TestProfileHas(t *testing.T) {
	p := testProfile()
	if !p.Has("git") {
		t.Error("Has(git) = false")
	}
	if p.Has("docker") {
		t.Error("Has(docker) = true for an absent tool")
	}
	if p.Has("never-probed") {
		t.Error("Has(never-probed) = true")
	}
}

func TestSaveAndLoadProfile(t *testing.T) {
	repo := t.TempDir()
	p := testProfile()
	p.UpdatedAt = time.Now().UTC().Truncate(time.Second)

	path, err := p.Save(repo)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if want := filepath.Join(repo, "nodes", "desktop.json"); path != want {
		t.Errorf("Save() path = %q, want %q", path, want)
	}

	loaded, err := LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile() error = %v", err)
	}
	if loaded.ID != p.ID || loaded.Hardware.CPUs != 16 || len(loaded.Hardware.GPUs) != 1 {
		t.Errorf("LoadProfile() = %+v, want a match for the saved profile", loaded)
	}
}

// Profiles are committed to git, so the JSON must be diff-friendly.
func TestSavedProfileIsIndentedJSON(t *testing.T) {
	repo := t.TempDir()
	path, err := testProfile().Save(repo)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\n  \"id\"") {
		t.Error("profile JSON is not indented")
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("profile JSON lacks a trailing newline")
	}
	var check map[string]any
	if err := json.Unmarshal(data, &check); err != nil {
		t.Errorf("saved profile is not valid JSON: %v", err)
	}
}

func TestLoadFleetReadsAllProfiles(t *testing.T) {
	repo := t.TempDir()
	for _, id := range []string{"desktop", "laptop", "server"} {
		p := testProfile()
		p.ID = id
		if _, err := p.Save(repo); err != nil {
			t.Fatal(err)
		}
	}

	fleet, err := LoadFleet(repo)
	if err != nil {
		t.Fatalf("LoadFleet() error = %v", err)
	}
	if len(fleet) != 3 {
		t.Errorf("LoadFleet() = %d profiles, want 3", len(fleet))
	}
}

// One bad file must not hide the rest of the fleet from an operator.
func TestLoadFleetSkipsCorruptProfiles(t *testing.T) {
	repo := t.TempDir()
	p := testProfile()
	if _, err := p.Save(repo); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(repo, "nodes", "broken.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	fleet, err := LoadFleet(repo)
	if err != nil {
		t.Fatalf("LoadFleet() error = %v", err)
	}
	if len(fleet) != 1 {
		t.Errorf("LoadFleet() = %d profiles, want 1 (the intact one)", len(fleet))
	}
}

func TestLoadFleetOnEmptyRepo(t *testing.T) {
	fleet, err := LoadFleet(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFleet() error = %v", err)
	}
	if len(fleet) != 0 {
		t.Errorf("LoadFleet() = %d, want 0", len(fleet))
	}
}

func TestSummaryIncludesKeyFacts(t *testing.T) {
	got := testProfile().Summary()
	for _, want := range []string{"linux/amd64", "pop 22.04", "16 cpu", "64 GB", "nvidia"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() = %q, want it to contain %q", got, want)
		}
	}
}

// Detect must work on a machine where most probes fail, which is the norm on
// a freshly imaged device.
func TestDetectProducesUsableProfile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := &Identity{ID: "test-node", Hostname: "test-host"}
	p := Detect(ctx, id, []Probe{{"sh", []string{"-c", "true"}}})

	if p.ID != "test-node" || p.Hostname != "test-host" {
		t.Errorf("Detect() identity = %q/%q", p.ID, p.Hostname)
	}
	if p.OS.Platform == "" || p.OS.Arch == "" {
		t.Error("Detect() produced no OS platform/arch")
	}
	if p.Hardware.CPUs < 1 {
		t.Errorf("Detect() CPUs = %d, want >= 1", p.Hardware.CPUs)
	}
	if p.UpdatedAt.IsZero() {
		t.Error("Detect() left UpdatedAt zero")
	}
}
