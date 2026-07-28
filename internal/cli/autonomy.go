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
func (e *Env) guard(nodeID string, t *task.Task) autonomy.Guard {
	g := autonomy.Guard{
		Level:    e.policy(nodeID).Level,
		Attended: e.Attended,
		WorkTree: e.Paths.Repo,
	}
	if t != nil {
		g.AllowedCommands = t.Contract().AllowedFirst
	}
	return g
}

func runAutonomy(ctx context.Context, env *Env, args []string) error {
	wanted, args := takeArg(args)

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
