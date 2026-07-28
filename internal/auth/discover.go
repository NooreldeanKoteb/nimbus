package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxRepoPages bounds pagination. An account with more than 500 repositories
// exists, but scanning further to find a state repo the user can also just name
// with --remote is not worth the API budget or the wait.
const maxRepoPages = 5

// ErrNoSuchFile means the repository has no file at that path.
var ErrNoSuchFile = fmt.Errorf("file not found in repository")

// ListRepos returns every repository the token can reach.
//
// Deliberately not the search API: search omits private repositories in some
// token configurations, and a private state repo is the normal case rather than
// the exception. Listing also covers repos shared *with* the user as a
// collaborator, which is how a second person joins a system.
func (g *GitHub) ListRepos(ctx context.Context, id *Identity) ([]Repository, error) {
	if id == nil || id.Token == "" {
		return nil, ErrNotLoggedIn
	}

	var all []Repository
	for page := 1; page <= maxRepoPages; page++ {
		endpoint := fmt.Sprintf("%s/user/repos?per_page=100&page=%d&affiliation=%s",
			g.api(), page, url.QueryEscape("owner,collaborator,organization_member"))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+id.Token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := g.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("list repos: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return nil, fmt.Errorf("list repos: token lacks the `repo` scope (run `nimbus login --force`)")
			}
			return nil, fmt.Errorf("list repos: unexpected status %s", resp.Status)
		}

		var pageRepos []Repository
		err = json.NewDecoder(resp.Body).Decode(&pageRepos)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list repos: %w", err)
		}

		all = append(all, pageRepos...)
		if len(pageRepos) < 100 {
			break
		}
	}
	return all, nil
}

// ReadFile fetches one file from a repository without cloning it.
//
// This is what makes discovery affordable: checking whether a repo is nimbus
// state costs a single request rather than a clone, so scanning an account with
// eighty repositories is still a matter of seconds.
func (g *GitHub) ReadFile(ctx context.Context, id *Identity, fullName, path string) ([]byte, error) {
	if id == nil || id.Token == "" {
		return nil, ErrNotLoggedIn
	}

	endpoint := fmt.Sprintf("%s/repos/%s/contents/%s", g.api(), fullName, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+id.Token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoSuchFile
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read %s: unexpected status %s", path, resp.Status)
	}

	var payload struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if payload.Encoding != "base64" {
		return nil, fmt.Errorf("read %s: unexpected encoding %q", path, payload.Encoding)
	}
	// GitHub wraps base64 at 60 columns, which the strict decoder rejects.
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(payload.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return decoded, nil
}

// SetTopics replaces a repository's topics, which is how a state repo becomes
// findable without opening every repo on the account.
//
// Best-effort by design: a token without write access to a repo shared by
// somebody else cannot set topics, and failing setup over a search hint would
// be the wrong trade.
func (g *GitHub) SetTopics(ctx context.Context, id *Identity, fullName string, topics []string) error {
	if id == nil || id.Token == "" {
		return ErrNotLoggedIn
	}

	body, err := json.Marshal(map[string]any{"names": topics})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/topics", g.api(), fullName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+id.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("set topics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("set topics: unexpected status %s", resp.Status)
	}
	return nil
}
