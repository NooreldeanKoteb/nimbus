package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// SyncResult describes what a sync actually did, so callers can report
// honestly instead of always claiming success.
type SyncResult struct {
	Committed  bool
	Pushed     bool
	Pulled     bool
	Reconciled bool
	// Offline is set when the remote was unreachable. The commit still
	// happened locally and will push on the next attempt.
	Offline bool
	Detail  string
}

// maxSyncAttempts bounds the reconcile loop. Two devices pushing at the same
// instant can lose one race; three is generous and still terminates.
const maxSyncAttempts = 3

// Sync commits pending changes and reconciles with origin.
//
// owned lists repo-relative paths this device is authoritative for — its own
// node profile, audit log, and per-task shards. Entries may be glob patterns.
// When history has diverged, those paths are reapplied on top of origin rather
// than merged, which is correct here because no other device ever writes them.
// Anything outside owned is taken from origin, so a concurrently-edited shared
// file is not silently reverted.
//
// Being offline is never an error: the commit is durable locally and the next
// sync pushes it.
func (r *Repo) Sync(ctx context.Context, message string, owned []string) (*SyncResult, error) {
	result := &SyncResult{}

	committed, err := r.Commit(message)
	if err != nil {
		return result, err
	}
	result.Committed = committed

	if r.RemoteURL() == "" {
		result.Detail = "no origin configured"
		return result, nil
	}

	for attempt := range maxSyncAttempts {
		// Fast-forward first: the overwhelmingly common case is that nothing
		// else changed and this is a plain pull-then-push.
		if err := r.Pull(ctx); err == nil {
			result.Pulled = true
		} else if isOffline(err) {
			result.Offline = true
			result.Detail = "remote unreachable; committed locally"
			return result, nil
		}

		err := r.Push(ctx)
		switch {
		case err == nil:
			result.Pushed = true
			return result, nil

		case isOffline(err):
			result.Offline = true
			result.Detail = "remote unreachable; committed locally"
			return result, nil

		case isDiverged(err):
			// Another device pushed between our pull and our push.
			if attempt == maxSyncAttempts-1 {
				return result, fmt.Errorf("sync: could not reconcile after %d attempts: %w",
					maxSyncAttempts, err)
			}
			if rerr := r.reconcile(ctx, message, owned); rerr != nil {
				return result, rerr
			}
			result.Reconciled = true

		default:
			return result, err
		}
	}

	return result, nil
}

// reconcile rebuilds local history on top of origin, preserving this device's
// own files.
//
// The sequence is deliberate: capture our files first, because the hard reset
// that follows will overwrite them with origin's versions.
func (r *Repo) reconcile(ctx context.Context, message string, owned []string) error {
	paths, err := r.expandOwned(owned)
	if err != nil {
		return err
	}

	saved := make(map[string][]byte, len(paths))
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(r.Path, rel))
		if err != nil {
			// A path we have not written yet is not an error.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("reconcile: read %s: %w", rel, err)
		}
		saved[rel] = data
	}

	if err := r.repo.FetchContext(ctx, &git.FetchOptions{Auth: r.auth}); err != nil &&
		!errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("reconcile: fetch: %w", err)
	}

	branch, err := r.HeadRef()
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	remoteRef, err := r.repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		return fmt.Errorf("reconcile: no origin/%s: %w", branch, err)
	}

	// Remember our own history before it is thrown away: the reset below drops
	// every commit origin has not seen, including files we added.
	var mine plumbing.Hash
	if head, herr := r.repo.Head(); herr == nil {
		mine = head.Hash()
	}

	wt, err := r.repo.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Reset(&git.ResetOptions{Commit: remoteRef.Hash(), Mode: git.HardReset}); err != nil {
		return fmt.Errorf("reconcile: reset to origin/%s: %w", branch, err)
	}

	// Restore before the owned files, so an owned path always wins.
	if !mine.IsZero() {
		if err := r.restoreAdditions(mine, remoteRef.Hash()); err != nil {
			return err
		}
	}

	for rel, data := range saved {
		path := filepath.Join(r.Path, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("reconcile: restore %s: %w", rel, err)
		}
	}

	if _, err := r.Commit(message + " (reconciled)"); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	return nil
}

// restoreAdditions puts back files we committed that origin has never seen.
//
// Without this, a hard reset to origin silently destroys anything shared that
// this device created while another device was pushing — two machines creating
// different tasks, or different long-term memories, and the second one's work
// disappears. Only *additions* are restored: a file origin also has is left at
// origin's version, which is what keeps the claim a lease (§7) and stops a
// stale copy overwriting a concurrent edit.
//
// Content is read out of the pre-reset commit rather than captured from disk
// beforehand, so only the files actually missing are ever loaded.
func (r *Repo) restoreAdditions(mine, theirs plumbing.Hash) error {
	mineTree, err := r.treeAt(mine)
	if err != nil {
		// A repo with no usable history has nothing to rescue.
		return nil
	}
	theirsTree, err := r.treeAt(theirs)
	if err != nil {
		return fmt.Errorf("reconcile: read origin tree: %w", err)
	}

	return mineTree.Files().ForEach(func(f *object.File) error {
		if _, err := theirsTree.File(f.Name); err == nil {
			return nil // origin has this path; origin wins
		}

		contents, err := f.Contents()
		if err != nil {
			return fmt.Errorf("reconcile: read %s: %w", f.Name, err)
		}
		path := filepath.Join(r.Path, f.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			return fmt.Errorf("reconcile: restore %s: %w", f.Name, err)
		}
		return nil
	})
}

func (r *Repo) treeAt(hash plumbing.Hash) (*object.Tree, error) {
	commit, err := r.repo.CommitObject(hash)
	if err != nil {
		return nil, err
	}
	return commit.Tree()
}

// expandOwned resolves glob patterns against the working tree.
//
// Task shards live at tasks/<id>/progress/<node>.jsonl, so a device cannot
// enumerate what it owns without looking — the set grows every time a task is
// created on any machine in the fleet.
func (r *Repo) expandOwned(owned []string) ([]string, error) {
	var paths []string
	for _, rel := range owned {
		if !strings.ContainsAny(rel, "*?[") {
			paths = append(paths, rel)
			continue
		}

		matches, err := filepath.Glob(filepath.Join(r.Path, rel))
		if err != nil {
			return nil, fmt.Errorf("reconcile: bad owned pattern %q: %w", rel, err)
		}
		for _, abs := range matches {
			match, err := filepath.Rel(r.Path, abs)
			if err != nil {
				return nil, fmt.Errorf("reconcile: %w", err)
			}
			paths = append(paths, match)
		}
	}
	return paths, nil
}

// Refresh pulls without pushing, for read-only commands that should still see
// what other devices published.
func (r *Repo) Refresh(ctx context.Context) error {
	if r.RemoteURL() == "" {
		return nil
	}
	if err := r.Pull(ctx); err != nil {
		if isOffline(err) || isDiverged(err) {
			// Stale local data beats failing a read.
			return nil
		}
		return err
	}
	return nil
}

// isOffline reports whether an error means the remote could not be reached,
// as opposed to the remote rejecting what we sent.
func isOffline(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, transport.ErrRepositoryNotFound) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"no such host", "network is unreachable", "connection refused",
		"i/o timeout", "timeout", "temporary failure in name resolution",
		"tls handshake", "connection reset", "eof",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// isDiverged reports whether the remote rejected a push because history moved.
func isDiverged(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, git.ErrNonFastForwardUpdate) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "cannot lock ref")
}
