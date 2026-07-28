package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/nkoteb/nimbus/internal/daemon"
	"github.com/nkoteb/nimbus/internal/state"
)

func runDaemon(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nimbus daemon <run|install|status|uninstall>")
	}
	switch args[0] {
	case "run":
		return runDaemonRun(ctx, env, args[1:])
	case "install":
		return runDaemonInstall(ctx, env, args[1:])
	case "status":
		return runDaemonStatus(ctx, env, args[1:])
	case "uninstall":
		return runDaemonUninstall(ctx, env, args[1:])
	default:
		return fmt.Errorf("unknown daemon subcommand %q", args[0])
	}
}

func runDaemonRun(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	interval := fs.Duration("interval", daemon.DefaultInterval, "how often to sync")
	once := fs.Bool("once", false, "run a single cycle and exit")
	noBoot := fs.Bool("no-boot", false, "do not look for a task to resume on startup")
	act := fs.Bool("act", false, "let boot resume start a session rather than only proposing one")
	work := fs.Bool("work", false, "run commands peers ask for, from this device's allowlist")
	if err := fs.Parse(args); err != nil {
		return err
	}

	id, err := env.identity()
	if err != nil {
		return err
	}
	if _, err := env.stateRepo(); err != nil {
		return err
	}

	if !*once {
		fmt.Fprintf(env.Out, "nimbus daemon: syncing %s every %s (ctrl-c to stop)\n",
			id.Label(), *interval)
	}

	return daemon.Run(ctx, func() (*state.Repo, error) {
		// Reopened each cycle so a token refreshed while the daemon runs is
		// picked up without a restart.
		return state.Open(env.Paths.Repo, env.repoAuth())
	}, daemon.Options{
		RepoPath: env.Paths.Repo,
		NodeID:   id.ID,
		Owned:    ownedPaths(id.ID),
		Interval: *interval,
		Once:     *once,
		Out:      env.Out,
		OnStart:  bootHook(env, *noBoot, *act),
		Work:     workHook(env, *work, *act),
	})
}

// workHook is what the daemon runs each cycle to do what peers have asked for.
//
// --no-sync is not optional here: the daemon's own loop owns the repo, and two
// git operations on one worktree at the same time corrupt the index. The worker
// writes files; the next cycle pushes them, which is also what makes a long
// command's output visible on another machine before it finishes.
func workHook(env *Env, enabled, act bool) func(context.Context) error {
	if !enabled {
		return nil
	}
	return func(ctx context.Context) error {
		args := []string{"work", "--no-sync"}
		if act {
			args = append(args, "--act")
		}
		return run(ctx, env, args)
	}
}

// bootHook is what the daemon runs once at startup to pick up work that was in
// flight when the machine went down.
//
// Reusing the `boot` command rather than calling into the packages directly
// keeps one implementation of the resume contract: the daemon's boot and a
// person typing `nimbus boot` cannot diverge on what is permitted.
func bootHook(env *Env, disabled, act bool) func(context.Context) error {
	if disabled {
		return nil
	}
	return func(ctx context.Context) error {
		args := []string{"boot"}
		if act {
			args = append(args, "--act")
		}
		return run(ctx, env, args)
	}
}

func runDaemonInstall(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("daemon install", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	interval := fs.Duration("interval", daemon.DefaultInterval, "how often to sync")
	act := fs.Bool("act", false, "let boot resume start a session rather than only proposing one")
	work := fs.Bool("work", false, "run commands peers ask for, from this device's allowlist")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Installing a service that starts at login only to fail every minute is
	// worse than not installing it.
	if _, err := env.stateRepo(); err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}

	svc, err := daemon.Install(daemon.ServiceOptions{
		Interval: interval.String(), Act: *act, Work: *work,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "installed %s unit at %s\n", svc.Manager, svc.Path)
	if *act {
		fmt.Fprintf(env.Out, "boot resume will start a session (needs %s)\n", "autonomy l3")
	} else {
		fmt.Fprintln(env.Out, "boot resume will write a proposal — `--act` to start the session instead")
	}
	if *work {
		// Naming the allowlist here rather than only in the docs: this is the
		// moment somebody has decided to let other machines drive this one, and
		// the empty list is the difference between that being safe and not.
		p := env.policy(id.ID)
		fmt.Fprintf(env.Out, "will run peer requests — %d command(s) allowed, %s\n",
			len(p.Exec), p.Level)
		if len(p.Exec) == 0 {
			fmt.Fprintln(env.Out, "  nothing is allowed yet: `nimbus autonomy allow \"<command>\"`")
		}
	}
	if svc.Enable != "" {
		// A headless SSH session has no user D-Bus, so nimbus cannot start the
		// unit itself and has to say so rather than pretend it did.
		fmt.Fprintf(env.Out, "could not start it from here — run:\n  %s\n", svc.Enable)
	} else {
		fmt.Fprintf(env.Out, "running now, syncing %s every %s\n", id.Label(), *interval)
	}
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "daemon.install", svc.Manager, svc.Path, nil)
	}
	return nil
}

func runDaemonStatus(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("daemon status", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}

	svc := daemon.Status()
	if !svc.Installed {
		fmt.Fprintf(env.Out, "daemon not installed (run `nimbus daemon install`)\n")
		if svc.Manager != "systemd" && svc.Manager != "launchd" {
			fmt.Fprintf(env.Out, "no service integration for %s — use `nimbus daemon run`\n", svc.Manager)
		}
		return nil
	}

	fmt.Fprintf(env.Out, "unit    %s\nmanager %s\n", svc.Path, svc.Manager)
	if svc.Enable != "" {
		fmt.Fprintf(env.Out, "state   %s\n", svc.Enable)
	}

	// The freshest signal is not what the service manager thinks, but whether
	// the repo has actually moved.
	if repo, err := state.Open(env.Paths.Repo, env.repoAuth()); err == nil {
		if when, err := repo.LastCommitTime(); err == nil {
			fmt.Fprintf(env.Out, "synced  %s ago\n", time.Since(when).Round(time.Second))
		}
	}
	return nil
}

func runDaemonUninstall(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("daemon uninstall", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}

	path, err := daemon.Uninstall()
	if err != nil {
		return err
	}
	if path == "" {
		fmt.Fprintln(env.Out, "daemon was not installed")
		return nil
	}
	fmt.Fprintf(env.Out, "removed %s\n", path)
	return nil
}
