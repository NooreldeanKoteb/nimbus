package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func add(t *testing.T, repo string, e *Entry, ttl time.Duration) *Entry {
	t.Helper()
	if e.Node == "" {
		e.Node = "laptop"
	}
	saved, err := Add(repo, e, ttl)
	if err != nil {
		t.Fatalf("Add(%q): %v", e.Text, err)
	}
	return saved
}

// addExpired writes a memory whose TTL has already run out. Add always stamps
// the expiry from now, so backdating has to be done on the stored file.
func addExpired(t *testing.T, repo, node, text string) *Entry {
	t.Helper()
	e := add(t, repo, &Entry{Text: text, Node: node}, DefaultTTL)
	e.Expires = time.Now().UTC().Add(-time.Hour)
	if err := write(repo, e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAddAndLoadRoundTrip(t *testing.T) {
	repo := t.TempDir()

	saved := add(t, repo, &Entry{
		Text: "evdi-dkms breaks on every kernel upgrade",
		Tags: []string{"boot", "dkms"},
		Task: "fix-boot",
	}, DefaultTTL)

	got, err := Load(repo, "laptop", saved.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Text != saved.Text {
		t.Errorf("Text = %q, want %q", got.Text, saved.Text)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "boot" {
		t.Errorf("Tags = %v, want them preserved", got.Tags)
	}
	if got.Task != "fix-boot" {
		t.Errorf("Task = %q, want it preserved", got.Task)
	}
	if got.Tier != TierScratch {
		t.Errorf("Tier = %q, want scratch by default", got.Tier)
	}
	if got.Expires.IsZero() {
		t.Error("scratch was saved with no expiry")
	}
}

// The file is markdown so a person and Claude can both read it directly.
func TestEncodedFormatIsReadableMarkdown(t *testing.T) {
	repo := t.TempDir()
	saved := add(t, repo, &Entry{Text: "the body text", Tags: []string{"a"}}, DefaultTTL)

	data, err := os.ReadFile(saved.Path(repo))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	if !strings.HasPrefix(text, "---\n") {
		t.Errorf("no frontmatter block:\n%s", text)
	}
	if !strings.Contains(text, "\n\nthe body text\n") {
		t.Errorf("body is not plain markdown after the frontmatter:\n%s", text)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"no frontmatter": "just some text",
		"unterminated":   "---\nid: x\nstill going",
		"no id":          "---\ntier: scratch\n---\n\nbody",
		"empty":          "",
	}
	for name, input := range cases {
		if _, err := Decode([]byte(input)); err == nil {
			t.Errorf("Decode(%s) = nil error, want rejection", name)
		}
	}
}

// A body containing the delimiter must not truncate the memory.
func TestDecodeHandlesDelimiterInBody(t *testing.T) {
	e := &Entry{ID: "x", Tier: TierScratch, Node: "laptop",
		Text: "first part\n---\nsecond part", Created: time.Now(), Updated: time.Now()}

	got, err := Decode(e.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != e.Text {
		t.Errorf("Text = %q, want the full body %q", got.Text, e.Text)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"evdi-dkms breaks on kernel upgrade": "evdi-dkms-breaks-on-kernel-upgrade",
		"  Leading and TRAILING  ":           "leading-and-trailing",
		"lots!!!of???punctuation":            "lots-of-punctuation",
		"":                                   "note",
		"!!!":                                "note",
	}
	for text, want := range cases {
		if got := Slug(text); got != want {
			t.Errorf("Slug(%q) = %q, want %q", text, got, want)
		}
	}

	// Whatever comes out has to be a legal id, since it becomes a filename.
	long := strings.Repeat("word ", 40)
	if err := ValidateID(Slug(long)); err != nil {
		t.Errorf("Slug() of a long text produced an invalid id: %v", err)
	}
}

// Two similar notes must not silently overwrite each other.
func TestAddDisambiguatesCollidingSlugs(t *testing.T) {
	repo := t.TempDir()

	first := add(t, repo, &Entry{Text: "same text"}, DefaultTTL)
	second := add(t, repo, &Entry{Text: "same text"}, DefaultTTL)

	if first.ID == second.ID {
		t.Fatalf("both memories got id %q", first.ID)
	}
	for _, e := range []*Entry{first, second} {
		if _, err := os.Stat(e.Path(repo)); err != nil {
			t.Errorf("%s was not written: %v", e.ID, err)
		}
	}
}

func TestAddRejectsEmptyAndBadTier(t *testing.T) {
	repo := t.TempDir()

	if _, err := Add(repo, &Entry{Text: "  ", Node: "laptop"}, DefaultTTL); err == nil {
		t.Error("Add() accepted an empty memory")
	}
	if _, err := Add(repo, &Entry{Text: "x", Node: "laptop", Tier: "invented"}, DefaultTTL); err == nil {
		t.Error("Add() accepted an unknown tier")
	}
}

func TestValidateIDRejectsPathEscapes(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "Upper", "has space"} {
		if err := ValidateID(bad); err == nil {
			t.Errorf("ValidateID(%q) = nil, want rejection", bad)
		}
	}
}

// Scratch lives under the device that wrote it; long-term is shared. That
// split is what makes scratch conflict-free and long-term fleet-wide.
func TestTierDeterminesLocation(t *testing.T) {
	repo := t.TempDir()

	scratch := add(t, repo, &Entry{Text: "working note", Node: "laptop"}, DefaultTTL)
	if !strings.Contains(scratch.Path(repo), filepath.Join("scratch", "laptop")) {
		t.Errorf("scratch path = %q, want it under the device", scratch.Path(repo))
	}

	durable := add(t, repo, &Entry{Text: "durable fact", Node: "laptop", Tier: TierLongTerm}, 0)
	if !strings.Contains(durable.Path(repo), "long-term") {
		t.Errorf("long-term path = %q, want it shared", durable.Path(repo))
	}
	if !durable.Expires.IsZero() {
		t.Error("long-term memory was given an expiry")
	}
}

func TestPromoteMakesMemoryDurable(t *testing.T) {
	repo := t.TempDir()
	scratch := add(t, repo, &Entry{Text: "worth keeping", Node: "laptop"}, DefaultTTL)
	oldPath := scratch.Path(repo)

	promoted, err := Promote(repo, "laptop", scratch.ID)
	if err != nil {
		t.Fatalf("Promote() error = %v", err)
	}
	if promoted.Tier != TierLongTerm {
		t.Errorf("Tier = %q, want long-term", promoted.Tier)
	}
	if !promoted.Expires.IsZero() {
		t.Error("promoted memory still has an expiry")
	}
	if promoted.Text != scratch.Text {
		t.Errorf("Text = %q, want it carried over", promoted.Text)
	}

	// The scratch copy must be gone, or the memory exists twice.
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("scratch copy survived promotion")
	}
	if _, err := os.Stat(promoted.Path(repo)); err != nil {
		t.Errorf("promoted memory is not on disk: %v", err)
	}
}

func TestPromoteIsIdempotent(t *testing.T) {
	repo := t.TempDir()
	e := add(t, repo, &Entry{Text: "already durable", Node: "laptop", Tier: TierLongTerm}, 0)

	got, err := Promote(repo, "laptop", e.ID)
	if err != nil {
		t.Fatalf("Promote() on a long-term memory = %v", err)
	}
	if got.Tier != TierLongTerm {
		t.Errorf("Tier = %q", got.Tier)
	}
}

// Decay is the point: unpromoted scratch has to actually go away, or the repo
// grows without bound and injecting it degrades the session.
func TestExpireSweepsStaleScratch(t *testing.T) {
	repo := t.TempDir()

	stale := addExpired(t, repo, "laptop", "old note")
	fresh := add(t, repo, &Entry{Text: "new note", Node: "laptop"}, DefaultTTL)
	durable := add(t, repo, &Entry{Text: "kept fact", Node: "laptop", Tier: TierLongTerm}, 0)

	swept, err := Expire(repo, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 1 || swept[0].ID != stale.ID {
		t.Fatalf("swept = %v, want only the stale note", swept)
	}

	if _, err := os.Stat(stale.Path(repo)); !errors.Is(err, os.ErrNotExist) {
		t.Error("expired memory is still on disk")
	}
	for _, e := range []*Entry{fresh, durable} {
		if _, err := os.Stat(e.Path(repo)); err != nil {
			t.Errorf("%s was swept but should not have been", e.ID)
		}
	}
}

// Another device's scratch is not ours to delete: those files are owned by
// that device and removing them would race its writes.
func TestExpireOnlyTouchesOwnScratch(t *testing.T) {
	repo := t.TempDir()
	theirs := addExpired(t, repo, "desktop", "their stale note")

	swept, err := Expire(repo, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 0 {
		t.Errorf("swept = %v, want nothing from another device", swept)
	}
	if _, err := os.Stat(theirs.Path(repo)); err != nil {
		t.Error("another device's memory was deleted")
	}
}

func TestListHidesExpiredByDefault(t *testing.T) {
	repo := t.TempDir()
	addExpired(t, repo, "laptop", "old")
	add(t, repo, &Entry{Text: "current", Node: "laptop"}, DefaultTTL)

	entries, err := List(repo, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Text != "current" {
		t.Errorf("List() = %v, want only the unexpired memory", entries)
	}

	all, err := List(repo, Query{IncludeExpired: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("List(IncludeExpired) returned %d, want 2", len(all))
	}
}

func TestListFilters(t *testing.T) {
	repo := t.TempDir()
	add(t, repo, &Entry{Text: "nouveau only on this box", Node: "laptop",
		Tier: TierLongTerm, Tags: []string{"gpu"}}, 0)
	add(t, repo, &Entry{Text: "handler is stubbed", Node: "laptop", Task: "port"}, DefaultTTL)
	add(t, repo, &Entry{Text: "unrelated note", Node: "desktop"}, DefaultTTL)

	byTier, _ := List(repo, Query{Tier: TierLongTerm})
	if len(byTier) != 1 || byTier[0].Tier != TierLongTerm {
		t.Errorf("tier filter = %v", byTier)
	}

	byTask, _ := List(repo, Query{Task: "port"})
	if len(byTask) != 1 || byTask[0].Task != "port" {
		t.Errorf("task filter = %v", byTask)
	}

	byNode, _ := List(repo, Query{Tier: TierScratch, Node: "desktop"})
	if len(byNode) != 1 || byNode[0].Node != "desktop" {
		t.Errorf("node filter = %v", byNode)
	}

	// Search covers text and tags, so "gpu" finds a memory that never says it.
	byText, _ := List(repo, Query{Text: "gpu"})
	if len(byText) != 1 {
		t.Errorf("tag search = %v, want the gpu-tagged memory", byText)
	}
	if found, _ := List(repo, Query{Text: "stubbed"}); len(found) != 1 {
		t.Errorf("text search = %v", found)
	}
}

func TestListSkipsUnreadableFiles(t *testing.T) {
	repo := t.TempDir()
	add(t, repo, &Entry{Text: "good", Node: "laptop"}, DefaultTTL)

	broken := filepath.Join(ScratchDir(repo, "laptop"), "broken.md")
	if err := os.WriteFile(broken, []byte("not a memory"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := List(repo, Query{})
	if err != nil {
		t.Fatalf("List() error = %v, want the bad file skipped", err)
	}
	if len(entries) != 1 {
		t.Errorf("List() = %d entries, want the 1 readable one", len(entries))
	}
}

func TestForget(t *testing.T) {
	repo := t.TempDir()
	e := add(t, repo, &Entry{Text: "temporary", Node: "laptop"}, DefaultTTL)

	if _, err := Forget(repo, "laptop", e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.Path(repo)); !errors.Is(err, os.ErrNotExist) {
		t.Error("forgotten memory is still on disk")
	}
	if _, err := Load(repo, "laptop", e.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load() after forget = %v, want ErrNotFound", err)
	}
}

func TestJournalRecordsWhatHappened(t *testing.T) {
	repo := t.TempDir()

	for _, action := range []string{ActionAdd, ActionPromote, ActionForget} {
		if err := Record(repo, "laptop", Event{Action: action, ID: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(JournalFile(repo, "laptop"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Errorf("journal has %d lines, want 3", len(lines))
	}
	// A deletion is the one change the files can no longer explain, so it has
	// to be in the journal.
	if !strings.Contains(string(data), ActionForget) {
		t.Error("journal did not record the deletion")
	}
}

func TestOwnedPatternsMatchWhatIsWritten(t *testing.T) {
	repo := t.TempDir()
	add(t, repo, &Entry{Text: "mine", Node: "laptop"}, DefaultTTL)
	add(t, repo, &Entry{Text: "theirs", Node: "desktop"}, DefaultTTL)
	if err := Record(repo, "laptop", Event{Action: ActionAdd, ID: "x"}); err != nil {
		t.Fatal(err)
	}

	// The sync layer reapplies these after a reset to origin, so they have to
	// match this device's files and only this device's files.
	mine, _ := filepath.Glob(filepath.Join(repo, ScratchPattern("laptop")))
	if len(mine) != 1 {
		t.Errorf("scratch pattern matched %d files, want 1", len(mine))
	}
	journal, _ := filepath.Glob(filepath.Join(repo, JournalPattern("laptop")))
	if len(journal) != 1 {
		t.Errorf("journal pattern matched %d files, want 1", len(journal))
	}

	// Long-term is deliberately NOT owned: one file per fact means concurrent
	// additions never collide, so origin can win without losing anything.
	durable := add(t, repo, &Entry{Text: "shared fact", Node: "laptop", Tier: TierLongTerm}, 0)
	if matched, _ := filepath.Glob(filepath.Join(repo, ScratchPattern("laptop"))); len(matched) != 1 {
		t.Errorf("long-term memory %q was matched by the scratch pattern", durable.ID)
	}
}
