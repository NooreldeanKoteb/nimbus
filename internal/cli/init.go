package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nkoteb/nimbus/internal/audit"
	"github.com/nkoteb/nimbus/internal/device"
	"github.com/nkoteb/nimbus/internal/install"
	"github.com/nkoteb/nimbus/internal/state"
)

func runInit(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	remote := fs.String("remote", "", "state repo URL to join (a link someone shared)")
	system := fs.String("system", "", "which system to join, when the account has several")
	newSystem := fs.String("new", "", "create a new system with this name")
	createRepo := fs.Bool("create-repo", false, "create the state repo if absent (same as --new)")
	repoName := fs.String("repo-name", DefaultStateRepoName, "repository name for a system being created")
	alias := fs.String("alias", "", "name for this device (default: derived from hostname)")
	skipClaude := fs.Bool("skip-claude", false, "do not install Claude Code")
	noSudo := fs.Bool("no-sudo", false, "never prompt for administrator access")
	offline := fs.Bool("offline", false, "skip provider discovery and work with the local repo only")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := env.Paths.EnsureDirs(); err != nil {
		return err
	}

	id, err := env.identity()
	if err != nil {
		return err
	}
	if *alias != "" {
		if err := id.SetAlias(env.Paths.IdentityFile(), *alias); err != nil {
			return err
		}
	}
	fmt.Fprintf(env.Out, "device %s (%s)\n\n", id.Label(), id.Hostname)

	// 1. Choose a system. On a device that already has one this is a no-op; on
	//    a new device it is the whole point — nobody should have to remember a
	//    repository URL to sit down at a machine.
	choice, err := chooseSystem(ctx, env, systemChoice{
		Remote: *remote, Name: *system, New: *newSystem,
		RepoName: *repoName, Create: *createRepo, Offline: *offline,
	})
	if err != nil {
		return err
	}

	repo, err := setupRepo(ctx, env, choice.Remote)
	if err != nil {
		return err
	}

	// 2. Stamp the marker. A system created before markers existed has none, so
	//    this both writes it for new systems and backfills old ones, which is
	//    what keeps an existing fleet discoverable after an upgrade.
	marker, err := ensureMarker(ctx, env, repo, choice)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "system %s\n", marker.Label())

	log, err := env.auditLog(id.ID)
	if err != nil {
		return err
	}
	log.Record("local", "init.start", id.ID, "nimbus init", nil)

	// 3. Manifest — seeded with a default on a brand-new fleet.
	manifest, err := install.LoadManifest(env.Paths.ManifestFile())
	if err != nil {
		return err
	}
	_, statErr := os.Stat(env.Paths.ManifestFile())
	fresh := errors.Is(statErr, os.ErrNotExist)
	// A fleet enrolled before the MCP server existed has a manifest without it,
	// and LoadManifest only falls back to the default when there is no file at
	// all — so an upgrade has to add it explicitly or those devices never get
	// the mesh tools.
	added := manifest.EnsureSelfRegistered()

	if fresh || added {
		if err := manifest.Save(env.Paths.ManifestFile()); err != nil {
			return err
		}
		switch {
		case fresh:
			fmt.Fprintf(env.Out, "  + manifest    created default\n")
		default:
			fmt.Fprintf(env.Out, "  + manifest    registered the nimbus mcp server\n")
		}
	}

	// 4. Adopt before linking. On the first device this captures an existing
	//    ~/.claude into the repo; on later devices it is a no-op.
	localClaude := localClaudeDir()
	repoClaude := env.Paths.RepoClaudeDir()
	if err := os.MkdirAll(repoClaude, 0o755); err != nil {
		return err
	}

	fmt.Fprintln(env.Out, "config")
	report(env, log, install.AdoptClaudeConfig(localClaude, repoClaude, manifest.LinkConfig))
	// The hook goes into the repo's settings.json before linking, so a device
	// that had no settings file still ends up with one to link.
	report(env, log, []install.Step{
		install.EnsureSessionHook(filepath.Join(repoClaude, "settings.json"), install.SessionHookCommand),
	})
	report(env, log, install.LinkClaudeConfig(repoClaude, localClaude, manifest.LinkConfig))

	// 5. Claude Code, then the MCP servers that depend on it. Its installer
	//    has its own prerequisites, which a bare device will not have.
	if !*skipClaude {
		fmt.Fprintln(env.Out, "\nclaude code")
		pkgMgr := device.DetectPackageManager()

		// Elevate only when something actually needs installing, so a
		// second run never prompts for a password it will not use.
		priv := install.DetectPrivilege()
		if priv == install.PrivNone && !*noSudo && install.NeedsPrereqs() {
			elevated, eerr := install.Elevate(ctx)
			if eerr != nil {
				fmt.Fprintf(env.Out, "  ! elevation    %v\n", eerr)
				log.Record("local", "elevate", id.ID, "administrator access", eerr)
			} else {
				priv = elevated
				log.Record("local", "elevate", id.ID, "obtained "+string(elevated), nil)
			}
		}
		stop := install.KeepAlive(ctx, priv)
		defer stop()

		report(env, log, install.EnsureClaudePrereqs(ctx, pkgMgr))
		report(env, log, []install.Step{install.InstallClaudeCode(ctx, manifest.ClaudeCode)})
	}

	if len(manifest.MCPServers) > 0 {
		fmt.Fprintln(env.Out, "\nmcp servers")
		report(env, log, install.InstallMCPServers(ctx, manifest.MCPServers))
	}

	// 6. Publish this device's profile so the fleet can see it.
	fmt.Fprintln(env.Out, "\nprofile")
	profile := device.Detect(ctx, id, nil)
	path, err := profile.Save(env.Paths.Repo)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "  + published   %s as %s\n", filepath.Base(path), id.Label())
	log.Record("local", "doctor.publish", id.ID, "device profile published", nil)

	// 7. Commit and push. The audit record is written before syncing so the
	//    sync itself carries a complete log of the run.
	log.Record("local", "init.done", id.ID, "nimbus init complete", nil)
	fmt.Fprintln(env.Out)
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: init %s", id.ID))

	fmt.Fprintf(env.Out, "\nready. %d device(s) known.\n", fleetSize(env))
	if repo.RemoteURL() == "" {
		fmt.Fprintln(env.Out, "no origin set — run `nimbus init --create-repo` to make this portable")
	}
	return nil
}

