package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/nkoteb/nimbus/internal/audit"
	"github.com/nkoteb/nimbus/internal/autonomy"
	"github.com/nkoteb/nimbus/internal/system"
	"github.com/nkoteb/nimbus/internal/task"
)

func runSystem(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nimbus system <list|show|apply|rollback> [args]")
	}
	switch args[0] {
	case "list":
		return runSystemList(ctx, env, args[1:])
	case "show":
		return runSystemShow(ctx, env, args[1:])
	case "apply":
		return runSystemApply(ctx, env, args[1:])
	case "rollback":
		return runSystemRollback(ctx, env, args[1:])
	default:
		return fmt.Errorf("unknown system subcommand %q", args[0])
	}
}

// runSystemApply is the enforcement point for invariant 4: a change made
// through here is journaled with its rollback before it runs, and a change made
// any other way is not a change nimbus can undo for you.
func runSystemApply(ctx context.Context, env *Env, args []string) error {
	command, args := takeArg(args)

	fs := flag.NewFlagSet("system apply", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	rollback := fs.String("rollback", "", "the command that undoes this one")
	kind := fs.String("kind", "command", "package, service, config, or command")
	target := fs.String("target", "", "what is being changed")
	irreversible := fs.Bool("irreversible", false, "this change genuinely cannot be undone")
	reason := fs.String("reason", "", "why it cannot be undone (required with --irreversible)")
	dryRun := fs.Bool("dry-run", false, "show what would run and whether it is permitted")
	confirm := fs.Bool("confirm", false, "confirm a command the task's contract holds for a person")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if command == "" {
		command = strings.Join(fs.Args(), " ")
	}
	if strings.TrimSpace(command) == "" {
		return errors.New(`usage: nimbus system apply "<command>" --rollback "<undo command>"`)
	}

	id, err := env.identity()
	if err != nil {
		return err
	}
	repo, err := env.stateRepo()
	if err != nil {
		return err
	}

	// The active task's contract can block a command outright, at any level.
	// Checked before the ladder because "this task said never do that" is a
	// stronger statement than "this device may not do that unattended".
	//
	// A person typing the command is not by itself the confirmation: the whole
	// point of naming a pattern in require_human is that someone has to say
	// "yes, this one" about that specific command. So it takes --confirm, and
	// unattended it cannot be satisfied at all.
	active, _ := task.Active(env.Paths.Repo, id.ID)
	if pattern, blocked := active.Contract().NeedsHuman(command); blocked && !(env.Attended && *confirm) {
		if !env.Attended {
			return fmt.Errorf("%q matches require_human pattern %q on task %s — "+
				"a person has to run this one", command, pattern, active.ID)
		}
		return fmt.Errorf("%q matches require_human pattern %q on task %s — "+
			"re-run with --confirm if that is what you mean", command, pattern, active.ID)
	}

	change := system.Change{
		Kind: *kind, Target: *target, Command: command,
		Rollback: *rollback, Irreversible: *irreversible, Reason: *reason,
	}
	if active != nil {
		change.Task = active.ID
	}

	decision := env.guard(id.ID, active).Allow(autonomy.Action{
		Kind:         autonomy.ActSystem,
		Target:       *target,
		Detail:       command,
		Command:      command,
		Rollback:     *rollback,
		Irreversible: *irreversible,
	})
	if !decision.Allowed {
		if log, lerr := env.auditLog(id.ID); lerr == nil {
			log.Record("local", "system.refused", command, decision.Reason, nil)
		}
		return errors.New(decision.Reason)
	}

	if *dryRun {
		fmt.Fprintf(env.Out, "would run  %s\nundo with  %s\npermitted  yes (%s)\n",
			command, orIrreversible(*rollback, *reason), env.policy(id.ID).Level)
		return nil
	}

	applied, runErr := system.Apply(ctx, env.Paths.Repo, id.ID, change, nil)
	if applied == nil {
		return runErr
	}

	// Journaled either way. A failed change still modified the machine often
	// enough that pretending otherwise is the dangerous assumption.
	fmt.Fprintf(env.Out, "change %s %s\n", applied.ID, applied.Status)
	if applied.Output != "" {
		fmt.Fprintln(env.Out, indent(applied.Output, "  "))
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		// Appended rather than Recorded so the rollback command lands in the
		// audit trail too: the log is what someone else reads to undo a change
		// they did not make.
		result := audit.ResultOK
		if !applied.Applied() {
			result = audit.ResultError
		}
		_, _ = log.Append(audit.Entry{
			Actor: "local", Action: "system.apply", Target: applied.ID,
			Detail: applied.Command, Result: result, Rollback: applied.Rollback,
		})
	}
	if active != nil {
		_ = task.Record(env.Paths.Repo, active.ID, id.ID, task.KindSystem,
			fmt.Sprintf("%s (%s) — undo: %s", command, applied.Status, orIrreversible(*rollback, *reason)))
	}
	autoSync(ctx, env, repo, id.ID, "nimbus: system change "+applied.ID)

	if runErr != nil {
		return fmt.Errorf("%s failed: %w\nroll back with `nimbus system rollback %s`", command, runErr, applied.ID)
	}
	fmt.Fprintf(env.Out, "undo with `nimbus system rollback %s`\n", applied.ID)
	return nil
}

func runSystemRollback(ctx context.Context, env *Env, args []string) error {
	wanted, args := takeArg(args)

	fs := flag.NewFlagSet("system rollback", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if wanted == "" {
		return errors.New("usage: nimbus system rollback <change-id>")
	}

	id, err := env.identity()
	if err != nil {
		return err
	}
	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	autoRefresh(ctx, env)

	undone, rerr := system.Rollback(ctx, env.Paths.Repo, id.ID, wanted, nil)
	if undone == nil {
		return rerr
	}

	fmt.Fprintf(env.Out, "change %s %s\n", undone.ID, undone.Status)
	if undone.Output != "" {
		fmt.Fprintln(env.Out, indent(undone.Output, "  "))
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "system.rollback", undone.ID, undone.Rollback, rerr)
	}
	autoSync(ctx, env, repo, id.ID, "nimbus: rollback "+undone.ID)
	return rerr
}

func runSystemList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("system list", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	all := fs.Bool("all", false, "include changes made on other devices")
	limit := fs.Int("n", 20, "how many to show (0 for all)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	autoRefresh(ctx, env)
	changes, err := system.History(env.Paths.Repo)
	if err != nil {
		return err
	}

	var selfID string
	if id, ierr := env.identity(); ierr == nil {
		selfID = id.ID
	}
	if !*all {
		mine := changes[:0]
		for _, c := range changes {
			if c.Node == selfID {
				mine = append(mine, c)
			}
		}
		changes = mine
	}
	if len(changes) == 0 {
		fmt.Fprintln(env.Out, "no system changes recorded on this device")
		return nil
	}
	if *limit > 0 && len(changes) > *limit {
		changes = changes[:*limit]
	}

	fleet := env.fleetLabels()
	for _, c := range changes {
		line := c.Summary()
		if *all {
			line = fmt.Sprintf("%-14s %s", truncate(fleet.Label(c.Node), 14), line)
		}
		fmt.Fprintln(env.Out, line)
	}
	return nil
}

func runSystemShow(ctx context.Context, env *Env, args []string) error {
	wanted, args := takeArg(args)

	fs := flag.NewFlagSet("system show", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if wanted == "" {
		return errors.New("usage: nimbus system show <change-id>")
	}

	autoRefresh(ctx, env)
	c, err := system.Load(env.Paths.Repo, wanted)
	if err != nil {
		return err
	}

	fleet := env.fleetLabels()
	fmt.Fprintf(env.Out, "change   %s\ndevice   %s\nwhen     %s\nstatus   %s\nkind     %s\n",
		c.ID, fleet.Label(c.Node), c.At.Format("2006-01-02 15:04:05"), c.Status, c.Kind)
	if c.Target != "" {
		fmt.Fprintf(env.Out, "target   %s\n", c.Target)
	}
	if c.Task != "" {
		fmt.Fprintf(env.Out, "task     %s\n", c.Task)
	}
	fmt.Fprintf(env.Out, "command  %s\nundo     %s\n", c.Command, orIrreversible(c.Rollback, c.Reason))
	if c.Err != "" {
		fmt.Fprintf(env.Out, "error    %s\n", c.Err)
	}
	if c.Output != "" {
		fmt.Fprintf(env.Out, "\noutput\n%s", indent(c.Output, "  "))
	}
	return nil
}

func orIrreversible(rollback, reason string) string {
	if rollback != "" {
		return rollback
	}
	if reason != "" {
		return "irreversible: " + reason
	}
	return "irreversible"
}
