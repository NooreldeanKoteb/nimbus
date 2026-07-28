package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/nkoteb/nimbus/internal/autonomy"
	"github.com/nkoteb/nimbus/internal/device"
	"github.com/nkoteb/nimbus/internal/system"
	"github.com/nkoteb/nimbus/internal/task"
)

// checkTimeout bounds one allowed_first command. These are meant to be status
// probes, so a slow one is a broken one and should not hold up the resume.
const checkTimeout = 2 * time.Minute

// bootTimeout bounds an unattended session started at boot. Long enough for
// real recovery work, short enough that a wedged session does not sit holding
// the task's claim until somebody notices.
const bootTimeout = 30 * time.Minute

// runBoot picks up work that was in flight when the machine restarted.
//
// The sequence is the resume contract (DESIGN.md §10) made executable:
//
//  1. Nimbus itself runs the task's allowed_first checks and captures what they
//     say. Running them here rather than instructing the session to run them
//     first is what makes "only these may run before state is re-established"
//     an enforced rule instead of a request.
//  2. Below L3, it writes down what it would do and stops. That is the
//     propose-only mode the design asks to ship first, and it is the default.
//  3. At L3 with --act, it starts the session unattended.
//
// Like `nimbus context`, this must never exit non-zero: it runs from a service
// unit at boot, where a failure is a red unit nobody reads rather than an error
// anybody sees.
func runBoot(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("boot", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	act := fs.Bool("act", false, "start the session rather than only proposing it")
	attended := fs.Bool("attended", false, "a person is watching this run")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Boot is unattended by definition — a service manager started it. The flag
	// is the escape for testing the path by hand.
	env.Attended = *attended

	id, err := env.identity()
	if err != nil {
		return nil
	}
	autoRefresh(ctx, env)

	t, err := task.BootTask(env.Paths.Repo, id.ID)
	if err != nil {
		fmt.Fprintln(env.Out, "nothing to resume: no task on this device asked to survive a reboot")
		fmt.Fprintln(env.Out, "mark one with `nimbus task new <id> --on-boot`")
		return nil
	}

	fmt.Fprintf(env.Out, "resuming %s after restart on %s\n\n", t.ID, id.Label())
	checks := runBootChecks(ctx, env, t)

	decision := env.guard(id.ID, t).Allow(autonomy.Action{
		Kind:   autonomy.ActBoot,
		Target: t.ID,
		Detail: "resume after reboot",
	})

	profile := device.Detect(ctx, id, nil)
	brief := task.Brief(env.Paths.Repo, profile, t, 0) + bootSection(t, checks)

	if !decision.Allowed || !*act {
		return proposeBoot(ctx, env, id.ID, t, brief, decision)
	}
	return actOnBoot(ctx, env, id.ID, t, brief)
}

// bootCheck is one allowed_first command and what it reported.
type bootCheck struct {
	Command string
	Output  string
	Err     error
}

func runBootChecks(ctx context.Context, env *Env, t *task.Task) []bootCheck {
	commands := t.Contract().AllowedFirst
	if len(commands) == 0 {
		return nil
	}

	fmt.Fprintln(env.Out, "re-establishing state:")
	checks := make([]bootCheck, 0, len(commands))
	for _, command := range commands {
		runCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		out, err := system.Shell(runCtx, command)
		cancel()

		checks = append(checks, bootCheck{Command: command, Output: strings.TrimSpace(out), Err: err})
		status := "ok"
		if err != nil {
			status = err.Error()
		}
		fmt.Fprintf(env.Out, "  %-40s %s\n", truncate(command, 40), status)
	}
	fmt.Fprintln(env.Out)
	return checks
}

// bootSection renders the check results into the brief, so the session opens
// already knowing the state of the machine it woke up on.
func bootSection(t *task.Task, checks []bootCheck) string {
	var b strings.Builder
	b.WriteString("\n## After the reboot\n\n")
	b.WriteString("This machine restarted while this task was in flight. ")

	if len(checks) == 0 {
		b.WriteString("No `allowed_first` checks were configured, so nothing has been verified yet — " +
			"establish the state of the machine before changing anything.\n")
	} else {
		b.WriteString("Nimbus already ran the task's `allowed_first` checks:\n\n")
		for _, c := range checks {
			fmt.Fprintf(&b, "- `%s`", c.Command)
			if c.Err != nil {
				fmt.Fprintf(&b, " — **failed**: %v", c.Err)
			}
			b.WriteString("\n")
			if c.Output != "" {
				fmt.Fprintf(&b, "```\n%s\n```\n", truncate(c.Output, 2000))
			}
		}
		b.WriteString("\nReport what these mean before making further changes.\n")
	}

	if human := t.Contract().RequireHuman; len(human) > 0 {
		fmt.Fprintf(&b, "\nThese always need a person, whatever the autonomy level: %s. "+
			"`nimbus system apply` will refuse them here.\n", strings.Join(human, ", "))
	}
	b.WriteString("\nMake system changes with `nimbus system apply \"<cmd>\" --rollback \"<undo>\"` " +
		"so they can be undone by someone who did not watch this happen.\n")
	return b.String()
}

// proposeBoot is the default: say what would happen and stop.
func proposeBoot(ctx context.Context, env *Env, nodeID string, t *task.Task, brief string, d autonomy.Decision) error {
	reason := "propose-only (pass --act to start the session)"
	if !d.Allowed {
		reason = d.Reason
	}

	_ = task.Record(env.Paths.Repo, t.ID, nodeID, task.KindProposal,
		"machine restarted; ready to resume — "+reason)
	if log, lerr := env.auditLog(nodeID); lerr == nil {
		log.Record("local", "boot.propose", t.ID, reason, nil)
	}
	if repo, rerr := env.stateRepo(); rerr == nil {
		autoSync(ctx, env, repo, nodeID, "nimbus: boot proposal for "+t.ID)
	}

	fmt.Fprint(env.Out, brief)
	fmt.Fprintf(env.Out, "\nnot starting a session: %s\n", reason)
	fmt.Fprintf(env.Out, "resume it yourself with `nimbus resume %s --exec`\n", t.ID)
	return nil
}

// actOnBoot starts the session unattended. Only reached at L3.
//
// The session itself is runSession, shared with peer dispatch: an unattended
// session is the same event whether the machine restarted or another device
// asked, and the two must leave the same trail behind.
func actOnBoot(ctx context.Context, env *Env, nodeID string, t *task.Task, brief string) error {
	err := runSession(ctx, env, nodeID, t, brief, bootTimeout)
	if errors.Is(err, errNoClaude) {
		fmt.Fprintln(env.Out, "claude is not installed on this device; leaving a proposal instead")
		return proposeBoot(ctx, env, nodeID, t, brief, autonomy.Decision{Allowed: true})
	}
	// Boot must never exit non-zero: it runs from a service unit, where a
	// failure is a red unit nobody reads. The failure is on the task timeline.
	return nil
}
