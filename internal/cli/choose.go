package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nkoteb/nimbus/internal/auth"
	"github.com/nkoteb/nimbus/internal/state"
)

// systemChoice is what the user asked for on the command line.
type systemChoice struct {
	Remote   string // an explicit URL, usually a link somebody shared
	Name     string // --system, when the account has several
	New      string // --new <name>
	RepoName string
	Create   bool // legacy --create-repo
	Offline  bool
}

// resolved is the outcome: which repo to use, and the marker if one is known.
type resolved struct {
	Remote string
	Marker *state.Marker
	// NewName is set when a system is being created and needs a marker written.
	NewName string
	Created bool
}

// chooseSystem decides which state repo this device should join.
//
// The ordering is what makes `nimbus init` with no arguments the normal case:
//
//	explicit URL      → use it, after checking it really is a state repo
//	already joined    → stay, so re-running init never re-asks
//	--new <name>      → create one
//	discovery         → one match is taken, several are offered, none is offered
//	                    as a creation
//
// Discovery is what removes `--remote <url>` from the second-device story. It
// is skipped entirely when the device is already set up, so the common case
// costs no API calls at all.
func chooseSystem(ctx context.Context, env *Env, want systemChoice) (resolved, error) {
	// An explicit link wins, and is verified before anything is written to it.
	if want.Remote != "" {
		marker, err := verifyRemote(ctx, env, want.Remote)
		if err != nil {
			return resolved{}, err
		}
		return resolved{Remote: want.Remote, Marker: marker}, nil
	}

	// Already on a system: re-running init must not re-ask or drift.
	if existing, err := state.Open(env.Paths.Repo, env.repoAuth()); err == nil {
		marker, _ := state.ReadMarker(env.Paths.Repo)
		if marker != nil || want.New == "" {
			return resolved{Remote: existing.RemoteURL(), Marker: marker, NewName: want.New}, nil
		}
	}

	name := want.New
	if name == "" && want.Create {
		// --create-repo predates named systems. Keep it working by naming the
		// system after the repository it was always going to create.
		name = want.RepoName
	}
	if name != "" {
		return createSystem(ctx, env, name, want.RepoName)
	}

	if want.Offline {
		return resolved{}, nil
	}

	systems, err := discoverSystems(ctx, env)
	switch {
	case errors.Is(err, ErrNoSystems):
		return offerToCreate(env)
	case err != nil:
		// Discovery is a convenience, not a prerequisite. A device with no
		// network still has to be able to set itself up locally.
		fmt.Fprintf(env.Out, "system could not check your account (%v)\n", err)
		return resolved{}, nil
	}

	chosen, err := selectSystem(env, systems, want.Name)
	if err != nil {
		return resolved{}, err
	}
	fmt.Fprintf(env.Out, "system joining %s at %s\n", chosen.Name(), chosen.Repo.FullName)
	return resolved{Remote: chosen.Repo.CloneURL, Marker: chosen.Marker}, nil
}

// verifyRemote refuses a URL that does not point at nimbus state.
//
// Without this, `--remote https://github.com/you/some-project` would clone that
// project and start writing device profiles, an audit log, and encrypted
// secrets into it — and the first sign of trouble would be a commit on somebody
// else's repository.
func verifyRemote(ctx context.Context, env *Env, remote string) (*state.Marker, error) {
	full := repoFullName(remote)
	if full == "" {
		return nil, nil // not a provider URL we can inspect; the clone decides
	}

	gh, id, err := env.provider()
	if err != nil {
		return nil, nil // not logged in: cloning will fail with its own message
	}

	data, err := gh.ReadFile(ctx, id, full, state.MarkerFile)
	if errors.Is(err, auth.ErrNoSuchFile) {
		// An empty repo that was just created has no marker yet, and neither
		// does a system from before markers existed. Both are legitimate, so
		// this reports rather than refuses.
		fmt.Fprintf(env.Out, "system %s has no %s yet — it will be marked as one\n", full, state.MarkerFile)
		return nil, nil
	}
	if err != nil {
		return nil, nil
	}

	marker, perr := state.ParseMarker(data)
	if perr != nil {
		return nil, fmt.Errorf("%s is not a nimbus system: %w\n"+
			"joining it would write device profiles and an audit trail into it", full, perr)
	}
	return marker, nil
}

