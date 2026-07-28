package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/nkoteb/nimbus/internal/bus"
	"github.com/nkoteb/nimbus/internal/device"
	"github.com/nkoteb/nimbus/internal/install"
	"github.com/nkoteb/nimbus/internal/task"
)

// runResume picks up a task on this device: claim it, then print everything the
// previous device left behind. This is the command the whole state repo exists
// to make possible.
func runResume(ctx context.Context, env *Env, args []string) error {
	wanted, args := takeArg(args)

	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	brief := fs.Bool("brief", false, "print the context without claiming the task")
	force := fs.Bool("force", false, "claim even if another device holds it, or requirements are unmet")
	execClaude := fs.Bool("exec", false, "launch Claude Code with this context")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Resuming against a stale repo would show the previous device's work as it
	// looked before it finished, which is worse than showing nothing.
	autoRefresh(ctx, env)

	id, err := env.identity()
	if err != nil {
		return err
	}
	t, err := env.resolveTask(wanted, id.ID)
	if err != nil {
		return err
	}

	profile := device.Detect(ctx, id, nil)

	if *brief {
		fmt.Fprint(env.Out, task.Brief(env.Paths.Repo, profile, t, 0))
		return nil
	}

	if unmet := t.Unmet(profile.Capabilities()); len(unmet) > 0 && !*force {
		return fmt.Errorf("%s needs %s, which this device does not have\n"+
			"run `nimbus fleet` to find a device that does, or --force to work here anyway",
			t.ID, strings.Join(unmet, ", "))
	}
	if t.Claim != nil && t.Claim.Node != id.ID && !*force {
		return fmt.Errorf("%s is held by %s since %s\n"+
			"have that device run `nimbus task release`, or --force to take it",
			t.ID, env.fleetLabels().Label(t.Claim.Node), t.Claim.At.Format("2006-01-02 15:04"))
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}

	previous := ""
	if t.Claim != nil {
		previous = t.Claim.Node
	}
	t.Take(id.ID, id.Hostname)
	if err := t.Save(env.Paths.Repo); err != nil {
		return err
	}
	if err := task.Record(env.Paths.Repo, t.ID, id.ID, task.KindClaim, "resumed on "+id.Label()); err != nil {
		return err
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "task.resume", t.ID, "claimed from "+orNobody(env.fleetLabels().Label(previous)), nil)
	}

	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: resume %s on %s", t.ID, id.ID))

	// task.json is shared, so a reconcile during that sync takes origin's copy
	// and our claim can lose the race. Reload to report what actually holds.
	if current, lerr := task.Load(env.Paths.Repo, t.ID); lerr == nil && !current.HeldBy(id.ID) {
		fmt.Fprintf(env.Out, "\n! %s was claimed by %s first — this device does not hold it\n",
			t.ID, orNobody(env.fleetLabels().Label(claimNode(current))))
		fmt.Fprintln(env.Out, "  your notes are safe; re-run with --force to take it anyway")
		return nil
	}

	context := task.Brief(env.Paths.Repo, profile, t, 0)
	fmt.Fprint(env.Out, "\n"+context)

	// Bind whether or not we launch, so the session id is stable for anyone who
	// prefers to start claude themselves.
	binding, err := task.BindSession(env.Paths.Repo, t.ID, id.ID, id.Hostname)
	if err != nil {
		return err
	}
	// Push the binding before the session starts — a crash mid-session must not
	// lose which conversation this was.
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: session for %s on %s", t.ID, id.ID))

	if *execClaude {
		return launchClaude(ctx, env, binding, context)
	}

	if binding.Resume {
		fmt.Fprintf(env.Out, "\ncontinue the conversation: claude --resume %s\n", binding.SessionID)
	} else {
		fmt.Fprintf(env.Out, "\nstart the session:         claude --session-id %s\n", binding.SessionID)
		fmt.Fprintf(env.Out, "  (%s)\n", binding.Reason)
	}
	fmt.Fprintln(env.Out, "or let nimbus do it:       nimbus resume "+t.ID+" --exec")
	return nil
}

// runContext prints the session brief for the SessionStart hook.
//
// It never fails. A hook that exits non-zero turns every Claude session on the
// device into an error report, so a missing repo or unreadable task degrades to
// printing less rather than printing a failure.
func runContext(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	noRefresh := fs.Bool("no-refresh", false, "skip pulling the state repo first")
	if err := fs.Parse(args); err != nil {
		return err
	}

	id, err := env.identity()
	if err != nil {
		return nil
	}
	if !*noRefresh {
		autoRefresh(ctx, env)
	}

	// A nil task is the normal case on a device with nothing claimed; Brief
	// still reports the hardware and tooling, which is the half Claude most
	// often gets wrong on its own.
	t, _ := task.Active(env.Paths.Repo, id.ID)
	fmt.Fprint(env.Out, task.Brief(env.Paths.Repo, device.Detect(ctx, id, nil), t, 0))
	printPendingMessages(env, id.ID)
	return nil
}

// printPendingMessages appends unread mail to the session brief.
//
// A message that only shows up when someone runs `nimbus inbox` is a message
// nobody reads. Putting it in the SessionStart payload means the other device's
// request is the first thing Claude sees.
func printPendingMessages(env *Env, nodeID string) {
	messages, err := bus.Inbox(env.Paths.Repo, nodeID, false)
	if err != nil || len(messages) == 0 {
		return
	}

	fleet := env.fleetLabels()
	fmt.Fprintf(env.Out, "\n## Messages (%d unread)\n\n", len(messages))
	for _, e := range messages {
		fmt.Fprintf(env.Out, "- **from %s** (%s): %s\n",
			fleet.Label(e.Message.From), e.Message.Kind, e.Message.Text)
		if e.Message.Task != "" {
			fmt.Fprintf(env.Out, "  Task `%s` — run `nimbus resume %s` to pick it up.\n",
				e.Message.Task, e.Message.Task)
		}
		fmt.Fprintf(env.Out, "  Acknowledge with `nimbus ack %s --note \"...\"`.\n", e.Message.ID)
	}
}

// launchClaude starts or resumes the Claude Code session bound to this task on
// this device.
//
// Resuming replays the real conversation, so the brief is not passed again —
// that history is already in the session, and re-injecting it would waste
// context restating what Claude just read.
//
// A fresh session gets the brief as its opening prompt rather than relying on
// the SessionStart hook, so --exec works on a device where the hook was never
// installed.
func launchClaude(ctx context.Context, env *Env, binding *task.Resumption, brief string) error {
	bin := install.ClaudeCodePath()
	if bin == "" {
		return errors.New("claude is not installed on this device (run `nimbus init`)")
	}

	var args []string
	if binding.Resume {
		fmt.Fprintf(env.Out, "\nresuming session %s\n", binding.SessionID)
		args = []string{"--resume", binding.SessionID}
	} else {
		fmt.Fprintf(env.Out, "\nnew session %s (%s)\n", binding.SessionID, binding.Reason)
		args = []string{"--session-id", binding.SessionID, brief}
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, env.Out, env.Err
	return cmd.Run()
}

func claimNode(t *task.Task) string {
	if t.Claim == nil {
		return ""
	}
	return t.Claim.Node
}

func orNobody(node string) string {
	if node == "" {
		return "nobody"
	}
	return node
}
