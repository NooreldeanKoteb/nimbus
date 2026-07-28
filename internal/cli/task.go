package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/nkoteb/nimbus/internal/state"
	"github.com/nkoteb/nimbus/internal/task"
)

// stringList collects a repeatable flag, so Claude can pass several --next
// items in one non-interactive invocation.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, "; ") }

func (s *stringList) Set(v string) error {
	if v = strings.TrimSpace(v); v != "" {
		*s = append(*s, v)
	}
	return nil
}

// takeArg pulls a leading positional argument out of args.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `nimbus resume <id> --brief` would otherwise parse the id and silently ignore
// every flag after it. Removing the id first makes both orderings work.
func takeArg(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func runTask(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nimbus task <new|list|queue|show|note|handoff|steal|done|release> [args]")
	}
	switch args[0] {
	case "new":
		return runTaskNew(ctx, env, args[1:])
	case "list":
		return runTaskList(ctx, env, args[1:])
	case "queue":
		return runTaskQueue(ctx, env, args[1:])
	case "steal":
		return runTaskSteal(ctx, env, args[1:])
	case "show":
		return runTaskShow(ctx, env, args[1:])
	case "note":
		return runTaskNote(ctx, env, args[1:])
	case "handoff":
		return runTaskHandoff(ctx, env, args[1:])
	case "done":
		return runTaskDone(ctx, env, args[1:])
	case "release":
		return runTaskRelease(ctx, env, args[1:])
	default:
		return fmt.Errorf("unknown task subcommand %q", args[0])
	}
}

// stateRepo opens the state repo with a message that says how to create one,
// since every task command is useless without it.
func (e *Env) stateRepo() (*state.Repo, error) {
	repo, err := state.Open(e.Paths.Repo, e.repoAuth())
	if errors.Is(err, state.ErrNotARepo) {
		return nil, fmt.Errorf("no state repo at %s (run `nimbus init --create-repo`)", e.Paths.Repo)
	}
	return repo, err
}

// resolveTask finds the task a command applies to: the named one, or whatever
// this device currently holds.
func (e *Env) resolveTask(id, nodeID string) (*task.Task, error) {
	if id != "" {
		return task.Load(e.Paths.Repo, id)
	}

	t, err := task.Active(e.Paths.Repo, nodeID)
	if errors.Is(err, task.ErrNotFound) {
		return nil, errors.New("no task claimed on this device (use --task <id>, or `nimbus resume <id>`)")
	}
	return t, err
}

func runTaskNew(ctx context.Context, env *Env, args []string) error {
	newID, args := takeArg(args)

	fs := flag.NewFlagSet("task new", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	goal := fs.String("goal", "", "what this task is trying to achieve")
	repoURL := fs.String("repo", "", "git remote of the project this task works on")
	branch := fs.String("branch", "", "branch the work lives on")
	var needs stringList
	fs.Var(&needs, "needs", "capability this task requires (repeatable, e.g. gpu:nvidia)")
	onBoot := fs.Bool("on-boot", false, "pick this task back up when the machine restarts")
	var allowedFirst, requireHuman stringList
	fs.Var(&allowedFirst, "allowed-first", "command nimbus runs to re-establish state after a reboot (repeatable)")
	fs.Var(&requireHuman, "require-human", "command pattern that always needs a person (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if newID == "" {
		return errors.New("usage: nimbus task new <id> --goal \"...\"")
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}

	t := &task.Task{
		ID:        newID,
		Goal:      *goal,
		Repo:      *repoURL,
		Branch:    *branch,
		Needs:     needs,
		CreatedOn: id.ID,
	}
	// A contract is only attached when something was actually asked for: an
	// empty one on every task would make `resume.on_boot` look meaningful
	// everywhere and mean nothing anywhere.
	if *onBoot || len(allowedFirst) > 0 || len(requireHuman) > 0 {
		t.Resume = &task.Contract{
			OnBoot:       *onBoot,
			AllowedFirst: allowedFirst,
			RequireHuman: requireHuman,
		}
	}
	t.Take(id.ID, id.Hostname)

	if err := task.New(env.Paths.Repo, t); err != nil {
		return err
	}
	if err := task.Record(env.Paths.Repo, t.ID, id.ID, task.KindClaim, "created: "+t.Goal); err != nil {
		return err
	}

	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "task.new", t.ID, t.Goal, nil)
	}

	fmt.Fprintf(env.Out, "task %s created and claimed by this device\n", t.ID)
	if len(t.Needs) > 0 {
		fmt.Fprintf(env.Out, "requires: %s\n", strings.Join(t.Needs, ", "))
	}
	if t.ResumesOnBoot() {
		fmt.Fprintln(env.Out, "survives a reboot: `nimbus boot` will pick it back up")
	}
	autoSync(ctx, env, repo, id.ID, "nimbus: new task "+t.ID)
	return nil
}

