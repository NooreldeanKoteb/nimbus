package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/nkoteb/nimbus/internal/auth"
	"github.com/nkoteb/nimbus/internal/state"
)

// System is one state repo the account can reach, with the marker that proves
// it is one.
type System struct {
	Repo   auth.Repository
	Marker *state.Marker
}

// Name is what a person selects by.
func (s System) Name() string { return s.Marker.Name }

// ErrNoSystems means the account has none, which is the first-run case rather
// than a failure.
var ErrNoSystems = errors.New("no nimbus systems found on this account")

// discoverSystems finds every state repo the token can reach.
//
// Two stages, because the cheap one is not authoritative. The topic narrows
// hundreds of repositories to a handful without opening any of them; the marker
// file then decides, because a topic can be removed by hand and a repo shared
// as a link may never have carried one.
func discoverSystems(ctx context.Context, env *Env) ([]System, error) {
	provider, id, err := env.provider()
	if err != nil {
		return nil, err
	}

	repos, err := provider.ListRepos(ctx, id)
	if err != nil {
		return nil, err
	}

	var found []System
	for _, repo := range repos {
		if !repo.HasTopic(state.Topic) && !looksLikeState(repo.FullName) {
			continue
		}
		data, rerr := provider.ReadFile(ctx, id, repo.FullName, state.MarkerFile)
		if rerr != nil {
			continue
		}
		marker, perr := state.ParseMarker(data)
		if perr != nil {
			continue
		}
		found = append(found, System{Repo: repo, Marker: marker})
	}

	// Oldest first: the system somebody has been using longest is the one they
	// most likely mean, and a stable order keeps the numbers in the picker from
	// moving between runs.
	sort.Slice(found, func(i, j int) bool {
		return found[i].Marker.Created.Before(found[j].Marker.Created)
	})
	if len(found) == 0 {
		return nil, ErrNoSystems
	}
	return found, nil
}

// looksLikeState catches repos created before topics were set, so an existing
// fleet is still discovered after an upgrade rather than appearing to vanish.
func looksLikeState(fullName string) bool {
	_, name, _ := strings.Cut(fullName, "/")
	return strings.HasPrefix(name, auth.StateRepoName)
}

// selectSystem resolves which system to use.
//
// The rules are ordered so that automation never blocks: an explicit name wins,
// a single match is taken silently, and only a genuine ambiguity in front of a
// person becomes a prompt. Unattended, ambiguity is an error that lists the
// options rather than a guess.
func selectSystem(env *Env, systems []System, wanted string) (*System, error) {
	if wanted != "" {
		var matches []System
		for _, s := range systems {
			if strings.EqualFold(s.Name(), wanted) || s.Marker.ID == wanted ||
				strings.EqualFold(s.Repo.FullName, wanted) {
				matches = append(matches, s)
			}
		}
		switch len(matches) {
		case 1:
			return &matches[0], nil
		case 0:
			return nil, fmt.Errorf("no system named %q\n%s", wanted, listSystems(systems))
		default:
			return nil, fmt.Errorf("%q matches more than one system\n%s", wanted, listSystems(systems))
		}
	}

	if len(systems) == 1 {
		return &systems[0], nil
	}

	if !env.Attended || !stdinIsTerminal(env) {
		return nil, fmt.Errorf("this account has %d systems — name one with --system\n%s",
			len(systems), listSystems(systems))
	}
	return promptForSystem(env, systems)
}

func listSystems(systems []System) string {
	var b strings.Builder
	for i, s := range systems {
		fmt.Fprintf(&b, "  %d. %-24s %s\n", i+1, s.Name(), s.Repo.FullName)
	}
	return b.String()
}

// promptForSystem asks which one, by number or by name.
func promptForSystem(env *Env, systems []System) (*System, error) {
	fmt.Fprintf(env.Out, "\nthis account has %d nimbus systems:\n\n", len(systems))
	for i, s := range systems {
		fmt.Fprintf(env.Out, "  %d. %-24s %s  (created %s)\n",
			i+1, s.Name(), s.Repo.FullName, s.Marker.Created.Format("2006-01-02"))
	}
	fmt.Fprintf(env.Out, "\nwhich one? [1-%d, or a name] ", len(systems))

	answer, err := readLine(env)
	if err != nil {
		return nil, err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil, errors.New("no system selected")
	}

	if n, cerr := strconv.Atoi(answer); cerr == nil {
		if n < 1 || n > len(systems) {
			return nil, fmt.Errorf("%d is not one of the %d systems listed", n, len(systems))
		}
		return &systems[n-1], nil
	}
	return selectSystem(env, systems, answer)
}

// stdinIsTerminal reports whether there is a person to answer a prompt.
//
// A prompt written to a pipe is a hang, and `nimbus init` runs in installers
// and containers where nothing will ever type an answer.
func stdinIsTerminal(env *Env) bool {
	f, ok := env.In.(*os.File)
	if !ok {
		if env.In == nil {
			f = os.Stdin
		} else {
			return false
		}
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func readLine(env *Env) (string, error) {
	var r io.Reader = env.In
	if r == nil {
		r = os.Stdin
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// runSystems lists the systems this account can reach.
func runSystems(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("systems", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}

	systems, err := discoverSystems(ctx, env)
	if errors.Is(err, ErrNoSystems) {
		fmt.Fprintln(env.Out, "no systems on this account yet")
		fmt.Fprintln(env.Out, "create one with `nimbus init --new <name>`, or join one with `nimbus init --remote <url>`")
		return nil
	}
	if err != nil {
		return err
	}

	// Which one this device is actually on, so a listing answers "where am I"
	// as well as "what exists".
	current, _ := state.ReadMarker(env.Paths.Repo)
	for _, s := range systems {
		marker := " "
		if current != nil && current.ID == s.Marker.ID {
			marker = "*"
		}
		fmt.Fprintf(env.Out, "%s %-24s %-40s created %s\n",
			marker, s.Name(), s.Repo.FullName, s.Marker.Created.Format("2006-01-02"))
	}
	if current == nil {
		fmt.Fprintln(env.Out, "\nthis device is not on any of them — `nimbus init` to join one")
	}
	return nil
}
