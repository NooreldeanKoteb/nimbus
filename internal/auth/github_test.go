package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub stands in for github.com. pollReplies is consumed in order, so a
// test can script "pending, pending, success" without timing games.
type fakeGitHub struct {
	pollReplies []tokenResponse
	pollCount   atomic.Int32
	login       string
	interval    int
}

func (f *fakeGitHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("device code: Accept = %q, want application/json", got)
		}
		interval := f.interval
		if interval == 0 {
			interval = 1
		}
		writeJSON(w, deviceCodeResponse{
			DeviceCode:      "dev-code-123",
			UserCode:        "WDJB-MJHT",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        interval,
		})
	})

	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		n := int(f.pollCount.Add(1)) - 1
		if n >= len(f.pollReplies) {
			n = len(f.pollReplies) - 1
		}
		writeJSON(w, f.pollReplies[n])
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]string{"login": f.login})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newGitHub(srv *httptest.Server) *GitHub {
	return &GitHub{
		ClientID: "test-client",
		BaseURL:  srv.URL,
		APIURL:   srv.URL,
		HTTP:     srv.Client(),
	}
}

func TestLoginSucceedsAfterPending(t *testing.T) {
	fake := &fakeGitHub{
		login: "nkoteb",
		pollReplies: []tokenResponse{
			{Error: "authorization_pending"},
			{Error: "authorization_pending"},
			{AccessToken: "gho_secret", Scope: "repo,read:user"},
		},
	}
	gh := newGitHub(fake.server(t))

	var prompted Verification
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	id, err := gh.Login(ctx, func(v Verification) { prompted = v })
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	if prompted.UserCode != "WDJB-MJHT" {
		t.Errorf("prompted user code = %q, want WDJB-MJHT", prompted.UserCode)
	}
	if id.Token != "gho_secret" {
		t.Errorf("token = %q, want gho_secret", id.Token)
	}
	if id.Login != "nkoteb" {
		t.Errorf("login = %q, want nkoteb", id.Login)
	}
	if id.Provider != "github" {
		t.Errorf("provider = %q, want github", id.Provider)
	}
	if len(id.Scopes) != 2 || id.Scopes[0] != "repo" || id.Scopes[1] != "read:user" {
		t.Errorf("scopes = %v, want [repo read:user]", id.Scopes)
	}
	if n := fake.pollCount.Load(); n != 3 {
		t.Errorf("polled %d times, want 3", n)
	}
}

func TestLoginPropagatesDenial(t *testing.T) {
	fake := &fakeGitHub{pollReplies: []tokenResponse{{Error: "access_denied"}}}
	gh := newGitHub(fake.server(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := gh.Login(ctx, nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("Login() error = %v, want ErrDenied", err)
	}
}

func TestLoginPropagatesExpiry(t *testing.T) {
	fake := &fakeGitHub{pollReplies: []tokenResponse{{Error: "expired_token"}}}
	gh := newGitHub(fake.server(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := gh.Login(ctx, nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("Login() error = %v, want ErrExpired", err)
	}
}

// slow_down must be honored rather than treated as a hard failure, otherwise a
// rate-limited login aborts instead of backing off.
func TestLoginHonorsSlowDown(t *testing.T) {
	fake := &fakeGitHub{
		login: "nkoteb",
		pollReplies: []tokenResponse{
			{Error: "slow_down", Interval: 1},
			{AccessToken: "gho_secret"},
		},
	}
	gh := newGitHub(fake.server(t))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	id, err := gh.Login(ctx, nil)
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if id.Token != "gho_secret" {
		t.Errorf("token = %q, want gho_secret", id.Token)
	}
}

func TestLoginRespectsContextCancellation(t *testing.T) {
	fake := &fakeGitHub{pollReplies: []tokenResponse{{Error: "authorization_pending"}}}
	gh := newGitHub(fake.server(t))

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	if _, err := gh.Login(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Login() error = %v, want DeadlineExceeded", err)
	}
}

func TestLoginRequiresClientID(t *testing.T) {
	gh := &GitHub{}
	if _, err := gh.Login(context.Background(), nil); err == nil {
		t.Fatal("Login() with no client id should fail")
	}
}

func TestValidateRejectsRevokedToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	gh := newGitHub(srv)
	err := gh.Validate(context.Background(), &Identity{Token: "stale"})
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Validate() error = %v, want ErrNotLoggedIn", err)
	}
}

func TestSplitScopes(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"   ", 0},
		{"repo", 1},
		{"repo,read:user", 2},
		{"repo, read:user", 2},
		{"repo read:user", 2},
	}
	for _, tc := range cases {
		if got := splitScopes(tc.in); len(got) != tc.want {
			t.Errorf("splitScopes(%q) = %v, want %d entries", tc.in, got, tc.want)
		}
	}
}
