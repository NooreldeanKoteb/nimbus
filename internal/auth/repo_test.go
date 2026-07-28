package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func repoServer(t *testing.T, handler http.HandlerFunc) *GitHub {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &GitHub{ClientID: "test", BaseURL: srv.URL, APIURL: srv.URL, HTTP: srv.Client()}
}

func testIdentity() *Identity {
	return &Identity{Provider: "github", Login: "nkoteb", Token: "gho_x"}
}

func TestCreateRepoSucceeds(t *testing.T) {
	var gotBody map[string]any

	gh := repoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer gho_x" {
			t.Errorf("Authorization = %q", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, Repository{
			FullName: "nkoteb/nimbus-state",
			CloneURL: "https://github.com/nkoteb/nimbus-state.git",
			Private:  true,
		})
	})

	repo, err := gh.CreateRepo(context.Background(), testIdentity(), "nimbus-state", true)
	if err != nil {
		t.Fatalf("CreateRepo() error = %v", err)
	}
	if repo.FullName != "nkoteb/nimbus-state" {
		t.Errorf("FullName = %q", repo.FullName)
	}

	// auto_init matters: a repo with no commit cannot be cloned, which
	// defeats the point of creating it for the next device.
	if gotBody["auto_init"] != true {
		t.Error("auto_init not requested; the repo would be unclonable")
	}
	if gotBody["private"] != true {
		t.Error("private not requested for a state repo")
	}
}

func TestCreateRepoDetectsNameCollision(t *testing.T) {
	gh := repoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(w, map[string]any{
			"message": "Repository creation failed.",
			"errors":  []map[string]string{{"message": "name already exists on this account"}},
		})
	})

	_, err := gh.CreateRepo(context.Background(), testIdentity(), "nimbus-state", true)
	if !errors.Is(err, ErrRepoExists) {
		t.Fatalf("CreateRepo() error = %v, want ErrRepoExists", err)
	}
}

// 422 also covers genuinely invalid input, which must not be reported as a
// collision or callers will silently reuse a repo that does not exist.
func TestCreateRepoSurfacesOtherValidationErrors(t *testing.T) {
	gh := repoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(w, map[string]any{
			"message": "Validation Failed",
			"errors":  []map[string]string{{"message": "name is invalid"}},
		})
	})

	_, err := gh.CreateRepo(context.Background(), testIdentity(), "bad name", true)
	if err == nil {
		t.Fatal("CreateRepo() accepted an invalid name")
	}
	if errors.Is(err, ErrRepoExists) {
		t.Error("invalid name misreported as an existing repo")
	}
}

func TestCreateRepoExplainsMissingScope(t *testing.T) {
	gh := repoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	_, err := gh.CreateRepo(context.Background(), testIdentity(), "nimbus-state", true)
	if err == nil {
		t.Fatal("CreateRepo() ignored a 403")
	}
	// The fix is re-login with the right scope, so the message must say so.
	if got := err.Error(); !contains(got, "scope") || !contains(got, "nimbus login") {
		t.Errorf("error = %q, want it to name the scope and the fix", got)
	}
}

func TestCreateRepoRequiresLogin(t *testing.T) {
	gh := &GitHub{}
	if _, err := gh.CreateRepo(context.Background(), nil, "x", true); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("CreateRepo(nil identity) error = %v, want ErrNotLoggedIn", err)
	}
	if _, err := gh.CreateRepo(context.Background(), testIdentity(), "  ", true); err == nil {
		t.Error("CreateRepo() accepted a blank name")
	}
}

func TestFindRepoReturnsNilWhenAbsent(t *testing.T) {
	gh := repoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	repo, err := gh.FindRepo(context.Background(), testIdentity(), "nimbus-state")
	if err != nil {
		t.Fatalf("FindRepo() error = %v", err)
	}
	if repo != nil {
		t.Errorf("FindRepo() = %+v, want nil for an absent repo", repo)
	}
}

func TestFindRepoAcceptsQualifiedName(t *testing.T) {
	var gotPath string
	gh := repoServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, map[string]any{"full_name": "team/fleet", "size": 4})
	})

	// A shared fleet repo lives under another owner, so owner/name must work.
	if _, err := gh.FindRepo(context.Background(), testIdentity(), "team/fleet"); err != nil {
		t.Fatalf("FindRepo() error = %v", err)
	}
	if gotPath != "/repos/team/fleet" {
		t.Errorf("requested %q, want /repos/team/fleet", gotPath)
	}
}

func TestFindRepoDefaultsToAuthenticatedOwner(t *testing.T) {
	var gotPath string
	gh := repoServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, map[string]any{"full_name": "nkoteb/nimbus-state"})
	})

	if _, err := gh.FindRepo(context.Background(), testIdentity(), "nimbus-state"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/nkoteb/nimbus-state" {
		t.Errorf("requested %q", gotPath)
	}
}

func TestEnsureRepoReusesExisting(t *testing.T) {
	created := false
	gh := repoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			created = true
		}
		writeJSON(w, map[string]any{
			"full_name": "nkoteb/nimbus-state",
			"clone_url": "https://github.com/nkoteb/nimbus-state.git",
		})
	})

	repo, wasCreated, err := gh.EnsureRepo(context.Background(), testIdentity(), "nimbus-state", true)
	if err != nil {
		t.Fatalf("EnsureRepo() error = %v", err)
	}
	if wasCreated || created {
		t.Error("EnsureRepo() created a repo that already existed")
	}
	if repo.CloneURL == "" {
		t.Error("EnsureRepo() returned no clone URL")
	}
}

func TestEnsureRepoCreatesWhenAbsent(t *testing.T) {
	var posted bool
	gh := repoServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posted = true
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{
				"full_name": "nkoteb/nimbus-state",
				"clone_url": "https://github.com/nkoteb/nimbus-state.git",
			})
		case !posted:
			w.WriteHeader(http.StatusNotFound)
		default:
			writeJSON(w, map[string]any{"full_name": "nkoteb/nimbus-state"})
		}
	})

	_, wasCreated, err := gh.EnsureRepo(context.Background(), testIdentity(), "nimbus-state", true)
	if err != nil {
		t.Fatalf("EnsureRepo() error = %v", err)
	}
	if !wasCreated {
		t.Error("EnsureRepo() did not report creating the repo")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
