// Package daemon keeps a device's state repo fresh in the background.
//
// Scope is deliberately narrow: it syncs and it reports. It does **not** run
// dispatched work unattended — that is the autonomy ladder in DESIGN.md §12,
// and shipping execution before the invariants exist would be exactly backwards.
// What it buys today is that a message sent from another device arrives without
// anyone remembering to type a command, which is the difference between a mesh
// and a pair of repos.
package daemon

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/nkoteb/nimbus/internal/bus"
	"github.com/nkoteb/nimbus/internal/state"
)

// DefaultInterval is how often the daemon syncs.
//
// A minute is short enough that a message feels like it arrived and long enough
// that an always-on device is not hammering the remote. A fetch with nothing to
// do is cheap, so the cost of being wrong on the low side is small.
const DefaultInterval = time.Minute

// MinInterval floors the configurable interval. Below this the daemon spends
// more time reconciling races with other devices than doing useful work.
const MinInterval = 10 * time.Second

// Options configures a run.
type Options struct {
	RepoPath string
	NodeID   string
	Owned    []string
	Interval time.Duration
	// Once runs a single cycle and returns, which is what makes the loop
	// testable without waiting on a timer.
	Once bool
	Out  io.Writer
	// OnStart runs after the first sync, before the loop settles. This is the
	// boot-resume hook (DESIGN.md §10): the daemon is what the service manager
	// starts at login, so it is the only thing present to notice that a task
	// was in flight when the machine went down.
	//
	// It runs after that sync rather than before, because whether a task is
	// still this device's to resume is a question only the remote can answer —
	// another machine may have taken it while this one was off.
	OnStart func(context.Context) error
}

// Result reports what one cycle observed.
type Result struct {
	Synced   bool
	Unread   int
	Offline  bool
	Detail   string
	NewSince []string
}

// Run syncs on an interval until the context is cancelled.
func Run(ctx context.Context, auth AuthFunc, opts Options) error {
	if opts.Interval < MinInterval {
		opts.Interval = DefaultInterval
	}

	seen := make(map[string]bool)
	// Prime from the current inbox so a daemon starting up does not announce
	// every message the user already read as though it just arrived.
	if messages, err := bus.Inbox(opts.RepoPath, opts.NodeID, true); err == nil {
		for _, e := range messages {
			seen[e.Message.ID] = true
		}
	}

	first := true
	for {
		result := cycle(ctx, auth, opts, seen)
		report(opts.Out, result)

		if first && opts.OnStart != nil {
			first = false
			// A failed boot resume must not stop the daemon syncing. Syncing is
			// the thing every other device depends on; resuming is this one's
			// convenience.
			if err := opts.OnStart(ctx); err != nil && opts.Out != nil {
				fmt.Fprintf(opts.Out, "%s boot resume: %v\n", time.Now().Format("15:04:05"), err)
			}
		}

		if opts.Once {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(withJitter(opts.Interval)):
		}
	}
}

// AuthFunc supplies git credentials at cycle time rather than once at startup,
// so a token refreshed while the daemon runs is picked up without a restart.
type AuthFunc func() (*state.Repo, error)

func cycle(ctx context.Context, auth AuthFunc, opts Options, seen map[string]bool) Result {
	var result Result

	repo, err := auth()
	if err != nil {
		result.Detail = err.Error()
		return result
	}

	sync, err := repo.Sync(ctx, "nimbus: daemon sync", opts.Owned)
	switch {
	case err != nil:
		result.Detail = err.Error()
	case sync.Offline:
		// Being offline is the normal state of a laptop, not an error worth
		// shouting about every minute.
		result.Offline = true
		result.Detail = sync.Detail
	default:
		result.Synced = true
	}

	messages, err := bus.Inbox(opts.RepoPath, opts.NodeID, false)
	if err != nil {
		return result
	}
	result.Unread = len(messages)
	for _, e := range messages {
		if seen[e.Message.ID] {
			continue
		}
		seen[e.Message.ID] = true
		result.NewSince = append(result.NewSince,
			fmt.Sprintf("%s: %s", e.Message.From, e.Message.Text))
	}
	return result
}

func report(out io.Writer, r Result) {
	if out == nil {
		return
	}
	stamp := time.Now().Format("15:04:05")

	switch {
	case r.Offline:
		fmt.Fprintf(out, "%s offline (%s)\n", stamp, r.Detail)
	case r.Detail != "":
		fmt.Fprintf(out, "%s error: %s\n", stamp, r.Detail)
	case len(r.NewSince) > 0:
		for _, m := range r.NewSince {
			fmt.Fprintf(out, "%s NEW MESSAGE %s\n", stamp, m)
		}
	case r.Synced && r.Unread > 0:
		fmt.Fprintf(out, "%s synced, %d unread\n", stamp, r.Unread)
	case r.Synced:
		fmt.Fprintf(out, "%s synced\n", stamp)
	}
}

// withJitter spreads sync times apart.
//
// Devices started by the same systemd unit at boot would otherwise line up on
// the same schedule, collide on every push, and spend their cycles reconciling
// each other rather than syncing.
func withJitter(interval time.Duration) time.Duration {
	spread := interval / 5
	if spread <= 0 {
		return interval
	}
	return interval - spread/2 + time.Duration(rand.Int63n(int64(spread)))
}
