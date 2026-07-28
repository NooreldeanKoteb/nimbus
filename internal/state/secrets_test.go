package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// leaked is a token-shaped string built at runtime, so this file does not
// itself contain something a scanner would flag.
var leaked = "ghp_" + strings.Repeat("a1B2", 9)

func newRepo(t *testing.T) *Repo {
	t.Helper()
	repo, err := Init(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func write(t *testing.T, repo *Repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo.Path, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Invariant 2 is enforced here rather than in a caller precisely because this
// is the one path everything takes. A check the caller has to remember is a
// check that gets skipped.
func TestCommitRefusesToPublishACredential(t *testing.T) {
	repo := newRepo(t)
	write(t, repo, "config/claude/settings.json", `{"env":{"GITHUB_TOKEN":"`+leaked+`"}}`)

	_, err := repo.Commit("nimbus: adopt config")
	if err == nil {
		t.Fatal("a commit containing a token succeeded")
	}

	var secretErr *SecretError
	if !errors.As(err, &secretErr) {
		t.Fatalf("got %v, want a SecretError", err)
	}
	if secretErr.Path != "config/claude/settings.json" {
		t.Errorf("the refusal names %q, want the offending file", secretErr.Path)
	}
	if strings.Contains(err.Error(), "a1B2") {
		t.Errorf("the refusal quotes the credential: %v", err)
	}
	// The suggested command has to be one that exists: a refusal that points at
	// a command nobody can run is a dead end wearing the shape of help.
	if !strings.Contains(err.Error(), "nimbus secrets encrypt") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

// The refusal has to be recoverable. A device that removes the credential must
// be able to sync again — otherwise one bad file strands the machine.
func TestRemovingTheCredentialUnblocksTheCommit(t *testing.T) {
	repo := newRepo(t)
	write(t, repo, "notes.md", "the token is "+leaked)

	if _, err := repo.Commit("nimbus: notes"); err == nil {
		t.Fatal("the commit was not refused")
	}

	write(t, repo, "notes.md", "the token lives in the keychain")
	committed, err := repo.Commit("nimbus: notes")
	if err != nil {
		t.Fatalf("the repo is still blocked after the fix: %v", err)
	}
	if !committed {
		t.Error("nothing was committed after the fix")
	}
}

// secrets/ is the encrypted store. Flagging it would mean the one directory
// built to hold credentials safely is the one that blocks every sync.
func TestTheEncryptedStoreIsNotScanned(t *testing.T) {
	repo := newRepo(t)
	write(t, repo, "secrets/github.age", "-----BEGIN AGE ENCRYPTED FILE-----\n"+leaked+"\n")

	if _, err := repo.Commit("nimbus: store a secret"); err != nil {
		t.Fatalf("the encrypted store blocked its own commit: %v", err)
	}
}

// Nothing nimbus writes trips the scanner, so an ordinary sync must not slow
// down or fail because this check exists.
func TestOrdinaryStateCommitsAreUnaffected(t *testing.T) {
	repo := newRepo(t)
	write(t, repo, "nodes/laptop.json", `{"id":"laptop","hostname":"pop-os"}`)
	write(t, repo, "audit/laptop.jsonl", `{"seq":1,"action":"init","hash":"9f2c4e1a"}`)
	write(t, repo, "memory/long-term/postgres.md", "postgres listens on 5433 here\n")

	if _, err := repo.Commit("nimbus: init"); err != nil {
		t.Fatalf("an ordinary commit was refused: %v", err)
	}
}