func setupRepo(ctx context.Context, env *Env, remote string) (*state.Repo, error) {
	if existing, err := state.Open(env.Paths.Repo, env.repoAuth()); err == nil {
		if remote != "" {
			if err := existing.SetRemote(remote); err != nil {
				return nil, err
			}
		}
		fmt.Fprintf(env.Out, "state  %s (existing)\n", existing.Path)
		return existing, nil
	}

	if remote != "" {
		fmt.Fprintf(env.Out, "state  cloning %s\n", remote)
		repo, err := state.Clone(ctx, remote, env.Paths.Repo, env.repoAuth())
		if err != nil {
			return nil, err
		}
		return repo, nil
	}

	repo, err := state.Init(env.Paths.Repo, env.repoAuth())
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(env.Out, "state  %s (new)\n", repo.Path)
	return repo, nil
}

// report prints and audits a batch of install steps. A failed step is reported
// but does not abort: partial setup that says what failed beats no setup.
func report(env *Env, log *audit.Log, steps []install.Step) {
	for _, s := range steps {
		switch {
		case s.Err != nil:
			fmt.Fprintf(env.Out, "  ! %-13s %v\n", s.Name, s.Err)
			log.Record("local", "install", s.Name, s.Detail, s.Err)
		case s.Changed:
			fmt.Fprintf(env.Out, "  + %-13s %s\n", s.Name, s.Detail)
			// Rollback belongs in the record even on success — especially on
			// success, since that is the change someone may need to undo.
			_, _ = log.Append(audit.Entry{
				Actor: "local", Action: "install", Target: s.Name,
				Detail: s.Detail, Rollback: s.Rollback,
			})
		case s.Skipped:
			fmt.Fprintf(env.Out, "  = %-13s %s\n", s.Name, s.Detail)
		}
	}
}

func localClaudeDir() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

func fleetSize(env *Env) int {
	fleet, err := device.LoadFleet(env.Paths.Repo)
	if err != nil {
		return 0
	}
	return len(fleet)
}
