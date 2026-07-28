package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Path: filepath.Join(t.TempDir(), "creds", "credentials.json")}
}

func TestStoreSaveAndGet(t *testing.T) {
	s := newStore(t)
	id := &Identity{Provider: "github", Login: "nkoteb", Token: "gho_x", CreatedAt: time.Now().UTC()}

	if err := s.Save(id); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := s.Get("github")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Login != "nkoteb" || got.Token != "gho_x" {
		t.Errorf("Get() = %+v, want login nkoteb / token gho_x", got)
	}
}

func TestStoreGetMissingReturnsErrNotLoggedIn(t *testing.T) {
	s := newStore(t)
	if _, err := s.Get("github"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Get() error = %v, want ErrNotLoggedIn", err)
	}
}

// Adding a second provider must not clobber the first.
func TestStorePreservesOtherProviders(t *testing.T) {
	s := newStore(t)
	if err := s.Save(&Identity{Provider: "github", Login: "a", Token: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(&Identity{Provider: "gitlab", Login: "b", Token: "t2"}); err != nil {
		t.Fatal(err)
	}

	gh, err := s.Get("github")
	if err != nil {
		t.Fatalf("github lost after second save: %v", err)
	}
	if gh.Token != "t1" {
		t.Errorf("github token = %q, want t1", gh.Token)
	}

	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Errorf("List() = %v, want 2 entries", names)
	}
}

func TestStoreOverwritesSameProvider(t *testing.T) {
	s := newStore(t)
	_ = s.Save(&Identity{Provider: "github", Login: "old", Token: "t1"})
	_ = s.Save(&Identity{Provider: "github", Login: "new", Token: "t2"})

	got, err := s.Get("github")
	if err != nil {
		t.Fatal(err)
	}
	if got.Login != "new" || got.Token != "t2" {
		t.Errorf("Get() = %+v, want login new / token t2", got)
	}
}

func TestStoreDelete(t *testing.T) {
	s := newStore(t)
	_ = s.Save(&Identity{Provider: "github", Login: "a", Token: "t"})

	if err := s.Delete("github"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := s.Get("github"); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("Get() after Delete error = %v, want ErrNotLoggedIn", err)
	}
}

func TestStoreDeleteAbsentIsNotAnError(t *testing.T) {
	s := newStore(t)
	if err := s.Delete("github"); err != nil {
		t.Fatalf("Delete() of absent provider error = %v", err)
	}
}

// The credentials file holds bearer tokens; 0600 is a correctness requirement.
func TestStoreFileIsOwnerOnly(t *testing.T) {
	s := newStore(t)
	if err := s.Save(&Identity{Provider: "github", Login: "a", Token: "t"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials mode = %o, want 600", perm)
	}
}

func TestStoreSaveRejectsIdentityWithoutProvider(t *testing.T) {
	s := newStore(t)
	if err := s.Save(&Identity{Login: "a", Token: "t"}); err == nil {
		t.Fatal("Save() without provider should fail")
	}
	if err := s.Save(nil); err == nil {
		t.Fatal("Save(nil) should fail")
	}
}

func TestStoreTreatsEmptyTokenAsLoggedOut(t *testing.T) {
	s := newStore(t)
	_ = s.Save(&Identity{Provider: "github", Login: "a", Token: ""})
	if _, err := s.Get("github"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Get() with empty token error = %v, want ErrNotLoggedIn", err)
	}
}

func TestStoreHandlesEmptyAndCorruptFile(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(s.Path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("github"); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("empty file: error = %v, want ErrNotLoggedIn", err)
	}

	if err := os.WriteFile(s.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("github"); err == nil {
		t.Error("corrupt file should surface a parse error")
	}
}
