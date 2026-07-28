package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/nkoteb/nimbus/internal/device"
	"github.com/nkoteb/nimbus/internal/task"
)

// runTaskQueue lists work this device could pick up right now.
//
// A claim is a lease, not a lock (DESIGN.md §7). Without a way to see lapsed
// leases, a device that goes offline mid-task takes that work with it and
// nothing else in the fleet can continue it — which is the failure the whole
// state repo exists to prevent.
func runTaskQueue(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("task queue", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	lease := fs.Duration("lease", task.DefaultLease, "how long a claim survives without activity")
	anyDevice := fs.Bool("any", false, "include work this device cannot meet the requirements for")
	if err := fs.Parse(args); err != nil {
		return err
	}

	autoRefresh(ctx, env)
	id, err := env.identity()
	if err != nil {
		return err
	}

	var open []*task.Task
	if *anyDevice {
		open, err = stealableAnywhere(env.Paths.Repo, id.ID, *lease)
	} else {
		open, err = task.Stealable(env.Paths.Repo, id.ID, device.Detect(ctx, id, nil).Capabilities(), *lease)
	}
	if err != nil {
		return err
	}

	if len(open) == 0 {
		fmt.Fprintf(env.Out, "nothing to pick up (claims lapse after %s of no activity)\n", *lease)
		return nil
	}

	fleet := env.fleetLabels()
	for _, t := range open {
		fmt.Fprintf(env.Out, "%-20s %-30s %s\n",
			t.ID, truncate(t.Goal, 30), t.StealReason(env.Paths.Repo, *lease, fleet))
	}
	fmt.Fprintf(env.Out, "\ntake one with `nimbus task steal <id>`\n")
	return nil
}

// runTaskSteal takes a task whose holder has gone quiet.
//
// Separate from `resume --force` on purpose: force is "I know better", steal is
// "the lease lapsed", and the timeline records them differently because a person
// reading it later needs to tell the two apart.
func runTaskSteal(ctx context.Context, env *Env, args []string) error {
	wanted, args := takeArg(args)

	fs := flag.NewFlagSet("task steal", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	lease := fs.Duration("lease", task.DefaultLease, "how long a claim survives without activity")
	force := fs.Bool("force", false, "take it even though the lease has not lapsed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if wanted == "" {
		return errors.New("usage: nimbus task steal <id>")
	}

	autoRefresh(ctx, env)
	id, err := env.identity()
	if err != nil {
		return err
	}
	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	t, err := task.Load(env.Paths.Repo, wanted)
	if err != nil {
		return err
	}

	if t.HeldBy(id.ID) {
		fmt.Fprintf(env.Out, "%s is already held by this device\n", t.ID)
		return nil
	}

	fleet := env.fleetLabels()
	if t.Claim != nil && !t.LeaseExpired(env.Paths.Repo, *lease, time.Now().UTC()) && !*force {
		return fmt.Errorf("%s is still active: %s\nwait for the lease to lapse, ask them to run "+
			"`nimbus task release`, or --force to take it now",
			t.ID, t.StealReason(env.Paths.Repo, *lease, fleet))
	}

	profile := device.Detect(ctx, id, nil)
	if unmet := t.Unmet(profile.Capabilities()); len(unmet) > 0 && !*force {
		return fmt.Errorf("%s needs %s, which this device does not have\n"+
			"run `nimbus fleet` to find one that does",
			t.ID, strings.Join(unmet, ", "))
	}

	from := ""
	if t.Claim != nil {
		from = fleet.Label(t.Claim.Node)
	}
	t.Take(id.ID, id.Hostname)
	if err := t.Save(env.Paths.Repo); err != nil {
		return err
	}
	if err := task.Record(env.Paths.Repo, t.ID, id.ID, task.KindSteal,
		fmt.Sprintf("taken by %s from %s (lease lapsed)", id.Label(), orNobody(from))); err != nil {
		return err
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "task.steal", t.ID, "taken from "+orNobody(from), nil)
	}

	fmt.Fprintf(env.Out, "%s taken from %s\n", t.ID, orNobody(from))
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: steal %s to %s", t.ID, id.ID))

	// task.json is shared, so the steal can lose a race with the original
	// holder waking up and pushing first. Say so rather than assume.
	if current, lerr := task.Load(env.Paths.Repo, t.ID); lerr == nil && !current.HeldBy(id.ID) {
		fmt.Fprintf(env.Out, "\n! %s is held by %s after syncing — the steal did not stick\n",
			t.ID, orNobody(fleet.Label(claimNode(current))))
		return nil
	}
	fmt.Fprintf(env.Out, "pick it up with `nimbus resume %s`\n", t.ID)
	return nil
}

// stealableAnywhere is Stealable without the capability filter, for an operator
// who wants to see everything that has gone quiet across the fleet.
func stealableAnywhere(repoPath, selfID string, lease time.Duration) ([]*task.Task, error) {
	tasks, err := task.List(repoPath)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	var open []*task.Task
	for _, t := range tasks {
		if t.Status != task.StatusActive || t.HeldBy(selfID) {
			continue
		}
		if t.Claim != nil && !t.LeaseExpired(repoPath, lease, now) {
			continue
		}
		open = append(open, t)
	}
	return open, nil
}
