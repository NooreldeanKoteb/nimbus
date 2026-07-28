// Package daemon keeps a device's state repo fresh in the background, and —
// once the invariants existed to make it safe — runs the work other devices ask
// it for.
//
// Syncing came first on purpose: shipping unattended execution before the
// autonomy ladder (DESIGN.md §12) would have been exactly backwards. The Work
// hook is opt-in for the same reason, and everything it may do is decided by
// this device's own policy rather than by whoever sent the request.
//
// Work runs in the background rather than inline. A dispatched session can take
// half an hour, and a daemon that stopped syncing for its duration would look
// dead to the rest of the fleet — long enough for another device to decide the
// claim had lapsed and steal the task out from under a machine that was busy
// doing it. Syncing through the run is also what makes streamed output arrive
// elsewhere while the command is still going.
package daemon

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync/atomic"
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
	// Work drains whatever peers have asked this device to do. Nil means the
	// daemon only syncs, which is the default: a device runs other people's
	// work because somebody turned that on, never because it was installed.
	//
	// It must not sync itself — this loop owns the repo, and a second git
	// operation on the same worktree at the same time is a corrupt index.
	// Writing files and letting the next cycle push them is both simpler and
	// what makes output stream out during a long run.
	Work func(context.Context) error
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

	// busy is what keeps one worker at a time. Without it a cycle every minute
	// would start a second worker on the same inbox before the first had
	// acknowledged anything, and the same command would run twice.
	var busy atomic.Bool

	first := true
	for {
		result := cycle(ctx, auth, opts, seen)
		report(opts.Out, result)

		// Boot resume goes first on the cycle it runs: work this device left in
		// flight has a stronger claim on it than work somebody else is offering.
		if first && opts.OnStart != nil {
			first = false
			// A failed boot resume must not stop the daemon syncing. Syncing is
			// the thing every other device depends on; resuming is this one's
			// convenience.
			if err := opts.OnStart(ctx); err != nil && opts.Out != nil {
				fmt.Fprintf(opts.Out, "%s boot resume: %v\n", time.Now().Format("15:04:05"), err)
			}
		}

		if opts.Work != nil && busy.CompareAndSwap(false, true) {
			if opts.Once {
				// A single cycle has no later sync to carry the results, and a
				// goroutine outliving this function would be killed mid-command.
				drain(ctx, opts, &busy)
			} else {
				go drain(ctx, opts, &busy)
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

// drain runs the work hook and releases the busy flag whatever happens, so one
// panicking or erroring run cannot wedge the device into never working again.
func drain(ctx context.Context, opts Options, busy *atomic.Bool) {
	defer busy.Store(false)
	if err := opts.Work(ctx); err != nil && opts.Out != nil {
		fmt.Fprintf(opts.Out, "%s work: %v\n", time.Now().Format("15:04:05"), err)
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
