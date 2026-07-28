package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
)

func runAudit(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	verify := fs.Bool("verify", false, "check the hash chain for tampering")
	limit := fs.Int("n", 20, "number of recent entries to show (0 for all)")
	node := fs.String("node", "", "read another device's log, by alias or id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	nodeID := *node
	if nodeID == "" {
		id, err := env.identity()
		if err != nil {
			return err
		}
		nodeID = id.ID
	} else {
		// Reading another device's log is only meaningful against fresh data.
		autoRefresh(ctx, env)
		resolved, err := env.resolveNode(nodeID)
		if err != nil {
			return err
		}
		nodeID = resolved
	}

	log, err := env.auditLog(nodeID)
	if err != nil {
		return err
	}

	label := env.fleetLabels().Label(nodeID)
	if *verify {
		if err := log.Verify(); err != nil {
			// Phrase this as the security finding it is, not a parse error.
			return fmt.Errorf("audit log for %s FAILED verification: %w", label, err)
		}
		entries, _ := log.Entries()
		fmt.Fprintf(env.Out, "audit log for %s verified: %d entries, chain intact\n", label, len(entries))
		return nil
	}

	entries, err := log.Entries()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("no audit entries for %s", label)
	}

	shown := entries
	if *limit > 0 && len(entries) > *limit {
		shown = entries[len(entries)-*limit:]
	}

	for _, e := range shown {
		mark := " "
		switch e.Result {
		case "error":
			mark = "!"
		case "skipped":
			mark = "="
		}
		fmt.Fprintf(env.Out, "%s %s  %-18s %-16s %s\n",
			mark,
			e.Timestamp.Format("2006-01-02 15:04:05"),
			truncate(e.Action, 18),
			truncate(e.Target, 16),
			truncate(e.Detail, 60))
		if e.Rollback != "" {
			fmt.Fprintf(env.Out, "    rollback: %s\n", e.Rollback)
		}
	}

	if len(shown) < len(entries) {
		fmt.Fprintf(env.Out, "\n(%d of %d entries; use -n 0 for all)\n", len(shown), len(entries))
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
