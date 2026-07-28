package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nkoteb/nimbus/internal/autonomy"
	"github.com/nkoteb/nimbus/internal/bus"
	"github.com/nkoteb/nimbus/internal/memory"
	"github.com/nkoteb/nimbus/internal/state"
	"github.com/nkoteb/nimbus/internal/system"
	"github.com/nkoteb/nimbus/internal/task"
)

// syncDisabled reports whether automatic syncing is turned off for this run.
// Automatic pushing is the default because a state repo that only syncs when
// asked is one that is stale exactly when another device needs it.
func syncDisabled() bool {
	v := os.Getenv("NIMBUS_NO_SYNC")
	return v != "" && v != "0" && v != "false"
}

// ownedPaths lists the files this device is authoritative for. They are
// reapplied on top of origin when history diverges, since no other device
// ever writes them.
//
// Task state is deliberately split: the per-device shards below are owned and
// survive a reconcile, while tasks/<id>/task.json is shared and takes origin's
// version. That is what makes a claim a real lease — two devices claiming at
// once, the one that pushes first keeps it.
func ownedPaths(nodeID string) []string {
	return []string{
		filepath.Join("nodes", nodeID+".json"),
		filepath.Join("audit", nodeID+".jsonl"),
		task.ProgressPattern(nodeID),
		task.SessionPattern(nodeID),
		// Scratch memory is per-device by design and expires on its own, so
		// this device is the only writer. Long-term memory is shared, but one
		// file per fact means concurrent additions land on different paths.
		memory.ScratchPattern(nodeID),
		memory.JournalPattern(nodeID),
		// A sender only ever writes its own messages, a receiver only ever its
		// own receipts. That split is what lets the bus run on git with no
		// merge strategy at all.
		bus.MessagePattern(nodeID),
		bus.ReceiptPattern(nodeID),
		// The output a device streams while running peer work is its own account
		// of what happened. Reconciling must never let another machine rewrite
		// it, or the transcript stops being evidence.
		bus.OutputPattern(nodeID),
		// A device's autonomy ceiling and its record of what it changed on
		// itself are both statements only that device can make. Another machine
		// reconciling must never be able to raise this one's level, or to drop
		// the rollback commands for changes it did not make.
		autonomy.Pattern(nodeID),
		system.Pattern(nodeID),
	}
}

// autoSync commits and pushes after a state-changing command.
//
// Sync problems are reported but never fail the caller: the work the user
// asked for already succeeded, and an unpushed commit is durable locally and
// will go out on the next run.
func autoSync(ctx context.Context, env *Env, repo *state.Repo, nodeID, message string) {
	if syncDisabled() {
		fmt.Fprintln(env.Out, "sync   skipped (NIMBUS_NO_SYNC)")
		return
	}

	result, err := repo.Sync(ctx, message, ownedPaths(nodeID))
	if err != nil {
		fmt.Fprintf(env.Out, "sync   failed: %v\n", err)
		fmt.Fprintln(env.Out, "       committed locally; run `nimbus state sync` to retry")
		return
	}

	switch {
	case result.Offline:
		fmt.Fprintf(env.Out, "sync   offline — %s\n", result.Detail)
	case result.Pushed && result.Reconciled:
		fmt.Fprintln(env.Out, "sync   pushed (reconciled with another device)")
	case result.Pushed:
		fmt.Fprintln(env.Out, "sync   pushed")
	case result.Detail != "":
		fmt.Fprintf(env.Out, "sync   %s\n", result.Detail)
	default:
		fmt.Fprintln(env.Out, "sync   nothing to push")
	}
}

// autoRefresh pulls before a read so the caller sees what other devices have
// published. Failure is silent: stale data beats a failed read.
func autoRefresh(ctx context.Context, env *Env) {
	if syncDisabled() {
		return
	}
	repo, err := state.Open(env.Paths.Repo, env.repoAuth())
	if err != nil {
		return
	}
	_ = repo.Refresh(ctx)
}
