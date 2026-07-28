package task

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/nkoteb/nimbus/internal/claude"
)

// claudeHome points Claude Code's session storage at a temp dir and returns a
// helper that fabricates a transcript for a given session id.
func claudeHome(t *testing.T) func(sessionID string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)

	return func(sessionID string) {
		t.Helper()
		// The directory name encodes a working directory; the exact value does
		// not matter, only that transcripts are found across all of them.
		dir := filepath.Join(root, "projects", "-home-someone-project")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, sessionID+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewSessionIDIsAValidUUID(t *testing.T) {
	// `claude --session-id` rejects anything that is not a v4 UUID, so this is
	// a hard requirement rather than a formatting preference.
	uuid4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	seen := make(map[string]bool)
	for range 100 {
		id, err := claude.NewSessionID()
		if err != nil {
			t.Fatal(err)
		}
		if !uuid4.MatchString(id) {
			t.Fatalf("NewSessionID() = %q, not a v4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("NewSessionID() repeated %q", id)
		}
		seen[id] = true
	}
}

func TestBindSessionCreatesOnFirstUse(t *testing.T) {
	claudeHome(t)
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	got, err := BindSession(repo, "auth", "laptop", "laptop.local")
	if err != nil {
		t.Fatal(err)
	}
	if got.Resume {
		t.Error("first session claimed to resume a conversation that never existed")
	}
	if got.SessionID == "" {
		t.Fatal("no session id assigned")
	}
	if got.Reason == "" {
		t.Error("a fresh session gave no reason")
	}

	// The binding has to be durable, or the next resume starts over.
	saved, err := LoadSession(repo, "auth", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if saved.SessionID != got.SessionID {
		t.Errorf("saved session = %q, want %q", saved.SessionID, got.SessionID)
	}
}

// Coming back to the same machine is the case this whole mechanism exists for.
func TestBindSessionResumesWhenTranscriptExists(t *testing.T) {
	transcript := claudeHome(t)
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	first, err := BindSession(repo, "auth", "laptop", "laptop.local")
	if err != nil {
		t.Fatal(err)
	}
	transcript(first.SessionID) // the session actually ran

	second, err := BindSession(repo, "auth", "laptop", "laptop.local")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Resume {
		t.Error("returning to the same device did not resume the conversation")
	}
	if second.SessionID != first.SessionID {
		t.Errorf("session id changed from %q to %q", first.SessionID, second.SessionID)
	}
}

// A binding can outlive its transcript — reimaged machine, pruned history, or a
// binding synced from another device. Resuming would fail outright, so this
// must degrade to a fresh session rather than to an error.
func TestBindSessionStartsFreshWhenTranscriptIsGone(t *testing.T) {
	claudeHome(t)
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	stale := &Handoff{Node: "laptop", SessionID: "11111111-1111-4111-8111-111111111111"}
	if err := stale.Save(repo, "auth"); err != nil {
		t.Fatal(err)
	}

	got, err := BindSession(repo, "auth", "laptop", "laptop.local")
	if err != nil {
		t.Fatalf("BindSession() error = %v, want a graceful fresh start", err)
	}
	if got.Resume {
		t.Error("claimed to resume a transcript that is not on this device")
	}
	if got.SessionID == stale.SessionID {
		t.Error("reused a session id whose transcript is missing")
	}
	if got.Reason == "" {
		t.Error("no explanation given for abandoning the previous session")
	}
}

// Sessions are per device. Two machines working one task must not collide on
// a single conversation, because neither has the other's transcript.
func TestBindSessionIsPerDevice(t *testing.T) {
	transcript := claudeHome(t)
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	laptop, err := BindSession(repo, "auth", "laptop", "laptop.local")
	if err != nil {
		t.Fatal(err)
	}
	transcript(laptop.SessionID)

	desktop, err := BindSession(repo, "auth", "desktop", "desktop.local")
	if err != nil {
		t.Fatal(err)
	}
	if desktop.SessionID == laptop.SessionID {
		t.Error("two devices were bound to the same conversation")
	}
	if desktop.Resume {
		t.Error("a new device claimed to resume a conversation it never had")
	}
}

// The binding and the handoff share a file. Writing one must not erase the
// other, or every handoff would silently downgrade the next resume.
func TestHandoffPreservesSessionBinding(t *testing.T) {
	claudeHome(t)
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	bound, err := BindSession(repo, "auth", "laptop", "laptop.local")
	if err != nil {
		t.Fatal(err)
	}

	h := &Handoff{Node: "laptop", Next: []string{"finish the handler"}}
	if err := h.Save(repo, "auth"); err != nil {
		t.Fatal(err)
	}

	saved, err := LoadSession(repo, "auth", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if saved.SessionID != bound.SessionID {
		t.Errorf("session id = %q after a handoff, want %q preserved", saved.SessionID, bound.SessionID)
	}
	if len(saved.Next) != 1 {
		t.Errorf("handoff content was lost: %+v", saved)
	}
}

func TestLoadSessionMissingIsNotFound(t *testing.T) {
	repo := t.TempDir()
	newTask(t, repo, "auth", "goal")

	if _, err := LoadSession(repo, "auth", "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LoadSession() error = %v, want ErrNotFound", err)
	}
}

func TestTranscriptExists(t *testing.T) {
	transcript := claudeHome(t)

	if claude.TranscriptExists("") {
		t.Error("TranscriptExists(\"\") = true")
	}
	if claude.TranscriptExists("22222222-2222-4222-8222-222222222222") {
		t.Error("found a transcript that was never written")
	}

	transcript("33333333-3333-4333-8333-333333333333")
	if !claude.TranscriptExists("33333333-3333-4333-8333-333333333333") {
		t.Error("did not find a transcript that exists")
	}
}
