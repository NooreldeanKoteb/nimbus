package task

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nkoteb/nimbus/internal/device"
)

func TestRecordAndMergeProgressAcrossDevices(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "shared", "work done everywhere")

	if err := Record(repo, "shared", "laptop", KindNote, "wrote the client"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := Record(repo, "shared", "desktop", KindNote, "wrote the server"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := Record(repo, "shared", "laptop", KindHandoff, "handed off"); err != nil {
		t.Fatal(err)
	}

	events, err := Progress(repo, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("Progress() returned %d events, want 3", len(events))
	}

	// The merged view has to be one chronological timeline, not one device's
	// shard followed by another's.
	want := []string{"wrote the client", "wrote the server", "handed off"}
	for i, w := range want {
		if events[i].Text != w {
			t.Errorf("event %d = %q, want %q", i, events[i].Text, w)
		}
	}
}

// Each device appending only to its own shard is what makes concurrent work
// conflict-free; verify the files really are separate.
func TestProgressShardsArePerDevice(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "shared", "goal")

	if err := Record(repo, "shared", "laptop", KindNote, "a"); err != nil {
		t.Fatal(err)
	}
	if err := Record(repo, "shared", "desktop", KindNote, "b"); err != nil {
		t.Fatal(err)
	}

	for _, node := range []string{"laptop", "desktop"} {
		if _, err := os.Stat(ProgressFile(repo, "shared", node)); err != nil {
			t.Errorf("no shard for %s: %v", node, err)
		}
	}
}

