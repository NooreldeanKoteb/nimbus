package install

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// AdoptClaudeConfig copies existing local Claude config into the state repo.
//
// This is the first-device case, and getting it wrong is destructive: a user
// who already has a working ~/.claude must have it captured, not overwritten
// by an empty repo. Entries already present in the repo are left alone, so the
// second device adopts nothing and links everything.
func AdoptClaudeConfig(localDir, repoDir string, entries []string) []Step {
	steps := make([]Step, 0, len(entries))

	for _, name := range entries {
		step := Step{Name: "adopt:" + name}
		src := filepath.Join(localDir, name)
		dst := filepath.Join(repoDir, name)

		// A symlink here means this device is already linked to the repo;
		// following it would copy the repo onto itself.
		info, err := os.Lstat(src)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			step.Skipped = true
			step.Detail = "nothing local to adopt"
			steps = append(steps, step)
			continue
		}
		if _, err := os.Stat(dst); err == nil {
			step.Skipped = true
			step.Detail = "already in state repo"
			steps = append(steps, step)
			continue
		}

		if err := copyPath(src, dst); err != nil {
			step.Err = err
		} else {
			step.Changed = true
			step.Detail = "copied into state repo"
		}
		steps = append(steps, step)
	}
	return steps
}

// LinkClaudeConfig points local ~/.claude entries at the state repo.
//
// Existing real files are backed up rather than deleted; losing a user's
// hand-written CLAUDE.md to an automated setup step would be unforgivable.
func LinkClaudeConfig(repoDir, localDir string, entries []string) []Step {
	steps := make([]Step, 0, len(entries))

	for _, name := range entries {
		step := Step{Name: "link:" + name}
		src := filepath.Join(repoDir, name)
		dst := filepath.Join(localDir, name)

		if _, err := os.Stat(src); err != nil {
			step.Skipped = true
			step.Detail = "not present in state repo"
			steps = append(steps, step)
			continue
		}

		if existing, err := os.Readlink(dst); err == nil && existing == src {
			step.Skipped = true
			step.Detail = "already linked"
			steps = append(steps, step)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			step.Err = err
			steps = append(steps, step)
			continue
		}

		if info, err := os.Lstat(dst); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				// A stale symlink is safe to replace outright.
				if err := os.Remove(dst); err != nil {
					step.Err = err
					steps = append(steps, step)
					continue
				}
			} else {
				backup := fmt.Sprintf("%s.nimbus-backup-%s", dst, time.Now().UTC().Format("20060102-150405"))
				if err := os.Rename(dst, backup); err != nil {
					step.Err = fmt.Errorf("back up existing %s: %w", name, err)
					steps = append(steps, step)
					continue
				}
				step.Detail = "backed up to " + filepath.Base(backup) + "; "
			}
		}

		if err := os.Symlink(src, dst); err != nil {
			step.Err = err
		} else {
			step.Changed = true
			step.Detail += "linked -> " + src
		}
		steps = append(steps, step)
	}
	return steps
}

// copyPath copies a file or directory tree.
func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(src, dst, info)
	}
	return copyFile(src, dst, info)
}

func copyDir(src, dst string, info os.FileInfo) error {
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, info os.FileInfo) error {
	// Symlinks inside a config tree are copied as links, not dereferenced,
	// so an adopted tree keeps its original shape.
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		os.Remove(dst)
		return os.Symlink(target, dst)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
