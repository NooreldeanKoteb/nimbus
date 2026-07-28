package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// maxOutput bounds what is kept from a command. Enough to see what happened,
// not so much that one noisy install bloats the journal for every device that
// syncs it.
const maxOutput = 8 << 10

// DefaultTimeout stops a change hanging forever. A package install can be slow
// on a bad connection, so this is generous rather than tight.
const DefaultTimeout = 30 * time.Minute

// Runner executes a command and returns its combined output. Injected so the
// journal can be tested without modifying the machine running the tests.
type Runner func(ctx context.Context, command string) (string, error)

// Shell runs a command through the platform's shell.
func Shell(ctx context.Context, command string) (string, error) {
	name, args := "sh", []string{"-c", command}
	if runtime.GOOS == "windows" {
		name, args = "cmd", []string{"/c", command}
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// Stream runs a command through the platform's shell, writing its combined
// output to w as it is produced rather than at the end, and returns the exit
// status.
//
// The difference from Shell matters for peer execution: a build that takes ten
// minutes has to be watchable from another machine while it runs, which means
// the output has to leave this process before the command finishes.
//
// The exit status is returned rather than the error alone because a peer needs
// to distinguish "ran and failed" from "never ran"; err is non-nil in both
// cases and cannot carry that distinction on its own.
func Stream(ctx context.Context, command string, w io.Writer) (int, error) {
	name, args := "sh", []string{"-c", command}
	if runtime.GOOS == "windows" {
		name, args = "cmd", []string{"/c", command}
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), err
	default:
		// The shell never started: a missing interpreter, a cancelled context.
		// That is not an exit status the command chose, so it must not look
		// like one.
		return -1, err
	}
}

// Apply journals a change, runs it, and journals the outcome.
//
// The first record is written and flushed to disk *before* the command runs, so
// a change that takes the machine down leaves behind both the fact that it was
// attempted and the command to undo it. A run that dies mid-flight is therefore
// visible as a change still marked "planned", which is precisely the state
// someone recovering the machine needs to see.
func Apply(ctx context.Context, repoPath, nodeID string, c Change, run Runner) (*Change, error) {
	if strings.TrimSpace(c.Command) == "" {
		return nil, errors.New("a change needs a command")
	}
	if c.Rollback == "" && !c.Irreversible {
		return nil, errors.New("a change needs a rollback command (or must be marked irreversible)")
	}
	if c.Irreversible && strings.TrimSpace(c.Reason) == "" {
		return nil, errors.New("an irreversible change needs a reason")
	}
	if c.ID == "" {
		c.ID = NewID()
	}
	if run == nil {
		run = Shell
	}

	c.Status = StatusPlanned
	c.At = time.Now().UTC()
	if err := Append(repoPath, nodeID, c); err != nil {
		// Refusing to run because the record could not be written is the
		// invariant doing its job, not a failure to be worked around.
		return nil, fmt.Errorf("not applying %q: %w", c.Command, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	output, err := run(runCtx, c.Command)
	c.At = time.Now().UTC()
	c.Output = clip(output)
	if err != nil {
		c.Status = StatusFailed
		c.Err = err.Error()
	} else {
		c.Status = StatusApplied
		c.Err = ""
	}

	if jerr := Append(repoPath, nodeID, c); jerr != nil {
		return &c, jerr
	}
	return &c, err
}

// Rollback runs a change's recorded undo command.
func Rollback(ctx context.Context, repoPath, nodeID, id string, run Runner) (*Change, error) {
	c, err := Load(repoPath, id)
	if err != nil {
		return nil, err
	}
	if c.Irreversible || c.Rollback == "" {
		return nil, fmt.Errorf("%s is irreversible: %s", c.ID, orUnstated(c.Reason))
	}
	if c.Status == StatusRolledBack {
		return c, fmt.Errorf("%s was already rolled back", c.ID)
	}
	if c.Node != nodeID {
		// The command undoes a change to a different machine's filesystem,
		// services, or packages. Running it here would at best do nothing.
		return nil, fmt.Errorf("%s was applied on another device; roll it back there", c.ID)
	}
	if run == nil {
		run = Shell
	}

	runCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	output, rerr := run(runCtx, c.Rollback)
	undone := *c
	undone.At = time.Now().UTC()
	undone.Output = clip(output)
	// The rolled-back record keeps the original command so the journal still
	// reads as a history of what was done, not only of what was undone.
	if rerr != nil {
		undone.Status = StatusFailed
		undone.Err = "rollback failed: " + rerr.Error()
	} else {
		undone.Status = StatusRolledBack
		undone.Err = ""
	}

	if jerr := Append(repoPath, nodeID, undone); jerr != nil {
		return &undone, jerr
	}
	return &undone, rerr
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "\n… output truncated"
}

func orUnstated(reason string) string {
	if reason == "" {
		return "no rollback was recorded"
	}
	return reason
}
