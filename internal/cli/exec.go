package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/nkoteb/nimbus/internal/autonomy"
	"github.com/nkoteb/nimbus/internal/bus"
)

// pollInterval is how often `--wait` looks for new output.
//
// Shorter than the daemon's sync interval on purpose: the far device pushes on
// its own schedule, and polling faster than it publishes costs one cheap fetch
// but shortens the gap between a command finishing there and being visible here.
const pollInterval = 10 * time.Second

// waitTimeout bounds `--wait`. Reaching it does not cancel anything on the far
// device — the command keeps running and the result still lands on the bus.
// It only stops this terminal from blocking forever on a peer that went dark.
const waitTimeout = 30 * time.Minute

// runExec asks another device to run one command.
//
// This is the narrow, auditable half of unattended peer work (DESIGN.md §6d):
// a named command, checked against the *recipient's* allowlist, with the output
// streamed back through the same git the rest of the mesh runs on. The sender
// gets no say in what is permitted there, which is the entire point — a device
// decides for itself what it will do for others.
func runExec(ctx context.Context, env *Env, args []string) error {
	// Device then quoted command, then flags — the same shape as
	// `nimbus system apply "<cmd>" --rollback "<undo>"`, so a command carrying
	// its own flags survives being passed through.
	to, args := takeArg(args)
	command, args := takeArg(args)

	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	follow := fs.String("follow", "", "re-attach to an earlier exec by message id")
	wait := fs.Bool("wait", false, "stream the output and wait for the result")
	timeout := fs.Duration("timeout", waitTimeout, "how long to wait before detaching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if command == "" {
		command = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}

	if *follow != "" {
		autoRefresh(ctx, env)
		return followExec(ctx, env, *follow, *timeout)
	}
	if to == "" || command == "" {
		return errors.New(`usage: nimbus exec <device> "<command>" [--wait]`)
	}

	autoRefresh(ctx, env)

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}
	target, err := env.resolveNode(to)
	if err != nil {
		return err
	}

	// Asking is a dispatch (L2). Doing is peer.exec (L3) and is decided on the
	// far device, which is why this check is about sending, not about running.
	decision := env.guard(id.ID, nil).Allow(autonomy.Action{
		Kind: autonomy.ActDispatch, Target: target, Detail: command,
	})
	if !decision.Allowed {
		return errors.New(decision.Reason)
	}

	m, err := bus.Send(env.Paths.Repo, &bus.Message{
		From: id.ID, To: target, Kind: bus.KindExec,
		Command: command, Text: "run this and report back",
	})
	if err != nil {
		return err
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "peer.exec.request", target, command, nil)
	}

	fleet := env.fleetLabels()
	fmt.Fprintf(env.Out, "asked %s to run %q (%s)\n", fleet.Label(target), command, m.ID)
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: exec request %s -> %s", m.ID, target))

	if repo.RemoteURL() == "" {
		fmt.Fprintln(env.Out, "warning: no origin configured — this cannot reach another device")
		return nil
	}
	if !*wait {
		fmt.Fprintf(env.Out, "follow it with `nimbus exec --follow %s`\n", m.ID)
		return nil
	}
	return followExec(ctx, env, m.ID, *timeout)
}

// followExec streams a command's output as it arrives and reports the result.
//
// Output arrives in whole appended chunks rather than as a stream of bytes, so
// this prints whatever is new since the last poll. That makes the display
// granularity the far device's sync interval, which is the honest resolution of
// a git transport and is still enough to watch a long build make progress.
func followExec(ctx context.Context, env *Env, id string, timeout time.Duration) error {
	m, err := bus.Load(env.Paths.Repo, id)
	if err != nil {
		return err
	}

	// A zero timeout is "tell me where this is right now" rather than a wait
	// that expires instantly. That is the only shape an MCP tool can use: a
	// tool call that blocks for half an hour is a session that has stopped.
	if timeout <= 0 {
		return checkExec(env, id)
	}

	fleet := env.fleetLabels()
	fmt.Fprintf(env.Out, "\nwaiting on %s — ctrl-c detaches, the command keeps running\n\n",
		fleet.Label(m.To))

	deadline := time.Now().Add(timeout)
	shown := 0

	for {
		if out, err := bus.Output(env.Paths.Repo, id); err == nil && len(out) > shown {
			fmt.Fprint(env.Out, out[shown:])
			shown = len(out)
		}

		answer, err := bus.Answer(env.Paths.Repo, id)
		if err == nil && answer != nil {
			return reportResult(env, answer)
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(env.Out, "\ndetaching after %s — re-attach with `nimbus exec --follow %s`\n",
				timeout, id)
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollInterval):
		}
		autoRefresh(ctx, env)
	}
}

// checkExec reports where a command has got to without waiting for it.
func checkExec(env *Env, id string) error {
	if out, err := bus.Output(env.Paths.Repo, id); err == nil && out != "" {
		fmt.Fprint(env.Out, out)
	}
	answer, err := bus.Answer(env.Paths.Repo, id)
	if err != nil {
		return err
	}
	if answer == nil {
		fmt.Fprintf(env.Out, "\nstill running — no result yet for %s\n", id)
		return nil
	}
	return reportResult(env, answer)
}

func reportResult(env *Env, answer *bus.Message) error {
	fmt.Fprintln(env.Out)
	switch {
	case answer.Exit == bus.ExitRefused:
		// A refusal is not a failed command, and conflating them would send
		// somebody debugging a machine that did exactly what it was told.
		fmt.Fprintf(env.Out, "refused: %s\n", answer.Text)
	case answer.Exit == 0:
		fmt.Fprintln(env.Out, "done (exit 0)")
	default:
		fmt.Fprintf(env.Out, "failed (exit %d)\n", answer.Exit)
	}
	if answer.Text != "" && answer.Exit != bus.ExitRefused {
		fmt.Fprintf(env.Out, "%s\n", answer.Text)
	}
	return nil
}