func runTaskList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task list", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	all := fs.Bool("all", false, "include finished tasks")
	asJSON := fs.Bool("json", false, "emit tasks as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Another device may have created or claimed a task since we last synced.
	autoRefresh(ctx, env)

	tasks, err := task.List(env.Paths.Repo)
	if err != nil {
		return err
	}
	if !*all {
		open := tasks[:0]
		for _, t := range tasks {
			if t.Status != task.StatusDone {
				open = append(open, t)
			}
		}
		tasks = open
	}

	if *asJSON {
		data, err := json.MarshalIndent(tasks, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(env.Out, string(data))
		return nil
	}

	if len(tasks) == 0 {
		fmt.Fprintln(env.Out, "no open tasks (create one with `nimbus task new <id> --goal \"...\"`)")
		return nil
	}

	var selfID string
	if id, err := env.identity(); err == nil {
		selfID = id.ID
	}
	fleet := env.fleetLabels()
	for _, t := range tasks {
		// Mark what this device holds, so an operator on a borrowed machine is
		// never confused about which work is theirs to continue.
		marker := " "
		if t.HeldBy(selfID) {
			marker = "*"
		}
		fmt.Fprintf(env.Out, "%s %-20s %-30s %s\n", marker, t.ID, truncate(t.Goal, 30), t.Summary(fleet))
	}
	return nil
}

func runTaskShow(ctx context.Context, env *Env, args []string) error {
	wanted, args := takeArg(args)

	fs := flag.NewFlagSet("task show", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	limit := fs.Int("n", 0, "number of progress events to show (0 for all)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	autoRefresh(ctx, env)

	var nodeID string
	if id, err := env.identity(); err == nil {
		nodeID = id.ID
	}
	t, err := env.resolveTask(wanted, nodeID)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "task     %s\ngoal     %s\nstatus   %s\n", t.ID, t.Goal, t.Status)
	if t.Repo != "" {
		fmt.Fprintf(env.Out, "repo     %s\n", t.Repo)
	}
	if t.Branch != "" {
		fmt.Fprintf(env.Out, "branch   %s\n", t.Branch)
	}
	fleet := env.fleetLabels()
	if t.Claim != nil {
		fmt.Fprintf(env.Out, "held by  %s since %s\n", fleet.Label(t.Claim.Node), t.Claim.At.Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintln(env.Out, "held by  nobody")
	}
	if len(t.Needs) > 0 {
		fmt.Fprintf(env.Out, "requires %s\n", strings.Join(t.Needs, ", "))
	}
	if c := t.Resume; c != nil {
		if c.OnBoot {
			fmt.Fprintln(env.Out, "on boot  resumes on this device after a restart")
		}
		if len(c.AllowedFirst) > 0 {
			fmt.Fprintf(env.Out, "first    %s\n", strings.Join(c.AllowedFirst, "; "))
		}
		if len(c.RequireHuman) > 0 {
			fmt.Fprintf(env.Out, "blocked  %s (always needs a person)\n", strings.Join(c.RequireHuman, ", "))
		}
	}
	// Only this device's own session is worth showing: another device's
	// transcript is not on this machine and cannot be resumed from here.
	if s, serr := task.LoadSession(env.Paths.Repo, t.ID, nodeID); serr == nil && s.SessionID != "" {
		fmt.Fprintf(env.Out, "session  %s (this device)\n", s.SessionID)
	}

	handoffs, _ := task.Handoffs(env.Paths.Repo, t.ID)
	if len(handoffs) > 0 {
		var b strings.Builder
		fmt.Fprintln(env.Out, "\nlatest handoff")
		handoffs[0].Render(&b, fleet)
		fmt.Fprint(env.Out, indent(b.String(), "  "))
	}

	events, err := task.Progress(env.Paths.Repo, t.ID)
	if err != nil || len(events) == 0 {
		return nil
	}
	shown := events
	if *limit > 0 && len(events) > *limit {
		shown = events[len(events)-*limit:]
	}
	fmt.Fprintln(env.Out, "\nprogress")
	for _, e := range shown {
		fmt.Fprintf(env.Out, "  %s  %-16s %-8s %s\n",
			e.Timestamp.Format("2006-01-02 15:04"), truncate(fleet.Label(e.Node), 16), e.Kind, truncate(e.Text, 60))
	}
	return nil
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
