package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/nkoteb/nimbus/internal/auth"
	"github.com/nkoteb/nimbus/internal/state"
)

func fakeSystem(name, fullName string, age time.Duration) System {
	return System{
		Repo: auth.Repository{FullName: fullName, CloneURL: "https://github.com/" + fullName + ".git"},
		Marker: &state.Marker{
			Kind: state.MarkerKind, Version: state.MarkerVersion,
			ID: name + "-id", Name: name, Created: time.Now().Add(-age),
		},
	}
}

// One system is the overwhelmingly common case and must never prompt: a device
// that asks a question it already knows the answer to is a device that cannot
// be set up from a script.
func TestASingleSystemIsChosenSilently(t *testing.T) {
	systems := []System{fakeSystem("personal", "you/nimbus-state", time.Hour)}

	env := &Env{Out: &bytes.Buffer{}, Attended: true}
	chosen, err := selectSystem(env, systems, "")
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Name() != "personal" {
		t.Errorf("chose %s, want personal", chosen.Name())
	}
}

func TestASystemCanBeNamedDirectly(t *testing.T) {
	systems := []System{
		fakeSystem("personal", "you/nimbus-state", 48*time.Hour),
		fakeSystem("work", "you/nimbus-state-work", time.Hour),
	}
	env := &Env{Out: &bytes.Buffer{}}

	for _, ref := range []string{"work", "WORK", "work-id", "you/nimbus-state-work"} {
		chosen, err := selectSystem(env, systems, ref)
		if err != nil {
			t.Errorf("%q did not resolve: %v", ref, err)
			continue
		}
		if chosen.Name() != "work" {
			t.Errorf("%q resolved to %s, want work", ref, chosen.Name())
		}
	}

	// An unknown name lists what does exist rather than failing blankly.
	_, err := selectSystem(env, systems, "nonexistent")
	if err == nil {
		t.Fatal("an unknown system name was accepted")
	}
	for _, want := range []string{"personal", "work"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not list %q as an option: %v", want, err)
		}
	}
}

// Unattended, ambiguity is an error that names the options — never a guess and
// never a prompt into a pipe that nothing will answer.
func TestAmbiguityIsAnErrorWhenNobodyIsWatching(t *testing.T) {
	systems := []System{
		fakeSystem("personal", "you/nimbus-state", 48*time.Hour),
		fakeSystem("work", "you/nimbus-state-work", time.Hour),
	}

	env := &Env{Out: &bytes.Buffer{}, Attended: false, In: bytes.NewReader(nil)}
	_, err := selectSystem(env, systems, "")
	if err == nil {
		t.Fatal("two systems were resolved without anyone choosing")
	}
	if !strings.Contains(err.Error(), "--system") {
		t.Errorf("the error does not say how to choose: %v", err)
	}
}

// A prompt written to a pipe is a hang. init runs in installers and containers
// where nothing will ever type an answer.
func TestNoPromptWhenStdinIsNotATerminal(t *testing.T) {
	systems := []System{
		fakeSystem("personal", "you/nimbus-state", 48*time.Hour),
		fakeSystem("work", "you/nimbus-state-work", time.Hour),
	}

	// Attended, but stdin is a pipe rather than a terminal.
	env := &Env{Out: &bytes.Buffer{}, Attended: true, In: strings.NewReader("1\n")}
	if _, err := selectSystem(env, systems, ""); err == nil {
		t.Fatal("a prompt was issued to something that cannot answer it")
	}
}

func TestRepoFullNameHandlesEveryURLFormPeoplePaste(t *testing.T) {
	want := "NooreldeanKoteb/nimbus-state"
	for _, url := range []string{
		"https://github.com/NooreldeanKoteb/nimbus-state.git",
		"https://github.com/NooreldeanKoteb/nimbus-state",
		"git@github.com:NooreldeanKoteb/nimbus-state.git",
		"ssh://git@github.com/NooreldeanKoteb/nimbus-state",
		"  https://github.com/NooreldeanKoteb/nimbus-state.git  ",
	} {
		if got := repoFullName(url); got != want {
			t.Errorf("repoFullName(%q) = %q, want %q", url, got, want)
		}
	}

	// A URL nimbus cannot parse is not an error — the clone decides.
	for _, url := range []string{"", "https://gitlab.com/you/thing", "not a url"} {
		if got := repoFullName(url); got != "" {
			t.Errorf("repoFullName(%q) = %q, want empty", url, got)
		}
	}
}

// A repository name cannot contain spaces; a system name can.
func TestSystemNamesBecomeUsableRepoNames(t *testing.T) {
	cases := map[string]string{
		"default":      "nimbus-state-default",
		"work":         "nimbus-state-work",
		"Home Lab":     "nimbus-state-home-lab",
		"nimbus-state": "nimbus-state",
	}
	for system, want := range cases {
		if got := repoNameFor(system); got != want {
			t.Errorf("repoNameFor(%q) = %q, want %q", system, got, want)
		}
	}
}

// A fleet enrolled before markers existed has to end up with a readable name,
// or every upgraded system shows up in the picker as a URL fragment.
func TestNameIsDerivedFromAnUnmarkedRepo(t *testing.T) {
	cases := map[string]string{
		"https://github.com/you/nimbus-state.git":      "default",
		"https://github.com/you/nimbus-state-work.git": "work",
		"https://github.com/you/my-fleet.git":          "my-fleet",
		"":                                             "default",
	}
	for remote, want := range cases {
		if got := nameFromRemote(remote); got != want {
			t.Errorf("nameFromRemote(%q) = %q, want %q", remote, got, want)
		}
	}
}

// Discovery must not miss a system just because its topic was removed by hand,
// which is why the repo-name heuristic exists alongside the topic.
func TestUnmarkedButConventionallyNamedReposAreStillCandidates(t *testing.T) {
	if !looksLikeState("you/nimbus-state") || !looksLikeState("you/nimbus-state-work") {
		t.Error("a conventionally named state repo was not treated as a candidate")
	}
	if looksLikeState("you/some-project") {
		t.Error("an unrelated repo was treated as a candidate")
	}
}
