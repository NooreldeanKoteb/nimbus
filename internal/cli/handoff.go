package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nkoteb/nimbus/internal/task"
)

func runTaskNote(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task note", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	taskID := fs.String("task", "", "task to note against (default: the one held here)")
	blocked := fs.Bool("blocked", false, "record this as a blocker rather than a note")
	if err := fs.Parse(args); err != nil {
		return err
	}

	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		return errors.New("usage: nimbus task note \"what happened\"")
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}
	t, err := env.resolveTask(*taskID, id.ID)
	if err != nil {
		return err
	}

	kind := task.KindNote
	if *blocked {
		kind = task.KindBlocked
	}
	if err := task.Record(env.Paths.Repo, t.ID, id.ID, kind, text); err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "recorded on %s\n", t.ID)
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: note on %s", t.ID))
	return nil
}

func runTaskHandoff(ctx context.Context, env *Env, args []string) error {
	wanted, args := takeArg(args)

	fs := flag.NewFlagSet("task handoff", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	taskID := fs.String("task", "", "task to hand off (default: the one held here)")
	branch := fs.String("branch", "", "branch the work is on (default: the task's branch)")
	release := fs.Bool("release", false, "drop this device's claim so another can pick it up")
	var done, inFlight, next, blocked, outside stringList
	fs.Var(&done, "done", "something finished this session (repeatable)")
	fs.Var(&inFlight, "in-flight", "something left half-finished (repeatable)")
	fs.Var(&next, "next", "the next thing to do (repeatable)")
	fs.Var(&blocked, "blocked", "something blocking progress (repeatable)")
	fs.Var(&outside, "outside-git", "state that git will not carry (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		*taskID = wanted
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}
	t, err := env.resolveTask(*taskID, id.ID)
	if err != nil {
		return err
	}

	h := &task.Handoff{
		Node: id.ID, Host: id.Hostname, Branch: t.Branch,
		Done: done, InFlight: inFlight, Next: next, Blocked: blocked, Outside: outside,
		Notes: readPiped(env),
	}
	if *branch != "" {
		h.Branch = *branch
	}
	if h.Empty() {
		return errors.New("nothing to hand off: pass --done/--next/--blocked or pipe notes on stdin")
	}

	if err := h.Save(env.Paths.Repo, t.ID); err != nil {
		return err
	}
	if err := task.Record(env.Paths.Repo, t.ID, id.ID, task.KindHandoff, summarizeHandoff(h)); err != nil {
		return err
	}

	if *release && t.Release(id.ID) {
		if err := t.Save(env.Paths.Repo); err != nil {
			return err
		}
		_ = task.Record(env.Paths.Repo, t.ID, id.ID, task.KindRelease, "released for another device")
		fmt.Fprintf(env.Out, "handoff saved and %s released\n", t.ID)
	} else {
		fmt.Fprintf(env.Out, "handoff saved for %s\n", t.ID)
	}

	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "task.handoff", t.ID, summarizeHandoff(h), nil)
	}

	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: handoff on %s", t.ID))
	fmt.Fprintf(env.Out, "\nresume on another device with: nimbus resume %s\n", t.ID)
	return nil
}

func runTaskDone(ctx context.Context, env *Env, args []string) error {
	return finishTask(ctx, env, args, "done")
}

func runTaskRelease(ctx context.Context, env *Env, args []string) error {
	return finishTask(ctx, env, args, "release")
}

// finishTask handles the two ways a device stops working a task. They differ
// only in whether the task itself is closed or merely let go.
func finishTask(ctx context.Context, env *Env, args []string, mode string) error {
	wanted, args := takeArg(args)

	fs := flag.NewFlagSet("task "+mode, flag.ContinueOnError)
	fs.SetOutput(env.Err)
	taskID := fs.String("task", "", "task to act on (default: the one held here)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		*taskID = wanted
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}
	t, err := env.resolveTask(*taskID, id.ID)
	if err != nil {
		return err
	}

	kind, verb := task.KindRelease, "released"
	if mode == "done" {
		t.Status = task.StatusDone
		kind, verb = task.KindDone, "marked done"
	}
	t.Release(id.ID)

	if err := t.Save(env.Paths.Repo); err != nil {
		return err
	}
	if err := task.Record(env.Paths.Repo, t.ID, id.ID, kind, verb); err != nil {
		return err
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "task."+mode, t.ID, verb, nil)
	}

	fmt.Fprintf(env.Out, "%s %s\n", t.ID, verb)
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: %s %s", verb, t.ID))
	return nil
}

// readPiped returns stdin when it is a pipe or file, and "" when it is a
// terminal. Without the check, a bare `nimbus task handoff --done x` would hang
// waiting for input the user never intended to give.
func readPiped(env *Env) string {
	in := env.In
	if in == nil {
		in = os.Stdin
	}
	if f, ok := in.(*os.File); ok {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice != 0 {
			return ""
		}
	}

	data, err := io.ReadAll(io.LimitReader(in, 256*1024))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func summarizeHandoff(h *task.Handoff) string {
	var parts []string
	for _, s := range []struct {
		label string
		items []string
	}{
		{"done", h.Done}, {"in flight", h.InFlight},
		{"next", h.Next}, {"blocked", h.Blocked},
	} {
		if len(s.items) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", len(s.items), s.label))
		}
	}
	if len(parts) == 0 {
		return "notes only"
	}
	return strings.Join(parts, ", ")
}
