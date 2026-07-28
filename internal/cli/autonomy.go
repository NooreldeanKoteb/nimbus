package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/nkoteb/nimbus/internal/autonomy"
	"github.com/nkoteb/nimbus/internal/task"
)

// policy reads this device's autonomy ceiling. It never fails: a device that
// cannot read its own policy falls back to the default rather than to whatever
// it was last told, which is the conservative direction.
func (e *Env) policy(nodeID string) *autonomy.Policy {
	return autonomy.Load(e.Paths.Repo, nodeID)
}

// guard builds the check applied to an action on this device.
//
// Attended comes from the Env rather than from a flag, because whether a person
// is present is a property of how nimbus was invoked — a terminal, an MCP tool
// call from Claude, or a systemd unit at boot — and not something the command
// being run gets to assert about itself.
// The task argument is accepted for callers that have one, but the peer-exec
// allowlist comes from this device's policy and never from the task: a task
// travels between machines, so letting it carry its own allowlist would let the
// device that wrote it decide what runs here.
func (e *Env) guard(nodeID string, _ *task.Task) autonomy.Guard {
	p := e.policy(nodeID)
	return autonomy.Guard{
		Level:           p.Level,
		Attended:        e.Attended,
		WorkTree:        e.Paths.Repo,
		AllowedCommands: p.Exec,
	}
}

func runAutonomy(ctx context.Context, env *Env, args []string) error {
	wanted, args := takeArg(args)

	// allow/deny edit the peer-exec allowlist rather than the level. They live
	// here because they are the same decision seen from a different angle: the
	// level says how far this device goes on its own, the allowlist says how
	// far another device may push it.
	switch wanted {
	case "allow", "deny":
		return runAllowlist(ctx, env, wanted, args)
	}

	fs := flag.NewFlagSet("autonomy", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	note := fs.String("note", "", "why this device runs at this level")
	if err := fs.Parse(args); err != nil {
		return err
	}

	id, err := env.identity()
	if err != nil {
		return err
	}

	if wanted == "" {
		autoRefresh(ctx, env)
		return showAutonomy(env, id.ID)
	}

	level, err := autonomy.Parse(wanted)
	if err != nil {
		return err
	}
	repo, err := env.stateRepo()
	if err != nil {
		return err
	}

	previous := env.policy(id.ID).Level
	p := &autonomy.Policy{Node: id.ID, Level: level, Note: *note}
	if err := p.Save(env.Paths.Repo); err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "%s is now %s — %s\n", id.Label(), level, level.Describe())
	if level > previous {
		// Raising the ceiling is the change worth being explicit about, since
		// it is the one that lets more happen while nobody is watching.
		fmt.Fprintf(env.Out, "raised from %s; the invariants below still hold\n", previous)
		printInvariants(env)
	}

	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "autonomy.set", level.String(),
			fmt.Sprintf("was %s%s", previous, detailSuffix(*note)), nil)
	}
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: autonomy %s on %s", level, id.ID))
	return nil
}

// runAllowlist adds or removes a command peers may run on this device.
func runAllowlist(ctx context.Context, env *Env, verb string, args []string) error {
	command := strings.TrimSpace(strings.Join(args, " "))
	if command == "" {
		return fmt.Errorf("usage: nimbus autonomy %s \"<command>\"", verb)
	}

	id, err := env.identity()
	if err != nil {
		return err
	}
	repo, err := env.stateRepo()
	if err != nil {
		return err
	}

	p := env.policy(id.ID)
	changed := p.Allow(command)
	if verb == "deny" {
		changed = p.Deny(command)
	}
	if !changed {
		fmt.Fprintf(env.Out, "no change: %q is %son the allowlist\n", command, notPrefix(verb))
		return nil
	}
	if err := p.Save(env.Paths.Repo); err != nil {
		return err
	}

	if verb == "allow" {
		fmt.Fprintf(env.Out, "peers may now run %q on %s\n", command, id.Label())
		if p.Level < autonomy.L3 {
			// Being on the list is necessary but not sufficient, and a person who
			// stops at "allow" will otherwise wonder why nothing runs.
			fmt.Fprintf(env.Out, "note: peer execution also needs %s — this device is at %s\n",
				autonomy.L3, p.Level)
		}
	} else {
		fmt.Fprintf(env.Out, "peers may no longer run %q on %s\n", command, id.Label())
	}

	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "autonomy."+verb, command, "peer-exec allowlist", nil)
	}
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: %s %q on %s", verb, command, id.ID))
	return nil
}

func notPrefix(verb string) string {
	if verb == "allow" {
		return "already "
	}
	return "not "
}

func showAutonomy(env *Env, nodeID string) error {
	p := env.policy(nodeID)

	fmt.Fprintf(env.Out, "level   %s — %s\n", p.Level, p.Level.Describe())
	if !p.SetAt.IsZero() {
		fmt.Fprintf(env.Out, "set     %s\n", p.SetAt.Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintln(env.Out, "set     never (this is the default)")
	}
	if p.Note != "" {
		fmt.Fprintf(env.Out, "note    %s\n", p.Note)
	}

	fmt.Fprintln(env.Out, "\nthe ladder")
	for _, l := range []autonomy.Level{autonomy.L0, autonomy.L1, autonomy.L2, autonomy.L3} {
		marker := " "
		if l == p.Level {
			marker = "*"
		}
		fmt.Fprintf(env.Out, "%s %s  %s\n", marker, l, l.Describe())
	}
	fmt.Fprintln(env.Out, "\npeers may run")
	if len(p.Exec) == 0 {
		fmt.Fprintln(env.Out, "  (nothing — `nimbus autonomy allow \"systemctl status nimbus\"`)")
	} else {
		for _, c := range p.Exec {
			fmt.Fprintf(env.Out, "  %s\n", c)
		}
	}

	printInvariants(env)
	fmt.Fprintln(env.Out, "\nchange it with `nimbus autonomy l2`")
	return nil
}

// printInvariants is shown on every level change and every status read.
//
// The ladder is only safe to raise because these do not move with it, and that
// is worth restating at exactly the moment somebody is deciding to raise it.
func printInvariants(env *Env) {
	fmt.Fprintln(env.Out, "\nalways enforced, at every level:")
	for _, line := range []string{
		"never force-push, and never push to a default branch",
		"never commit unencrypted secrets",
		"peer-dispatched commands run only from this device's allowlist",
		"every system change is journaled with its rollback before it runs",
		"destructive filesystem operations outside the tree need a human",
	} {
		fmt.Fprintf(env.Out, "  - %s\n", line)
	}
}

func detailSuffix(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return ": " + note
}
