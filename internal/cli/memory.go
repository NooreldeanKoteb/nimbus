package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/nkoteb/nimbus/internal/memory"
	"github.com/nkoteb/nimbus/internal/task"
)

func runMemory(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nimbus memory <add|list|show|promote|forget|expire> [args]")
	}
	switch args[0] {
	case "add":
		return runMemoryAdd(ctx, env, args[1:])
	case "list", "search":
		return runMemoryList(ctx, env, args[1:])
	case "show":
		return runMemoryShow(ctx, env, args[1:])
	case "promote":
		return runMemoryPromote(ctx, env, args[1:])
	case "forget":
		return runMemoryForget(ctx, env, args[1:])
	case "expire":
		return runMemoryExpire(ctx, env, args[1:])
	default:
		return fmt.Errorf("unknown memory subcommand %q", args[0])
	}
}

func runMemoryAdd(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("memory add", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	long := fs.Bool("long-term", false, "remember this durably instead of as scratch")
	tags := fs.String("tags", "", "comma-separated tags")
	taskID := fs.String("task", "", "task this belongs to (default: the one held here)")
	ttl := fs.Duration("ttl", memory.DefaultTTL, "how long scratch survives")
	if err := fs.Parse(args); err != nil {
		return err
	}

	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		text = readPiped(env)
	}
	if text == "" {
		return errors.New("usage: nimbus memory add \"what to remember\"")
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}

	entry := &memory.Entry{
		Tier: memory.TierScratch,
		Text: text,
		Node: id.ID,
		Task: *taskID,
		Tags: splitTags(*tags),
	}
	if *long {
		entry.Tier = memory.TierLongTerm
	}
	// Default to whatever this device is working on, so a note taken mid-task
	// is findable later without the user having to remember to say so.
	if entry.Task == "" {
		if t, terr := task.Active(env.Paths.Repo, id.ID); terr == nil {
			entry.Task = t.ID
		}
	}

	saved, err := memory.Add(env.Paths.Repo, entry, *ttl)
	if err != nil {
		return err
	}
	_ = memory.Record(env.Paths.Repo, id.ID, memory.Event{
		Action: memory.ActionAdd, ID: saved.ID, Tier: saved.Tier, Text: saved.Summary(80),
	})

	fmt.Fprintf(env.Out, "remembered %s (%s)\n", saved.ID, saved.Tier)
	if saved.Tier == memory.TierScratch {
		fmt.Fprintf(env.Out, "expires %s — `nimbus memory promote %s` to keep it\n",
			saved.Expires.Local().Format("Jan 2"), saved.ID)
	}
	autoSync(ctx, env, repo, id.ID, "nimbus: remember "+saved.ID)
	return nil
}

func runMemoryList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("memory list", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	tier := fs.String("tier", "", "limit to scratch or long-term")
	taskID := fs.String("task", "", "limit to one task")
	mine := fs.Bool("mine", false, "only this device's scratch")
	if err := fs.Parse(args); err != nil {
		return err
	}

	autoRefresh(ctx, env)

	q := memory.Query{Tier: *tier, Task: *taskID, Text: strings.Join(fs.Args(), " ")}
	if *mine {
		if id, err := env.identity(); err == nil {
			q.Node = id.ID
		}
	}

	entries, err := memory.List(env.Paths.Repo, q)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(env.Out, "nothing remembered yet (`nimbus memory add \"...\"`)")
		return nil
	}

	for _, e := range entries {
		marker := " "
		if e.Tier == memory.TierLongTerm {
			// Long-term is the tier that survives, so it is worth seeing at a
			// glance which notes are about to disappear and which are not.
			marker = "*"
		}
		fmt.Fprintf(env.Out, "%s %-28s %-10s %s\n", marker, e.ID, e.Tier, e.Summary(60))
		if e.Task != "" || len(e.Tags) > 0 {
			fmt.Fprintf(env.Out, "  %-28s %s\n", "", describe(e))
		}
	}
	return nil
}

