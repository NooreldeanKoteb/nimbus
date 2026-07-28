package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nkoteb/nimbus/internal/auth"
	"github.com/nkoteb/nimbus/internal/browser"
	"golang.org/x/term"
)

// defaultGitHubClientID is the OAuth app nimbus authenticates through.
//
// Client IDs are public by design in the device flow — there is no client
// secret, so baking one into the binary is safe and is what gh does. Until an
// app is registered this is empty and `nimbus login` uses the token flow,
// which needs no app at all.
const defaultGitHubClientID = ""

func providerFor(name string) (auth.Provider, error) {
	switch name {
	case "github", "":
		clientID := os.Getenv("NIMBUS_GITHUB_CLIENT_ID")
		if clientID == "" {
			clientID = defaultGitHubClientID
		}
		gh := &auth.GitHub{ClientID: clientID}
		if base := os.Getenv("NIMBUS_GITHUB_BASE_URL"); base != "" {
			gh.BaseURL = base
		}
		if api := os.Getenv("NIMBUS_GITHUB_API_URL"); api != "" {
			gh.APIURL = api
		}
		return gh, nil
	default:
		return nil, fmt.Errorf("unknown provider %q (supported: github)", name)
	}
}

func (e *Env) store() *auth.Store {
	return &auth.Store{Path: e.Paths.CredentialsFile()}
}

func runLogin(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	providerName := fs.String("provider", "github", "identity provider to authenticate with")
	force := fs.Bool("force", false, "re-authenticate even if already logged in")
	useToken := fs.Bool("token", false, "paste a personal access token instead of using the device flow")
	noBrowser := fs.Bool("no-browser", false, "print URLs instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	provider, err := providerFor(*providerName)
	if err != nil {
		return err
	}

	if err := env.Paths.EnsureDirs(); err != nil {
		return err
	}
	store := env.store()

	if !*force {
		if existing, err := store.Get(provider.Name()); err == nil {
			if err := provider.Validate(ctx, existing); err == nil {
				fmt.Fprintf(env.Out, "already logged in to %s as %s (use --force to re-authenticate)\n",
					existing.Provider, existing.Login)
				return nil
			}
			// Stored token is stale; fall through and re-authenticate.
		}
	}

	gh, isGitHub := provider.(*auth.GitHub)

	// The device flow needs a registered OAuth app. Without one, fall back to
	// the token flow rather than failing: the user still gets logged in, and
	// the difference is one paste.
	switch {
	case *useToken && isGitHub:
		return loginWithToken(ctx, env, gh, store, *noBrowser)
	case isGitHub && gh.ClientID == "":
		fmt.Fprintln(env.Out, "No OAuth app configured, using a personal access token instead.")
		fmt.Fprintln(env.Out, "(Register an app and set NIMBUS_GITHUB_CLIENT_ID for one-click login.)")
		fmt.Fprintln(env.Out)
		return loginWithToken(ctx, env, gh, store, *noBrowser)
	}

	return loginWithDeviceFlow(ctx, env, provider, store, *noBrowser)
}

func loginWithDeviceFlow(ctx context.Context, env *Env, provider auth.Provider, store *auth.Store, noBrowser bool) error {
	prompt := func(v auth.Verification) {
		fmt.Fprintf(env.Out, "\n  Your code:  %s\n\n", v.UserCode)

		opened := false
		if !noBrowser && browser.Available() {
			if err := browser.Open(v.VerificationURI); err == nil {
				opened = true
			}
		}
		if opened {
			fmt.Fprintf(env.Out, "  Opened %s in your browser.\n", v.VerificationURI)
			fmt.Fprintln(env.Out, "  Enter the code above to authorize this device.")
		} else {
			fmt.Fprintf(env.Out, "  Open %s and enter the code above.\n", v.VerificationURI)
		}
		fmt.Fprintf(env.Out, "\n  Waiting for approval (expires in %s)...\n", v.ExpiresIn.Round(1e9))
	}

	id, err := provider.Login(ctx, prompt)
	switch {
	case errors.Is(err, auth.ErrDenied):
		return errors.New("login denied in browser")
	case errors.Is(err, auth.ErrExpired):
		return errors.New("login code expired; run `nimbus login` again")
	case err != nil:
		return err
	}

	return finishLogin(env, store, id)
}

func loginWithToken(ctx context.Context, env *Env, gh *auth.GitHub, store *auth.Store, noBrowser bool) error {
	tokenURL := gh.TokenPageURL()

	opened := false
	if !noBrowser && browser.Available() {
		if err := browser.Open(tokenURL); err == nil {
			opened = true
		}
	}

	if opened {
		fmt.Fprintln(env.Out, "  Opened GitHub's token page in your browser, with the right")
		fmt.Fprintln(env.Out, "  permissions already selected.")
	} else {
		fmt.Fprintln(env.Out, "  Open this page (the right permissions are pre-selected):")
		fmt.Fprintf(env.Out, "\n    %s\n", tokenURL)
	}
	fmt.Fprintln(env.Out, "\n  Scroll down, click \"Generate token\", then paste it here.")
	fmt.Fprint(env.Out, "\n  Token: ")

	token, err := readSecret()
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Out)

	id, err := gh.LoginWithToken(ctx, token)
	if err != nil {
		return err
	}
	return finishLogin(env, store, id)
}

// readSecret reads a line from stdin without echoing it when attached to a
// terminal, so a pasted token does not linger on screen or in scrollback.
func readSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		data, err := term.ReadPassword(fd)
		if err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}

	// Piped input: read one line so `echo $TOKEN | nimbus login --token` works.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read token: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func finishLogin(env *Env, store *auth.Store, id *auth.Identity) error {
	if err := store.Save(id); err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "\n  Logged in to %s as %s\n", id.Provider, id.Login)
	if len(id.Scopes) > 0 {
		fmt.Fprintf(env.Out, "  Scopes: %s\n", strings.Join(id.Scopes, ", "))
	}
	fmt.Fprintln(env.Out, "\nnext:  nimbus init --create-repo")
	return nil
}

func runLogout(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	providerName := fs.String("provider", "github", "identity provider to sign out of")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store := env.store()
	if _, err := store.Get(*providerName); errors.Is(err, auth.ErrNotLoggedIn) {
		fmt.Fprintf(env.Out, "not logged in to %s\n", *providerName)
		return nil
	}
	if err := store.Delete(*providerName); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "logged out of %s\n", *providerName)
	return nil
}

func runWhoami(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	verify := fs.Bool("verify", false, "check the stored token against the provider")
	if err := fs.Parse(args); err != nil {
		return err
	}

	names, err := env.store().List()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("not logged in (run `nimbus login`)")
	}

	for _, name := range names {
		id, err := env.store().Get(name)
		if err != nil {
			continue
		}
		status := ""
		if *verify {
			provider, perr := providerFor(name)
			if perr != nil {
				status = "  [unknown provider]"
			} else if verr := provider.Validate(ctx, id); verr != nil {
				status = "  [token invalid]"
			} else {
				status = "  [ok]"
			}
		}
		fmt.Fprintf(env.Out, "%s: %s%s\n", id.Provider, id.Login, status)
	}
	return nil
}
