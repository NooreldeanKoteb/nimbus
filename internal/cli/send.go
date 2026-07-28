package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/nkoteb/nimbus/internal/bus"
	"github.com/nkoteb/nimbus/internal/device"
	"github.com/nkoteb/nimbus/internal/task"
)

func runSend(ctx context.Context, env *Env, args []string) error {
	to, args := takeArg(args)

	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	taskID := fs.String("task", "", "task this message is about")
	replyTo := fs.String("reply-to", "", "message id this answers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if to == "" {
		return errors.New("usage: nimbus send <device> \"message\"")
	}

	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		text = readPiped(env)
	}

	// Fresh before addressing: a device enrolled since the last sync is not
	// resolvable otherwise, and "no device named X" would be wrong.
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

	kind := bus.KindNote
	if *replyTo != "" {
		kind = bus.KindReply
	}

	m, err := bus.Send(env.Paths.Repo, &bus.Message{
		From: id.ID, To: target, Kind: kind,
		Text: text, Task: *taskID, ReplyTo: *replyTo,
	})
	if err != nil {
		return err
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "bus.send", target, m.Text, nil)
	}

	fleet := env.fleetLabels()
	fmt.Fprintf(env.Out, "sent to %s (%s)\n", fleet.Label(target), m.ID)
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: message %s -> %s", id.ID, target))

	// A message sitting in an unpushed commit has not been sent to anything.
	if repo.RemoteURL() == "" {
		fmt.Fprintln(env.Out, "warning: no origin configured — this cannot reach another device")
	}
	return nil
}

// runDispatch hands a task to another device: release it here, tell them there.
//
// One command because doing it in two invites the state where a task is
// announced but still claimed, which blocks the device being asked to do it.
func runDispatch(ctx context.Context, env *Env, args []string) error {
	to, args := takeArg(args)
	taskRef, args := takeArg(args)

	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	note := fs.String("note", "", "what you want done")
	force := fs.Bool("force", false, "dispatch even if the target lacks required capabilities")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if to == "" || taskRef == "" {
		return errors.New("usage: nimbus dispatch <device> <task-id>")
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
	t, err := task.Load(env.Paths.Repo, taskRef)
	if err != nil {
		return err
	}

	// Routing on capability is the whole point of publishing profiles: sending
	// GPU work to a machine without one wastes a round trip through git and
	// leaves the task stuck on the wrong device.
	fleet, ferr := device.LoadFleet(env.Paths.Repo)
	if ferr == nil && !*force {
		for _, p := range fleet {
			if p.ID != target {
				continue
			}
			if unmet := t.Unmet(p.Capabilities()); len(unmet) > 0 {
				return fmt.Errorf("%s cannot run %s: missing %s\n"+
					"run `nimbus fleet` to find a device that can, or --force",
					p.Label(), t.ID, strings.Join(unmet, ", "))
			}
		}
	}

	// Release before announcing, so the target is not asked to pick up a task
	// this device still holds.
	released := t.Release(id.ID)
	if err := t.Save(env.Paths.Repo); err != nil {
		return err
	}
	if released {
		_ = task.Record(env.Paths.Repo, t.ID, id.ID, task.KindRelease, "dispatched to "+fleet.Label(target))
	}

	text := *note
	if text == "" {
		text = "please pick this up: " + t.Goal
	}
	m, err := bus.Send(env.Paths.Repo, &bus.Message{
		From: id.ID, To: target, Kind: bus.KindDispatch, Text: text, Task: t.ID,
	})
	if err != nil {
		return err
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "bus.dispatch", target, t.ID+": "+text, nil)
	}

	fmt.Fprintf(env.Out, "dispatched %s to %s (%s)\n", t.ID, fleet.Label(target), m.ID)
	fmt.Fprintf(env.Out, "they run: nimbus resume %s\n", t.ID)
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: dispatch %s -> %s", t.ID, target))
	return nil
}