// createSystem provisions a new system on the provider.
func createSystem(ctx context.Context, env *Env, name, repoName string) (resolved, error) {
	if err := state.ValidateSystemName(name); err != nil {
		return resolved{}, err
	}
	if repoName == "" || repoName == DefaultStateRepoName {
		repoName = repoNameFor(name)
	}

	url, created, err := ensureStateRepo(ctx, env, repoName, true)
	if err != nil {
		return resolved{}, err
	}
	if created {
		fmt.Fprintf(env.Out, "system created %s at %s\n", name, url)
	} else {
		fmt.Fprintf(env.Out, "system using existing %s\n", url)
	}
	return resolved{Remote: url, NewName: name, Created: created}, nil
}

// offerToCreate handles the genuine first run: no systems anywhere.
func offerToCreate(env *Env) (resolved, error) {
	fmt.Fprintln(env.Out, "system none found on this account")
	fmt.Fprintln(env.Out, "       `nimbus init --new <name>` to start one,")
	fmt.Fprintln(env.Out, "       or `nimbus init --remote <url>` to join one somebody shared")
	fmt.Fprintln(env.Out, "       continuing with a local-only state repo for now")
	return resolved{}, nil
}

// repoNameFor turns a system name into a repository name, since a repository
// cannot contain spaces and a system name can.
func repoNameFor(system string) string {
	slug := strings.ToLower(strings.TrimSpace(system))
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r == ' ':
			return '-'
		default:
			return -1
		}
	}, slug)
	if slug == "" || slug == DefaultStateRepoName {
		return DefaultStateRepoName
	}
	return DefaultStateRepoName + "-" + slug
}

// repoFullName extracts owner/name from a provider URL.
func repoFullName(remote string) string {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	for _, prefix := range []string{
		"https://github.com/", "http://github.com/", "git@github.com:", "ssh://git@github.com/",
	} {
		if rest, ok := strings.CutPrefix(remote, prefix); ok {
			if owner, name, found := strings.Cut(rest, "/"); found && owner != "" && name != "" {
				return owner + "/" + name
			}
		}
	}
	return ""
}

// ensureMarker writes the marker for a new system and backfills one for a
// system that predates markers, then makes it discoverable.
//
// Backfilling matters more than it looks: without it, every fleet enrolled
// before this feature stays invisible to discovery forever, and the second
// device still needs a URL somebody has to go and find.
func ensureMarker(ctx context.Context, env *Env, repo *state.Repo, choice resolved) (*state.Marker, error) {
	if existing, err := state.ReadMarker(env.Paths.Repo); err == nil {
		markDiscoverable(ctx, env, repo)
		return existing, nil
	}

	name := choice.NewName
	if choice.Marker != nil {
		// The remote has one but this checkout does not yet — a clone that
		// happened before the marker was pushed. Adopt rather than invent.
		return choice.Marker, nil
	}
	if name == "" {
		name = nameFromRemote(repo.RemoteURL())
	}

	id, err := env.identity()
	if err != nil {
		return nil, err
	}
	marker, err := state.NewMarker(name, id.ID)
	if err != nil {
		return nil, err
	}
	if err := marker.Save(env.Paths.Repo); err != nil {
		return nil, err
	}
	markDiscoverable(ctx, env, repo)
	return marker, nil
}

// markDiscoverable sets the provider topic so this system shows up on the next
// device without anybody pasting a URL.
//
// Best effort throughout: a token that cannot write topics, a repo owned by
// somebody else, or no network at all must not fail setup over a search hint.
// The marker file is the authority; this only makes finding it cheap.
func markDiscoverable(ctx context.Context, env *Env, repo *state.Repo) {
	full := repoFullName(repo.RemoteURL())
	if full == "" {
		return
	}
	gh, id, err := env.provider()
	if err != nil {
		return
	}

	existing, err := gh.FindRepo(ctx, id, full)
	if err != nil || existing == nil || existing.HasTopic(state.Topic) {
		return
	}
	_ = gh.SetTopics(ctx, id, full, append(existing.Topics, state.Topic))
}

// nameFromRemote derives a system name for a repo that never had one.
func nameFromRemote(remote string) string {
	full := repoFullName(remote)
	if full == "" {
		return "default"
	}
	_, name, _ := strings.Cut(full, "/")
	// nimbus-state → default; nimbus-state-work → work. The prefix is nimbus's
	// own convention and carries no information for a person choosing between
	// two systems.
	if trimmed, ok := strings.CutPrefix(name, DefaultStateRepoName+"-"); ok && trimmed != "" {
		return trimmed
	}
	if name == DefaultStateRepoName {
		return "default"
	}
	return name
}
