package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/nkoteb/nimbus/internal/device"
)

// runAlias shows or sets this device's human-readable name.
//
// The alias is a label over the node id, never a replacement for it: the id is
// embedded in every state repo path and inside every hash-chained audit entry,
// so renaming it would orphan the history and break the chain. Aliases are
// therefore free to change as often as you like.
func runAlias(ctx context.Context, env *Env, args []string) error {
	name, args := takeArg(args)

	fs := flag.NewFlagSet("alias", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	force := fs.Bool("force", false, "take an alias another device is already using")
	if err := fs.Parse(args); err != nil {
		return err
	}

	id, err := env.identity()
	if err != nil {
		return err
	}

	if name == "" {
		fmt.Fprintf(env.Out, "%s\n", id.Label())
		fmt.Fprintf(env.Out, "id       %s\nhostname %s\n", id.ID, id.Hostname)
		if id.Alias == "" {
			fmt.Fprintln(env.Out, "\nno alias set — run `nimbus alias <name>` to name this device")
		}
		return nil
	}

	// Another device answering to the same name makes the name useless for
	// addressing either of them.
	autoRefresh(ctx, env)
	fleet, ferr := device.LoadFleet(env.Paths.Repo)
	if ferr == nil && fleet.AliasTaken(name, id.ID) && !*force {
		return fmt.Errorf("another device is already called %q (run `nimbus fleet`, or --force)", name)
	}

	previous := id.Label()
	if err := id.SetAlias(env.Paths.IdentityFile(), name); err != nil {
		return err
	}

	// Republish so the rest of the fleet sees the new name. Without this the
	// rename is invisible on every other device.
	repo, rerr := env.stateRepo()
	if rerr != nil {
		fmt.Fprintf(env.Out, "%s is now %s (local only: %v)\n", previous, name, rerr)
		return nil
	}
	if _, err := device.Detect(ctx, id, nil).Save(env.Paths.Repo); err != nil {
		return err
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "device.alias", id.ID, previous+" -> "+name, nil)
	}

	fmt.Fprintf(env.Out, "%s is now %s\n", previous, name)
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: alias %s -> %s", id.ID, name))
	return nil
}

// fleetLabels loads the fleet purely for rendering node ids as names. A failure
// is not worth reporting: the caller falls back to short ids, which are still
// readable, and the command the user actually asked for still runs.
func (e *Env) fleetLabels() device.Fleet {
	fleet, err := device.LoadFleet(e.Paths.Repo)
	if err != nil {
		return nil
	}
	return fleet
}

// resolveNode turns a device reference typed by the user — alias, full id, or
// unambiguous prefix — into a node id.
func (e *Env) resolveNode(ref string) (string, error) {
	fleet, err := device.LoadFleet(e.Paths.Repo)
	if err != nil {
		return "", err
	}
	if len(fleet) == 0 {
		return "", errors.New("no devices published yet (run `nimbus doctor --publish`)")
	}
	return fleet.Resolve(ref)
}