func TestProgressSkipsMalformedLines(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "shared", "goal")
	if err := Record(repo, "shared", "laptop", KindNote, "good"); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(ProgressFile(repo, "shared", "laptop"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{ truncated\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	events, err := Progress(repo, "shared")
	if err != nil {
		t.Fatalf("Progress() error = %v, want the bad line skipped", err)
	}
	if len(events) != 1 {
		t.Errorf("Progress() returned %d events, want the 1 readable one", len(events))
	}
}

func TestHandoffRoundTrip(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	h := &Handoff{
		Node: "laptop", Host: "laptop.local", Branch: "feat/jwt",
		Done:    []string{"client token refresh"},
		Next:    []string{"server verification middleware"},
		Outside: []string{"postgres running in docker on :5432"},
		Notes:   "the refresh endpoint is stubbed",
	}
	if err := h.Save(repo, "auth"); err != nil {
		t.Fatal(err)
	}

	got, err := LatestHandoff(repo, "auth", "")
	if err != nil {
		t.Fatalf("LatestHandoff() error = %v", err)
	}
	if got.Notes != h.Notes || len(got.Outside) != 1 {
		t.Errorf("LatestHandoff() = %+v, want the handoff that was saved", got)
	}
	if got.At.IsZero() {
		t.Error("Save() did not stamp a time")
	}
}

// Resuming wants the note the *other* device left, not the one this device
// wrote before it walked away.
func TestLatestHandoffPrefersAnotherDevice(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	mine := &Handoff{Node: "desktop", Notes: "mine, newer", At: time.Now().UTC()}
	theirs := &Handoff{Node: "laptop", Notes: "theirs, older", At: time.Now().UTC().Add(-time.Hour)}
	for _, h := range []*Handoff{mine, theirs} {
		if err := h.Save(repo, "auth"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := LatestHandoff(repo, "auth", "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if got.Node != "laptop" {
		t.Errorf("LatestHandoff() came from %s, want the other device", got.Node)
	}

	// With nothing from anyone else, our own stale note beats no note at all.
	solo := t.TempDir()
	newTask(t, solo, "auth", "goal")
	if err := mine.Save(solo, "auth"); err != nil {
		t.Fatal(err)
	}
	got, err = LatestHandoff(solo, "auth", "desktop")
	if err != nil || got.Node != "desktop" {
		t.Errorf("LatestHandoff() = %v/%v, want a fallback to our own note", got, err)
	}
}

func TestLatestHandoffMissingIsNotFound(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	if _, err := LatestHandoff(repo, "auth", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("LatestHandoff() error = %v, want ErrNotFound", err)
	}
}

// Saving twice from one device replaces that device's note rather than
// accumulating stale ones.
func TestHandoffReplacesPreviousFromSameDevice(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	for _, note := range []string{"first pass", "second pass"} {
		h := &Handoff{Node: "laptop", Notes: note}
		if err := h.Save(repo, "auth"); err != nil {
			t.Fatal(err)
		}
	}

	all, err := Handoffs(repo, "auth")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Notes != "second pass" {
		t.Errorf("Handoffs() = %d entries (%v), want only the latest", len(all), all)
	}
}

func TestHandoffEmpty(t *testing.T) {
	if !(&Handoff{Node: "x"}).Empty() {
		t.Error("Empty() = false for a handoff with no content")
	}
	if (&Handoff{Node: "x", Next: []string{"do the thing"}}).Empty() {
		t.Error("Empty() = true for a handoff with a next step")
	}
	if (&Handoff{Node: "x", Notes: "something"}).Empty() {
		t.Error("Empty() = true for a handoff with notes")
	}
}

func TestOwnedPatternsMatchShards(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")
	if err := Record(repo, "auth", "laptop", KindNote, "x"); err != nil {
		t.Fatal(err)
	}
	if err := (&Handoff{Node: "laptop", Notes: "y"}).Save(repo, "auth"); err != nil {
		t.Fatal(err)
	}

	// The sync layer reapplies these patterns after a hard reset to origin, so
	// they have to match what the writers actually produced.
	for _, pattern := range []string{ProgressPattern("laptop"), SessionPattern("laptop")} {
		matches, err := filepath.Glob(filepath.Join(repo, pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 1 {
			t.Errorf("pattern %q matched %d files, want 1", pattern, len(matches))
		}
	}

	// Another device's shards must not match, or a reconcile would resurrect
	// our stale copy of their work.
	matches, _ := filepath.Glob(filepath.Join(repo, ProgressPattern("desktop")))
	if len(matches) != 0 {
		t.Errorf("pattern for desktop matched %v, want nothing", matches)
	}
}

func TestBriefIncludesDeviceTaskAndHandoff(t *testing.T) {
	repo := t.TempDir()
	tk := newTask(t, repo, "auth", "move auth to JWT")
	tk.Branch = "feat/jwt"
	tk.Take("desktop", "desktop.local")
	if err := tk.Save(repo); err != nil {
		t.Fatal(err)
	}
	if err := Record(repo, "auth", "laptop", KindNote, "wrote the client"); err != nil {
		t.Fatal(err)
	}
	h := &Handoff{Node: "laptop", Next: []string{"server middleware"}}
	if err := h.Save(repo, "auth"); err != nil {
		t.Fatal(err)
	}

	profile := &device.Profile{
		ID: "desktop", Hostname: "desktop.local",
		OS:      device.OSInfo{Platform: "linux", Arch: "amd64"},
		Missing: []string{"gh"},
	}

	brief := Brief(repo, profile, tk, 0)
	for _, want := range []string{
		"desktop.local", "move auth to JWT", "feat/jwt",
		"server middleware", "wrote the client", "gh",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("Brief() is missing %q:\n%s", want, brief)
		}
	}
}

// The brief has to warn when the device cannot do the work, or Claude will
// confidently attempt steps that cannot succeed here.
func TestBriefFlagsUnmetRequirements(t *testing.T) {
	repo := t.TempDir()
	tk := newTask(t, repo, "train", "train the model")
	tk.Needs = []string{"gpu:nvidia"}
	if err := tk.Save(repo); err != nil {
		t.Fatal(err)
	}

	profile := &device.Profile{
		ID: "laptop", Hostname: "laptop.local",
		OS: device.OSInfo{Platform: "darwin", Arch: "arm64"},
	}

	brief := Brief(repo, profile, tk, 0)
	if !strings.Contains(brief, "gpu:nvidia") || !strings.Contains(brief, "does not meet") {
		t.Errorf("Brief() did not flag the missing GPU:\n%s", brief)
	}
}

// A device with nothing claimed still needs to know where it is running.
func TestBriefWithoutTaskStillDescribesDevice(t *testing.T) {
	profile := &device.Profile{
		ID: "laptop", Hostname: "laptop.local",
		OS: device.OSInfo{Platform: "darwin", Arch: "arm64"},
	}

	brief := Brief(t.TempDir(), profile, nil, 0)
	if !strings.Contains(brief, "laptop.local") {
		t.Errorf("Brief() dropped the device section:\n%s", brief)
	}
	if !strings.Contains(brief, "No task claimed") {
		t.Errorf("Brief() did not say there is no task:\n%s", brief)
	}
}

func TestBriefLimitsProgressEvents(t *testing.T) {
	repo := t.TempDir()
	tk := newTask(t, repo, "long", "lots of history")
	for i := range 30 {
		if err := Record(repo, "long", "laptop", KindNote, string(rune('a'+i%26))); err != nil {
			t.Fatal(err)
		}
	}

	brief := Brief(repo, nil, tk, 5)

	_, timeline, ok := strings.Cut(brief, "## Recent progress")
	if !ok {
		t.Fatalf("Brief() has no progress section:\n%s", brief)
	}
	var rendered int
	for _, line := range strings.Split(timeline, "\n") {
		if strings.HasPrefix(line, "- ") {
			rendered++
		}
	}
	if rendered != 5 {
		t.Errorf("Brief() rendered %d events, want 5", rendered)
	}
}
