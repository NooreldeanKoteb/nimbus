package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/nkoteb/nimbus/internal/auth"
	"github.com/nkoteb/nimbus/internal/state"
)

func runState(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nimbus state <init|clone|status|sync> [args]")
	}
	switch args[0] {
	case "init":
		return runStateInit(ctx, env, args[1:])
	case "clone":
		return runStateClone(ctx, env, args[1:])
	case "status":
		return runStateStatus(ctx, env, args[1:])
	case "sync":
		return runStateSync(ctx, env, args[1:])
	default:
		return fmt.Errorf("unknown state subcommand %q", args[0])
	}
}

// repoAuth builds git credentials from the stored provider token. A missing
// token is not fatal: public clones and purely local repos still work.
func (e *Env) repoAuth() transport.AuthMethod {
	id, err := e.store().Get("github")
	if err != nil {
		return nil
	}
	return state.TokenAuth(id.Token)
}

func runStateInit(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("state init", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	remote := fs.String("remote", "", "origin URL for the state repo")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repo, err := state.Init(env.Paths.Repo, env.repoAuth())
	if err != nil {
		return err
	}
	if *remote != "" {
		if err := repo.SetRemote(*remote); err != nil {
			return err
		}
	}

	fmt.Fprintf(env.Out, "state repo ready at %s\n", repo.Path)
	if url := repo.RemoteURL(); url != "" {
		fmt.Fprintf(env.Out, "origin: %s\n", url)
	}
	return nil
}

func runStateClone(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("state clone", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: nimbus state clone <url>")
	}

	url := fs.Arg(0)
	fmt.Fprintf(env.Out, "cloning %s -> %s\n", url, env.Paths.Repo)

	repo, err := state.Clone(ctx, url, env.Paths.Repo, env.repoAuth())
	if err != nil {
		if errors.Is(err, transport.ErrAuthenticationRequired) {
			return fmt.Errorf("%w (run `nimbus login` first)", err)
		}
		return err
	}

	branch, err := repo.HeadRef()
	if err != nil {
		// A cloned-but-empty repo has no HEAD yet; that is not a failure.
		fmt.Fprintf(env.Out, "cloned (empty repo)\n")
		return nil
	}
	fmt.Fprintf(env.Out, "cloned at branch %s\n", branch)
	return nil
}

func runStateStatus(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("state status", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}

	repo, err := state.Open(env.Paths.Repo, env.repoAuth())
	if errors.Is(err, state.ErrNotARepo) {
		return fmt.Errorf("no state repo at %s (run `nimbus state init` or `nimbus state clone <url>`)", env.Paths.Repo)
	}
	if err != nil {
		return err
	}

	clean, changed, err := repo.Status()
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "path:   %s\n", repo.Path)
	if url := repo.RemoteURL(); url != "" {
		fmt.Fprintf(env.Out, "origin: %s\n", url)
	}
	if branch, err := repo.HeadRef(); err == nil {
		fmt.Fprintf(env.Out, "branch: %s\n", branch)
	}
	if clean {
		fmt.Fprintln(env.Out, "status: clean")
		return nil
	}
	fmt.Fprintf(env.Out, "status: %d uncommitted change(s)\n", len(changed))
	for _, f := range changed {
		fmt.Fprintf(env.Out, "  %s\n", f)
	}
	return nil
}

func runStateSync(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("state sync", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	message := fs.String("message", "nimbus: sync state", "commit message")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repo, err := state.Open(env.Paths.Repo, env.repoAuth())
	if errors.Is(err, state.ErrNotARepo) {
		return fmt.Errorf("no state repo at %s", env.Paths.Repo)
	}
	if err != nil {
		return err
	}

	// State-changing commands sync on their own; this is the manual retry for
	// when a device was offline, or for changes made outside nimbus.
	var nodeID string
	if id, ierr := env.identity(); ierr == nil {
		nodeID = id.ID
	}

	result, err := repo.Sync(ctx, *message, ownedPaths(nodeID))
	if err != nil {
		if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, auth.ErrNotLoggedIn) {
			return fmt.Errorf("%w (run `nimbus login` first)", err)
		}
		return err
	}

	switch {
	case result.Offline:
		fmt.Fprintf(env.Out, "offline: %s\n", result.Detail)
	case result.Pushed && result.Reconciled:
		fmt.Fprintln(env.Out, "synced (reconciled with another device)")
	case result.Pushed:
		fmt.Fprintln(env.Out, "synced")
	case result.Detail != "":
		fmt.Fprintf(env.Out, "%s\n", result.Detail)
	default:
		fmt.Fprintln(env.Out, "nothing to sync")
	}
	return nil
}
