package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTokenPageURLPreselectsScopes(t *testing.T) {
	gh := &GitHub{}
	raw := gh.TokenPageURL()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("TokenPageURL() is not a valid URL: %v", err)
	}
	if parsed.Host != "github.com" || parsed.Path != "/settings/tokens/new" {
		t.Errorf("URL = %q, want github.com/settings/tokens/new", raw)
	}

	// Pre-selecting scopes is the entire point: the user should only have to
	// click Generate, not reason about permissions.
	scopes := parsed.Query().Get("scopes")
	for _, want := range DefaultGitHubScopes {
		if !strings.Contains(scopes, want) {
			t.Errorf("scopes = %q, want it to include %q", scopes, want)
		}
	}
	if parsed.Query().Get("description") == "" {
		t.Error("no description pre-filled; the token would be unlabelled in GitHub's UI")
	}
}

func TestTokenPageURLHonorsBaseURL(t *testing.T) {
	gh := &GitHub{BaseURL: "https://ghe.internal"}
	if !strings.HasPrefix(gh.TokenPageURL(), "https://ghe.internal/") {
		t.Errorf("TokenPageURL() = %q, want the configured base", gh.TokenPageURL())
	}
}

func TestLoginWithTokenIdentifiesUser(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ghp_valid" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:user")
		writeJSON(w, map[string]string{"login": "nkoteb"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	gh := &GitHub{APIURL: srv.URL, HTTP: srv.Client()}
	id, err := gh.LoginWithToken(context.Background(), "ghp_valid")
	if err != nil {
		t.Fatalf("LoginWithToken() error = %v", err)
	}
	if id.Login != "nkoteb" || id.Token != "ghp_valid" || id.Provider != "github" {
		t.Errorf("identity = %+v", id)
	}
	// Scopes come from the response header, not the request.
	if len(id.Scopes) != 2 {
		t.Errorf("Scopes = %v, want 2 entries", id.Scopes)
	}
}

// A pasted token frequently picks up surrounding whitespace or a newline.
func TestLoginWithTokenTrimsWhitespace(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_valid" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]string{"login": "nkoteb"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	gh := &GitHub{APIURL: srv.URL, HTTP: srv.Client()}
	id, err := gh.LoginWithToken(context.Background(), "  ghp_valid\n")
	if err != nil {
		t.Fatalf("LoginWithToken() error = %v", err)
	}
	if id.Token != "ghp_valid" {
		t.Errorf("Token = %q, want it trimmed", id.Token)
	}
}

func TestLoginWithTokenRejectsBadToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	gh := &GitHub{APIURL: srv.URL, HTTP: srv.Client()}
	_, err := gh.LoginWithToken(context.Background(), "ghp_bad")
	if err == nil {
		t.Fatal("LoginWithToken() accepted a rejected token")
	}
	// The message has to tell the user what to actually do about it.
	if !strings.Contains(err.Error(), "copied in full") {
		t.Errorf("error = %q, want actionable guidance", err)
	}
}

func TestLoginWithTokenRejectsEmpty(t *testing.T) {
	gh := &GitHub{}
	for _, in := range []string{"", "   ", "\n"} {
		if _, err := gh.LoginWithToken(context.Background(), in); err == nil {
			t.Errorf("LoginWithToken(%q) accepted an empty token", in)
		}
	}
}

// Fine-grained tokens report no scopes; that is valid, not a failure.
func TestLoginWithTokenAcceptsFineGrainedToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"login": "nkoteb"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	gh := &GitHub{APIURL: srv.URL, HTTP: srv.Client()}
	id, err := gh.LoginWithToken(context.Background(), "github_pat_x")
	if err != nil {
		t.Fatalf("LoginWithToken() error = %v", err)
	}
	if len(id.Scopes) != 0 {
		t.Errorf("Scopes = %v, want none for a fine-grained token", id.Scopes)
	}
}