func runMemoryShow(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("memory show", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: nimbus memory show <id>")
	}

	autoRefresh(ctx, env)
	id, err := env.identity()
	if err != nil {
		return err
	}

	e, err := memory.Load(env.Paths.Repo, id.ID, fs.Arg(0))
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "id      %s\ntier    %s\nfrom    %s\n",
		e.ID, e.Tier, env.fleetLabels().Label(e.Node))
	if e.Task != "" {
		fmt.Fprintf(env.Out, "task    %s\n", e.Task)
	}
	if len(e.Tags) > 0 {
		fmt.Fprintf(env.Out, "tags    %s\n", strings.Join(e.Tags, ", "))
	}
	if !e.Expires.IsZero() {
		fmt.Fprintf(env.Out, "expires %s\n", e.Expires.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(env.Out, "\n%s\n", e.Text)
	return nil
}

func runMemoryPromote(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("memory promote", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: nimbus memory promote <id>")
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}

	e, err := memory.Promote(env.Paths.Repo, id.ID, fs.Arg(0))
	if err != nil {
		return err
	}
	_ = memory.Record(env.Paths.Repo, id.ID, memory.Event{
		Action: memory.ActionPromote, ID: e.ID, Tier: e.Tier, Text: e.Summary(80),
	})
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "memory.promote", e.ID, e.Summary(80), nil)
	}

	fmt.Fprintf(env.Out, "%s is now long-term and will not expire\n", e.ID)
	autoSync(ctx, env, repo, id.ID, "nimbus: promote memory "+e.ID)
	return nil
}

func runMemoryForget(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("memory forget", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: nimbus memory forget <id>")
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}

	e, err := memory.Forget(env.Paths.Repo, id.ID, fs.Arg(0))
	if err != nil {
		return err
	}
	// Journalled because a deletion is the one change the files themselves can
	// no longer explain.
	_ = memory.Record(env.Paths.Repo, id.ID, memory.Event{
		Action: memory.ActionForget, ID: e.ID, Tier: e.Tier, Text: e.Summary(80),
	})
	if log, lerr := env.auditLog(id.ID); lerr == nil {
		log.Record("local", "memory.forget", e.ID, e.Summary(80), nil)
	}

	fmt.Fprintf(env.Out, "forgot %s\n", e.ID)
	autoSync(ctx, env, repo, id.ID, "nimbus: forget memory "+e.ID)
	return nil
}

func runMemoryExpire(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("memory expire", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}

	swept, err := sweepExpired(env, id.ID)
	if err != nil {
		return err
	}
	if len(swept) == 0 {
		fmt.Fprintln(env.Out, "nothing has expired")
		return nil
	}

	for _, e := range swept {
		fmt.Fprintf(env.Out, "expired %s — %s\n", e.ID, e.Summary(60))
	}
	autoSync(ctx, env, repo, id.ID, fmt.Sprintf("nimbus: expire %d memories", len(swept)))
	return nil
}

// sweepExpired drops this device's stale scratch and journals each removal.
func sweepExpired(env *Env, nodeID string) ([]*memory.Entry, error) {
	swept, err := memory.Expire(env.Paths.Repo, nodeID)
	for _, e := range swept {
		_ = memory.Record(env.Paths.Repo, nodeID, memory.Event{
			Action: memory.ActionExpire, ID: e.ID, Tier: e.Tier, Text: e.Summary(80),
		})
	}
	return swept, err
}

func describe(e *memory.Entry) string {
	var parts []string
	if e.Task != "" {
		parts = append(parts, "task:"+e.Task)
	}
	if len(e.Tags) > 0 {
		parts = append(parts, strings.Join(e.Tags, " "))
	}
	if e.Tier == memory.TierScratch && !e.Expires.IsZero() {
		parts = append(parts, "expires "+e.Expires.Local().Format("Jan 2"))
	}
	return strings.Join(parts, "  ")
}

func splitTags(s string) []string {
	var tags []string
	for _, tag := range strings.Split(s, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}
