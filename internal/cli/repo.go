package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/nkoteb/nimbus/internal/auth"
)

// DefaultStateRepoName is the repository nimbus creates when none is named.
const DefaultStateRepoName = "nimbus-state"

func runRepo(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nimbus repo <create|show> [name]")
	}
	switch args[0] {
	case "create":
		return runRepoCreate(ctx, env, args[1:])
	case "show":
		return runRepoShow(ctx, env, args[1:])
	default:
		return fmt.Errorf("unknown repo subcommand %q", args[0])
	}
}

// ensureStateRepo finds or creates the remote state repo and returns its URL.
func ensureStateRepo(ctx context.Context, env *Env, name string, private bool) (string, bool, error) {
	if name == "" {
		name = DefaultStateRepoName
	}

	provider, err := providerFor("github")
	if err != nil {
		return "", false, err
	}
	gh, ok := provider.(*auth.GitHub)
	if !ok {
		return "", false, fmt.Errorf("provider %s cannot create repositories", provider.Name())
	}

	id, err := env.store().Get("github")
	if err != nil {
		return "", false, fmt.Errorf("not logged in (run `nimbus login` first)")
	}

	repo, created, err := gh.EnsureRepo(ctx, id, name, private)
	if err != nil {
		return "", false, err
	}
	return repo.CloneURL, created, nil
}

func runRepoCreate(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("repo create", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	public := fs.Bool("public", false, "create a public repository (private by default)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	name := DefaultStateRepoName
	if fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() > 1 {
		return errors.New("usage: nimbus repo create [name]")
	}

	// The state repo holds device profiles, audit trails, and encrypted
	// secrets, so private is the only sane default.
	url, created, err := ensureStateRepo(ctx, env, name, !*public)
	if err != nil {
		return err
	}

	if created {
		fmt.Fprintf(env.Out, "created %s\n", url)
	} else {
		fmt.Fprintf(env.Out, "already exists: %s\n", url)
	}
	fmt.Fprintf(env.Out, "\nrun `nimbus init --remote %s` to use it\n", url)
	return nil
}

func runRepoShow(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("repo show", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}

	name := DefaultStateRepoName
	if fs.NArg() == 1 {
		name = fs.Arg(0)
	}

	provider, err := providerFor("github")
	if err != nil {
		return err
	}
	gh := provider.(*auth.GitHub)

	id, err := env.store().Get("github")
	if err != nil {
		return fmt.Errorf("not logged in (run `nimbus login` first)")
	}

	repo, err := gh.FindRepo(ctx, id, name)
	if err != nil {
		return err
	}
	if repo == nil {
		return fmt.Errorf("no repository named %q (run `nimbus repo create`)", name)
	}

	fmt.Fprintf(env.Out, "name    %s\n", repo.FullName)
	fmt.Fprintf(env.Out, "clone   %s\n", repo.CloneURL)
	fmt.Fprintf(env.Out, "web     %s\n", repo.HTMLURL)
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	fmt.Fprintf(env.Out, "access  %s\n", visibility)
	return nil
}
