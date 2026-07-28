package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/nkoteb/nimbus/internal/autonomy"
	"github.com/nkoteb/nimbus/internal/bus"
	"github.com/nkoteb/nimbus/internal/device"
	"github.com/nkoteb/nimbus/internal/install"
	"github.com/nkoteb/nimbus/internal/system"
	"github.com/nkoteb/nimbus/internal/task"
)

// execTimeout bounds one peer-run command. Generous, because a peer is often
// asked to build or install something; bounded, because an unattended device
// holding a wedged shell forever is a device nobody can reach.
const execTimeout = 15 * time.Minute

// sessionTimeout bounds a dispatched Claude session on this device.
const sessionTimeout = 30 * time.Minute

// resultTail is how much of a command's output travels back in the result
// message. The full transcript is in the streamed log; this is the part that
// shows up without following anything, so it is the tail rather than the head —
// what a command says as it dies is what explains why.
const resultTail = 4 << 10

// errNoClaude is returned when this device cannot start a session at all.
var errNoClaude = errors.New("claude is not installed on this device")

// runWork does the work other devices have asked this one for.
//
// This is the receiving half of DESIGN.md §6d, and the thing that turns a
// message bus into a mesh of agents: until something drains the inbox and acts,
// a dispatch is a note nobody read.
//
// Two kinds of work, deliberately unequal:
//
//   - An exec request runs one command, and only if that command is on *this*
//     device's allowlist (invariant 3). Narrow, auditable, and the sender has
//     no say in what is permitted.
//   - A dispatched task starts a Claude session, which is not bounded by any
//     allowlist. That is a much larger grant, so it needs --act on top of L3
//     rather than following from the level alone.
func runWork(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("work", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	act := fs.Bool("act", false, "also start Claude sessions for dispatched tasks")
	noSync := fs.Bool("no-sync", false, "do not sync; the caller owns that (used by the daemon)")
	dryRun := fs.Bool("dry-run", false, "show what would run and whether it is permitted")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Work arriving over the bus is never attended, whoever started the drain:
	// the person here did not choose the command, the sender did. Treating a
	// human running `nimbus work` as attended would let anyone on the bus
	// borrow that presence to bypass the ladder.
	env.Attended = false

	if !*noSync {
		autoRefresh(ctx, env)
	}
	id, err := env.identity()
	if err != nil {
		return err
	}

	pending, err := bus.Inbox(env.Paths.Repo, id.ID, false)
	if err != nil {
		return err
	}

	handled := 0
	for _, e := range pending {
		m := e.Message
		switch m.Kind {
		case bus.KindExec:
			if *dryRun {
				reportDryRun(env, id.ID, m)
				continue
			}
			workExec(ctx, env, id.ID, m)
			handled++
		case bus.KindDispatch:
			if !*act || *dryRun {
				continue
			}
			workDispatch(ctx, env, id.ID, m)
			handled++
		}
	}

	if handled == 0 {
		if !*noSync {
			fmt.Fprintln(env.Out, "nothing to do")
		}
		return nil
	}
	if !*noSync {
		if repo, rerr := env.stateRepo(); rerr == nil {
			autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: %s did %d peer job(s)", id.ID, handled))
		}
	}
	return nil
}

func reportDryRun(env *Env, nodeID string, m *bus.Message) {
	d := execDecision(env, nodeID, m)
	verdict := "would run"
	if !d.Allowed {
		verdict = d.Reason
	}
	fmt.Fprintf(env.Out, "%s  %-40s %s\n", m.ID, truncate(m.Command, 40), verdict)
}

// execDecision is the single place an incoming command is judged, so the
// dry run and the real thing can never disagree about what is permitted.
func execDecision(env *Env, nodeID string, m *bus.Message) autonomy.Decision {
	return env.guard(nodeID, nil).Allow(autonomy.Action{
		Kind:     autonomy.ActExec,
		Target:   m.From,
		Command:  m.Command,
		FromPeer: true,
		Detail:   "requested by " + m.From,
	})
}

// workExec runs one command a peer asked for and streams the output back.
func workExec(ctx context.Context, env *Env, nodeID string, m *bus.Message) {
	if d := execDecision(env, nodeID, m); !d.Allowed {
		// A refusal is reported, not swallowed. A sender that hears nothing
		// cannot tell "refused" from "still running", and would keep waiting
		// on a machine that already decided.
		answer(env, nodeID, m, bus.ExitRefused, d.Reason)
		if log, lerr := env.auditLog(nodeID); lerr == nil {
			log.Record("peer", "peer.exec.refused", m.From, m.Command+": "+d.Reason, nil)
		}
		// Said here as well as sent back. Somebody watching this device drain
		// its queue needs to see what it turned down — a refusal that is only
		// visible on the far machine looks from here like nothing happened.
		fmt.Fprintf(env.Out, "refused %q from %s: %s\n", m.Command, m.From, d.Reason)
		return
	}

	// Acknowledged *before* running, so a device that dies mid-command does not
	// run it again on the next drain. At-most-once is the right guarantee for
	// something with side effects: a command that never ran is visible as a
	// missing result, but a package installed twice is not visible at all.
	if _, err := bus.Ack(env.Paths.Repo, nodeID, m.ID, "running"); err != nil {
		return
	}
	if log, lerr := env.auditLog(nodeID); lerr == nil {
		log.Record("peer", "peer.exec.start", m.From, m.Command, nil)
	}

	runCtx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	sink := &streamSink{repoPath: env.Paths.Repo, nodeID: nodeID, id: m.ID}
	exit, runErr := system.Stream(runCtx, m.Command, sink)

	text := strings.TrimSpace(sink.tail())
	if runErr != nil && exit == -1 {
		// Never started, or killed by the timeout. Say which; "exit -1" alone
		// tells the far device nothing it can act on.
		text = strings.TrimSpace(runErr.Error() + "\n" + text)
		exit = bus.ExitRefused
	}
	answer(env, nodeID, m, exit, text)

	if log, lerr := env.auditLog(nodeID); lerr == nil {
		log.Record("peer", "peer.exec.done", m.From,
			fmt.Sprintf("%s (exit %d)", m.Command, exit), runErr)
	}
	fmt.Fprintf(env.Out, "ran %q for %s (exit %d)\n", m.Command, m.From, exit)
}

// answer sends the outcome back and closes out the request.
func answer(env *Env, nodeID string, m *bus.Message, exit int, text string) {
	if _, err := bus.Ack(env.Paths.Repo, nodeID, m.ID, fmt.Sprintf("exit %d", exit)); err != nil {
		// Already acknowledged before the run in the allowed path; only the
		// refusal path needs this, and a failure here must not lose the result.
		_ = err
	}
	if text == "" {
		text = "(no output)"
	}
	_, _ = bus.Send(env.Paths.Repo, &bus.Message{
		From: nodeID, To: m.From, Kind: bus.KindResult,
		ReplyTo: m.ID, Command: m.Command, Exit: exit, Text: text,
	})
}

// streamSink writes a command's output into the message's log as it is produced
// and keeps the tail in memory for the result message.
//
// Appending straight to the file rather than buffering is what makes the output
// visible elsewhere while the command is still running: the daemon's next sync
// pushes whatever has landed, with no coordination between the two.
type streamSink struct {
	repoPath, nodeID, id string
	kept                 []byte
}

func (s *streamSink) Write(p []byte) (int, error) {
	if err := bus.AppendOutput(s.repoPath, s.nodeID, s.id, p); err != nil {
		return 0, err
	}
	s.kept = append(s.kept, p...)
	if len(s.kept) > resultTail {
		s.kept = s.kept[len(s.kept)-resultTail:]
	}
	return len(p), nil
}

func (s *streamSink) tail() string { return string(s.kept) }

// workDispatch picks up a task another device handed over and works it.
func workDispatch(ctx context.Context, env *Env, nodeID string, m *bus.Message) {
	if m.Task == "" {
		return
	}
	t, err := task.Load(env.Paths.Repo, m.Task)
	if err != nil {
		answer(env, nodeID, m, bus.ExitRefused, "no such task here: "+m.Task)
		return
	}

	// Starting a session is peer.exec territory — it is the far end deciding
	// what runs here — even though no single command is named.
	d := env.guard(nodeID, t).Allow(autonomy.Action{
		Kind: autonomy.ActExec, Target: t.ID, Detail: "dispatched session",
	})
	if !d.Allowed {
		answer(env, nodeID, m, bus.ExitRefused, d.Reason)
		return
	}

	id, err := env.identity()
	if err != nil {
		return
	}
	profile := device.Detect(ctx, id, nil)
	if unmet := t.Unmet(profile.Capabilities()); len(unmet) > 0 {
		answer(env, nodeID, m, bus.ExitRefused,
			"this device is missing "+strings.Join(unmet, ", "))
		return
	}

	if _, err := bus.Ack(env.Paths.Repo, nodeID, m.ID, "picked up"); err != nil {
		return
	}
	t.Take(nodeID, profile.Hostname)
	if err := t.Save(env.Paths.Repo); err != nil {
		return
	}
	_ = task.Record(env.Paths.Repo, t.ID, nodeID, task.KindClaim,
		"picked up unattended from "+m.From)

	brief := task.Brief(env.Paths.Repo, profile, t, 0) + dispatchSection(m)
	err = runSession(ctx, env, nodeID, t, brief, sessionTimeout)

	switch {
	case errors.Is(err, errNoClaude):
		_ = task.Record(env.Paths.Repo, t.ID, nodeID, task.KindBlocked,
			"cannot work this here: claude is not installed")
		answer(env, nodeID, m, bus.ExitRefused, "claude is not installed on this device")
	case err != nil:
		answer(env, nodeID, m, 1, "session failed: "+err.Error())
	default:
		answer(env, nodeID, m, 0, "session finished; see `nimbus task show "+t.ID+"`")
	}
}

// dispatchSection tells the session it is working somebody else's request, and
// how to answer them. Without it the session has the task but no idea a person
// on another machine is waiting on a reply.
func dispatchSection(m *bus.Message) string {
	var b strings.Builder
	b.WriteString("\n## Dispatched to this device\n\n")
	fmt.Fprintf(&b, "Device `%s` handed you this task and said: %q\n\n", m.From, m.Text)
	b.WriteString("Nobody is watching this run. Record what you find with " +
		"`nimbus task note`, and make system changes only through " +
		"`nimbus system apply \"<cmd>\" --rollback \"<undo>\"` so they can be " +
		"undone by someone who did not see this happen.\n")
	return b.String()
}

// runSession starts a headless Claude session for a task and records the
// outcome on the task's timeline.
//
// Shared by boot resume and peer dispatch so the two cannot drift on what an
// unattended session looks like — the audit trail it leaves is the only record
// that anything happened.
func runSession(ctx context.Context, env *Env, nodeID string, t *task.Task, brief string, timeout time.Duration) error {
	bin := install.ClaudeCodePath()
	if bin == "" {
		return errNoClaude
	}

	binding, err := task.BindSession(env.Paths.Repo, t.ID, nodeID, "")
	if err != nil {
		return err
	}
	_ = task.Record(env.Paths.Repo, t.ID, nodeID, task.KindBoot,
		"unattended session started ("+binding.SessionID+")")
	if repo, rerr := env.stateRepo(); rerr == nil {
		// Pushed before the session starts: if it wedges, the fleet still knows
		// this device took the work.
		autoSync(ctx, env, repo, nodeID, "nimbus: unattended session for "+t.ID)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Headless, because there is no terminal here. The brief goes in as the
	// prompt rather than through the SessionStart hook, so this works on a
	// device where the hook was never installed.
	cmd := exec.CommandContext(runCtx, bin, "-p", brief)
	cmd.Stdout, cmd.Stderr = env.Out, env.Err
	runErr := cmd.Run()

	kind, detail := task.KindNote, "unattended session finished"
	if runErr != nil {
		kind, detail = task.KindBlocked, "unattended session failed: "+runErr.Error()
	}
	_ = task.Record(env.Paths.Repo, t.ID, nodeID, kind, detail)
	if log, lerr := env.auditLog(nodeID); lerr == nil {
		log.Record("local", "session.unattended", t.ID, detail, runErr)
	}
	if repo, rerr := env.stateRepo(); rerr == nil {
		autoSync(ctx, env, repo, nodeID, "nimbus: session done for "+t.ID)
	}
	return runErr
}
