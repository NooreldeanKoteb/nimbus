package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrRepoExists means the named repository is already present on the account.
var ErrRepoExists = errors.New("repository already exists")

// Repository is a created or discovered remote repo.
type Repository struct {
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	SSHURL   string `json:"ssh_url"`
	HTMLURL  string `json:"html_url"`
	Private  bool   `json:"private"`
	Empty    bool   `json:"-"`
}

// CreateRepo creates a repository on the authenticated account.
//
// autoInit is important: a repo with no initial commit cannot be cloned, and
// the whole point here is that the next device can clone it immediately.
func (g *GitHub) CreateRepo(ctx context.Context, id *Identity, name string, private bool) (*Repository, error) {
	if id == nil || id.Token == "" {
		return nil, ErrNotLoggedIn
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("repository name is empty")
	}

	body, err := json.Marshal(map[string]any{
		"name":        name,
		"private":     private,
		"auto_init":   true,
		"description": "Nimbus state: device profiles, memory, config, and audit trail",
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.api()+"/user/repos", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+id.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("create repo: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated:
		var repo Repository
		if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
			return nil, fmt.Errorf("create repo: %w", err)
		}
		return &repo, nil

	case http.StatusUnprocessableEntity:
		// GitHub returns 422 both for a name collision and for genuinely
		// invalid input, so the message has to be inspected to tell them apart.
		var apiErr struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		detail := apiErr.Message
		for _, e := range apiErr.Errors {
			if strings.Contains(strings.ToLower(e.Message), "already exists") {
				return nil, ErrRepoExists
			}
			if e.Message != "" {
				detail = e.Message
			}
		}
		return nil, fmt.Errorf("create repo: %s", detail)

	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("create repo: token lacks the `repo` scope (run `nimbus login --force`)")

	default:
		return nil, fmt.Errorf("create repo: unexpected status %s", resp.Status)
	}
}

// FindRepo looks up an existing repository owned by the authenticated user.
func (g *GitHub) FindRepo(ctx context.Context, id *Identity, name string) (*Repository, error) {
	if id == nil || id.Token == "" {
		return nil, ErrNotLoggedIn
	}

	owner := id.Login
	// Accept a fully qualified owner/name so a shared fleet repo works too.
	if o, n, ok := strings.Cut(name, "/"); ok {
		owner, name = o, n
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api()+"/repos/"+owner+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+id.Token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("find repo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("find repo: unexpected status %s", resp.Status)
	}

	var repo struct {
		Repository
		Size int `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		return nil, fmt.Errorf("find repo: %w", err)
	}
	out := repo.Repository
	out.Empty = repo.Size == 0
	return &out, nil
}

// EnsureRepo returns the named repo, creating it when absent. This is what
// makes first-run setup a single command with no trip to a browser.
func (g *GitHub) EnsureRepo(ctx context.Context, id *Identity, name string, private bool) (*Repository, bool, error) {
	existing, err := g.FindRepo(ctx, id, name)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	created, err := g.CreateRepo(ctx, id, name, private)
	if err != nil {
		return nil, false, err
	}

	// GitHub occasionally reports 201 slightly before the repo is clonable,
	// so confirm it is really there rather than handing back a URL that 404s.
	for range 3 {
		if found, ferr := g.FindRepo(ctx, id, name); ferr == nil && found != nil {
			return created, true, nil
		}
		time.Sleep(time.Second)
	}
	return created, true, nil
}
