// Package state manages the git-backed state repo described in DESIGN.md §4.
//
// git is embedded via go-git rather than shelled out to, because a bare device
// is not guaranteed to have a git binary. When a system git is present it is
// still faster, but correctness must not depend on it.
package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// ErrNotARepo means the path exists but is not a git working tree.
var ErrNotARepo = errors.New("not a nimbus state repo")

// Repo is an open state repo on disk.
type Repo struct {
	Path string
	repo *git.Repository
	auth transport.AuthMethod
}

// TokenAuth builds credentials for an HTTPS remote from a provider token.
// GitHub accepts any non-empty username alongside a token; x-access-token is
// the documented convention.
func TokenAuth(token string) transport.AuthMethod {
	if token == "" {
		return nil
	}
	return &http.BasicAuth{Username: "x-access-token", Password: token}
}

// Clone fetches url into path. An existing repo at path is opened instead,
// which makes `nimbus init` safe to re-run.
func Clone(ctx context.Context, url, path string, auth transport.AuthMethod) (*Repo, error) {
	if existing, err := Open(path, auth); err == nil {
		return existing, nil
	}

	repo, err := git.PlainCloneContext(ctx, path, false, &git.CloneOptions{
		URL:  url,
		Auth: auth,
	})
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", url, err)
	}
	return &Repo{Path: path, repo: repo, auth: auth}, nil
}

// Init creates a new empty state repo at path.
func Init(path string, auth transport.AuthMethod) (*Repo, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	repo, err := git.PlainInit(path, false)
	if errors.Is(err, git.ErrRepositoryAlreadyExists) {
		return Open(path, auth)
	}
	if err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	return &Repo{Path: path, repo: repo, auth: auth}, nil
}

// Open loads an existing state repo.
func Open(path string, auth transport.AuthMethod) (*Repo, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, ErrNotARepo
	}
	return &Repo{Path: path, repo: repo, auth: auth}, nil
}

// Status reports whether the working tree is clean, plus the changed paths.
func (r *Repo) Status() (bool, []string, error) {
	wt, err := r.repo.Worktree()
	if err != nil {
		return false, nil, err
	}
	st, err := wt.Status()
	if err != nil {
		return false, nil, err
	}

	changed := make([]string, 0, len(st))
	for path := range st {
		changed = append(changed, path)
	}
	return len(changed) == 0, changed, nil
}

// Commit stages everything and commits. It reports false when the tree was
// already clean, so callers can skip an empty push.
//
// Sync is unattended (DESIGN.md §8), so this must never fail merely because
// there was nothing to do.
func (r *Repo) Commit(message string) (bool, error) {
	wt, err := r.repo.Worktree()
	if err != nil {
		return false, err
	}

	clean, changed, err := r.Status()
	if err != nil {
		return false, err
	}
	if clean {
		return false, nil
	}

	// Invariant 2 (DESIGN.md §12): never commit unencrypted secrets. Enforced
	// here rather than in a caller because this is the one path everything
	// takes — a check the caller has to remember is a check that gets skipped.
	if err := r.scanForSecrets(changed); err != nil {
		return false, err
	}

	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return false, fmt.Errorf("stage changes: %w", err)
	}

	_, err = wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "nimbus",
			Email: "nimbus@localhost",
			When:  time.Now(),
		},
	})
	if err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// Push sends commits to origin. An up-to-date remote is not an error.
func (r *Repo) Push(ctx context.Context) error {
	err := r.repo.PushContext(ctx, &git.PushOptions{Auth: r.auth})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// Pull fast-forwards from origin. An up-to-date tree is not an error.
func (r *Repo) Pull(ctx context.Context) error {
	wt, err := r.repo.Worktree()
	if err != nil {
		return err
	}
	err = wt.PullContext(ctx, &git.PullOptions{Auth: r.auth})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	// A freshly initialised repo has no upstream yet; that is expected.
	if errors.Is(err, transport.ErrEmptyRemoteRepository) || errors.Is(err, git.ErrRemoteNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	return nil
}

// SetRemote points origin at url, replacing any existing origin.
func (r *Repo) SetRemote(url string) error {
	if existing, err := r.repo.Remote("origin"); err == nil {
		if len(existing.Config().URLs) > 0 && existing.Config().URLs[0] == url {
			return nil
		}
		if err := r.repo.DeleteRemote("origin"); err != nil {
			return fmt.Errorf("replace remote: %w", err)
		}
	}
	_, err := r.repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}})
	if err != nil {
		return fmt.Errorf("set remote: %w", err)
	}
	return nil
}

// RemoteURL returns origin's URL, or "" when no origin is configured.
func (r *Repo) RemoteURL() string {
	remote, err := r.repo.Remote("origin")
	if err != nil || len(remote.Config().URLs) == 0 {
		return ""
	}
	return remote.Config().URLs[0]
}

// HeadRef returns the short name of the checked-out branch.
func (r *Repo) HeadRef() (string, error) {
	head, err := r.repo.Head()
	if err != nil {
		return "", err
	}
	return head.Name().Short(), nil
}

// LastCommitTime is when the repo last changed.
//
// A more honest liveness signal than asking the service manager whether the
// daemon is running: a process can be up and failing every cycle, but the
// commit timestamp only moves when a sync actually did something.
func (r *Repo) LastCommitTime() (time.Time, error) {
	head, err := r.repo.Head()
	if err != nil {
		return time.Time{}, err
	}
	commit, err := r.repo.CommitObject(head.Hash())
	if err != nil {
		return time.Time{}, err
	}
	return commit.Committer.When, nil
}
