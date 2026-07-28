package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bareRemote creates a local bare repo to stand in for origin, so push/pull
// are exercised without a network or credentials.
func bareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatalf("create bare remote: %v", err)
	}
	return dir
}

func TestInitCreatesRepo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	repo, err := Init(path, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if repo.Path != path {
		t.Errorf("Path = %q, want %q", repo.Path, path)
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		t.Errorf("no .git directory created: %v", err)
	}
}

// nimbus init must be safe to re-run on an already-configured device.
func TestInitIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if _, err := Init(path, nil); err != nil {
		t.Fatalf("first Init() error = %v", err)
	}
	if _, err := Init(path, nil); err != nil {
		t.Fatalf("second Init() error = %v", err)
	}
}

func TestOpenNonRepoReturnsErrNotARepo(t *testing.T) {
	_, err := Open(t.TempDir(), nil)
	if !errors.Is(err, ErrNotARepo) {
		t.Fatalf("Open() error = %v, want ErrNotARepo", err)
	}
}

// Unattended sync must not error just because nothing changed.
func TestCommitOnCleanTreeReportsNothingToDo(t *testing.T) {
	repo, err := Init(filepath.Join(t.TempDir(), "state"), nil)
	if err != nil {
		t.Fatal(err)
	}

	committed, err := repo.Commit("nimbus: sync")
	if err != nil {
		t.Fatalf("Commit() on clean tree error = %v", err)
	}
	if committed {
		t.Error("Commit() reported a commit on a clean tree")
	}
}

func TestCommitStagesNewFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	repo, err := Init(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, path, "memory/long-term/fact.md", "the desktop has the GPU")

	committed, err := repo.Commit("nimbus: add fact")
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if !committed {
		t.Fatal("Commit() reported nothing to commit")
	}

	clean, changed, err := repo.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Errorf("tree still dirty after commit: %v", changed)
	}
}

func TestStatusListsChangedPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	repo, err := Init(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, path, "notes.md", "hello")

	clean, changed, err := repo.Status()
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Fatal("Status() reported clean with an untracked file present")
	}
	if len(changed) != 1 || changed[0] != "notes.md" {
		t.Errorf("changed = %v, want [notes.md]", changed)
	}
}

func TestHeadRefAfterCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	repo, _ := Init(path, nil)
	writeFile(t, path, "a.txt", "a")
	if _, err := repo.Commit("init"); err != nil {
		t.Fatal(err)
	}

	branch, err := repo.HeadRef()
	if err != nil {
		t.Fatalf("HeadRef() error = %v", err)
	}
	if branch == "" {
		t.Error("HeadRef() returned empty branch name")
	}
}

func TestSetRemoteAndRemoteURL(t *testing.T) {
	repo, _ := Init(filepath.Join(t.TempDir(), "state"), nil)

	if got := repo.RemoteURL(); got != "" {
		t.Errorf("RemoteURL() on fresh repo = %q, want empty", got)
	}
	if err := repo.SetRemote("https://example.com/a.git"); err != nil {
		t.Fatalf("SetRemote() error = %v", err)
	}
	if got := repo.RemoteURL(); got != "https://example.com/a.git" {
		t.Errorf("RemoteURL() = %q", got)
	}
}

// Re-running init with a different remote must replace, not duplicate, origin.
func TestSetRemoteReplacesExisting(t *testing.T) {
	repo, _ := Init(filepath.Join(t.TempDir(), "state"), nil)

	if err := repo.SetRemote("https://example.com/old.git"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRemote("https://example.com/new.git"); err != nil {
		t.Fatalf("SetRemote() replace error = %v", err)
	}
	if got := repo.RemoteURL(); got != "https://example.com/new.git" {
		t.Errorf("RemoteURL() = %q, want new.git", got)
	}
}

func TestSetRemoteToSameURLIsNoop(t *testing.T) {
	repo, _ := Init(filepath.Join(t.TempDir(), "state"), nil)
	url := "https://example.com/a.git"
	if err := repo.SetRemote(url); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRemote(url); err != nil {
		t.Fatalf("SetRemote() with identical URL error = %v", err)
	}
}

func TestPushToLocalRemote(t *testing.T) {
	origin := bareRemote(t)
	path := filepath.Join(t.TempDir(), "state")

	repo, err := Init(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRemote(origin); err != nil {
		t.Fatal(err)
	}

	writeFile(t, path, "memory/journal.jsonl", `{"event":"boot"}`)
	if _, err := repo.Commit("nimbus: first sync"); err != nil {
		t.Fatal(err)
	}

	if err := repo.Push(context.Background()); err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	// Cloning the remote back proves the content actually landed.
	clonePath := filepath.Join(t.TempDir(), "clone")
	if _, err := Clone(context.Background(), origin, clonePath, nil); err != nil {
		t.Fatalf("Clone() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(clonePath, "memory", "journal.jsonl")); err != nil {
		t.Errorf("pushed file missing from clone: %v", err)
	}
}

func TestPushWhenUpToDateIsNotAnError(t *testing.T) {
	origin := bareRemote(t)
	path := filepath.Join(t.TempDir(), "state")

	repo, _ := Init(path, nil)
	_ = repo.SetRemote(origin)
	writeFile(t, path, "a.txt", "a")
	_, _ = repo.Commit("first")

	if err := repo.Push(context.Background()); err != nil {
		t.Fatalf("first Push() error = %v", err)
	}
	if err := repo.Push(context.Background()); err != nil {
		t.Fatalf("second Push() should be a no-op, got %v", err)
	}
}

// Clone on an existing path opens it, so `nimbus init` can be re-run safely.
func TestCloneOnExistingRepoOpensIt(t *testing.T) {
	origin := bareRemote(t)
	path := filepath.Join(t.TempDir(), "state")

	repo, _ := Init(path, nil)
	_ = repo.SetRemote(origin)
	writeFile(t, path, "a.txt", "a")
	_, _ = repo.Commit("first")
	_ = repo.Push(context.Background())

	reopened, err := Clone(context.Background(), origin, path, nil)
	if err != nil {
		t.Fatalf("Clone() over existing repo error = %v", err)
	}
	if reopened.Path != path {
		t.Errorf("Path = %q, want %q", reopened.Path, path)
	}
}

// A repo with no origin yet must not fail sync; it commits locally instead.
func TestPullWithoutRemoteIsNotAnError(t *testing.T) {
	repo, _ := Init(filepath.Join(t.TempDir(), "state"), nil)
	if err := repo.Pull(context.Background()); err != nil {
		t.Fatalf("Pull() with no remote error = %v", err)
	}
}

func TestTokenAuthEmptyTokenYieldsNil(t *testing.T) {
	if got := TokenAuth(""); got != nil {
		t.Errorf("TokenAuth(\"\") = %v, want nil", got)
	}
	if got := TokenAuth("gho_x"); got == nil {
		t.Error("TokenAuth(token) = nil, want credentials")
	}
}
