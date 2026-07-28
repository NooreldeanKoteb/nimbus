package daemon

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/nkoteb/nimbus/internal/state"
)

// noRepo stands in for a device that cannot reach its remote. The work hook has
// to run anyway: a peer request that already arrived is still there to answer,
// and a laptop that is offline is the normal case rather than a broken one.
func noRepo() (*state.Repo, error) { return nil, errors.New("offline") }

// A single cycle must finish its work before returning. Backgrounding it there
// would mean the process exits mid-command — the goroutine would be killed with
// the request already acknowledged and no result ever sent.
func TestOneShotRunsWorkBeforeReturning(t *testing.T) {
	done := false
	err := Run(context.Background(), noRepo, Options{
		RepoPath: t.TempDir(), NodeID: "node-a", Once: true, Out: io.Discard,
		Work: func(context.Context) error {
			done = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Error("the daemon returned before the work it started had run")
	}
}

// A daemon that only syncs is the default. Running other devices' work is
// something somebody turns on, never something that follows from installing it.
func TestNoWorkHookMeansNoWork(t *testing.T) {
	err := Run(context.Background(), noRepo, Options{
		RepoPath: t.TempDir(), NodeID: "node-a", Once: true, Out: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A failing work hook must not stop the loop. Syncing is what every other
// device depends on; doing their work is this one's contribution.
func TestFailingWorkDoesNotStopTheDaemon(t *testing.T) {
	err := Run(context.Background(), noRepo, Options{
		RepoPath: t.TempDir(), NodeID: "node-a", Once: true, Out: io.Discard,
		Work: func(context.Context) error { return errors.New("boom") },
	})
	if err != nil {
		t.Errorf("a failing work hook failed the daemon: %v", err)
	}
}

// Boot resume goes first on the cycle it runs: work this device left in flight
// has a stronger claim on it than work somebody else is offering.
func TestBootResumeRunsBeforePeerWork(t *testing.T) {
	var order []string
	err := Run(context.Background(), noRepo, Options{
		RepoPath: t.TempDir(), NodeID: "node-a", Once: true, Out: io.Discard,
		OnStart: func(context.Context) error {
			order = append(order, "boot")
			return nil
		},
		Work: func(context.Context) error {
			order = append(order, "work")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "boot" {
		t.Errorf("ran %v, want boot resume before peer work", order)
	}
}
