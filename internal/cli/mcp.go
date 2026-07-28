package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/nkoteb/nimbus/internal/mcp"
	"github.com/nkoteb/nimbus/internal/version"
)

// runMCP serves the Nimbus MCP server on stdio.
//
// This is how Claude reaches the mesh without shelling out — it is registered
// in the manifest, so it installs on every device that runs `nimbus init`.
func runMCP(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}

	server := &mcp.Server{
		Name:    "nimbus",
		Version: version.String(),
		Tools:   tools(env),
	}
	// stdout is the protocol stream and nothing else; env.Out would corrupt it.
	return server.Serve(ctx, os.Stdin, os.Stdout)
}

// invoke runs a nimbus command and captures what it printed.
//
// The MCP tools are deliberately thin shims over the same commands a person
// runs, rather than a parallel implementation against the packages underneath.
// One code path means the two can never disagree about what a dispatch does or
// when a claim is refused, and every fix reaches both at once.
// Attended is deliberately left false on the inner Env: a tool call is Claude
// acting, not a person at a keyboard, and that distinction is what the autonomy
// ladder measures (DESIGN.md §12).
func invoke(ctx context.Context, env *Env, args ...string) (string, error) {
	var buf bytes.Buffer
	inner := &Env{Out: &buf, Err: &buf, In: bytes.NewReader(nil), Paths: env.Paths}

	err := run(ctx, inner, args)
	output := buf.String()
	if err != nil {
		if output != "" {
			return "", fmt.Errorf("%w\n%s", err, output)
		}
		return "", err
	}
	return output, nil
}

func tools(env *Env) []mcp.Tool {
	return []mcp.Tool{{
		Name: "nimbus_context",
		Description: "Describe this device and the task it is working on: OS, hardware, " +
			"installed and missing tools, the previous device's handoff, recent progress, " +
			"durable memory, and unread messages from other devices. Call this first when " +
			"you need to know where you are running or what is in flight.",
		InputSchema: mcp.ObjectSchema(nil),
		Call: func(ctx context.Context, _ map[string]any) (string, error) {
			return invoke(ctx, env, "context")
		},
	}, {
		Name: "nimbus_fleet",
		Description: "List every device in the fleet with its hardware, capabilities, and " +
			"missing tools. Use this to choose where work should run.",
		InputSchema: mcp.ObjectSchema(nil),
		Call: func(ctx context.Context, _ map[string]any) (string, error) {
			return invoke(ctx, env, "fleet")
		},
	}, {
		Name: "nimbus_send",
		Description: "Send a message to another device, addressed by its alias. The message " +
			"appears in that device's next Claude session automatically.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"to":   {Type: "string", Description: "Device alias, e.g. \"studio\" or \"kali-thinkpad\""},
			"text": {Type: "string", Description: "What to tell them"},
			"task": {Type: "string", Description: "Optional task id this is about"},
		}, "to", "text"),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			to, err := mcp.RequireArg(args, "to")
			if err != nil {
				return "", err
			}
			text, err := mcp.RequireArg(args, "text")
			if err != nil {
				return "", err
			}
			argv := []string{"send", to}
			if task := mcp.StringArg(args, "task"); task != "" {
				argv = append(argv, "--task", task)
			}
			return invoke(ctx, env, append(argv, text)...)
		},
	}, {
		Name:        "nimbus_inbox",
		Description: "Read messages other devices have sent to this one.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"include_read": {Type: "boolean", Description: "Also show already-acknowledged messages"},
		}),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			argv := []string{"inbox"}
			if mcp.BoolArg(args, "include_read") {
				argv = append(argv, "--all")
			}
			return invoke(ctx, env, argv...)
		},
	}, {
		Name: "nimbus_ack",
		Description: "Acknowledge a message and optionally answer it. The note travels back " +
			"to whoever sent it.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"id":   {Type: "string", Description: "Message id from nimbus_inbox"},
			"note": {Type: "string", Description: "What you did about it"},
		}, "id"),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			id, err := mcp.RequireArg(args, "id")
			if err != nil {
				return "", err
			}
			argv := []string{"ack", id}
			if note := mcp.StringArg(args, "note"); note != "" {
				argv = append(argv, "--note", note)
			}
			return invoke(ctx, env, argv...)
		},
	}, {
		Name: "nimbus_dispatch",
		Description: "Hand a task to another device: releases the claim here and asks them " +
			"there. Refuses if the target lacks a capability the task requires.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"to":   {Type: "string", Description: "Device alias to hand the task to"},
			"task": {Type: "string", Description: "Task id"},
			"note": {Type: "string", Description: "What you want done"},
		}, "to", "task"),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			to, err := mcp.RequireArg(args, "to")
			if err != nil {
				return "", err
			}
			taskID, err := mcp.RequireArg(args, "task")
			if err != nil {
				return "", err
			}
			argv := []string{"dispatch", to, taskID}
			if note := mcp.StringArg(args, "note"); note != "" {
				argv = append(argv, "--note", note)
			}
			return invoke(ctx, env, argv...)
		},
	}, {
		Name: "nimbus_task_note",
		Description: "Record something that happened on the current task. Notes are merged " +
			"across devices into one timeline.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"text":    {Type: "string", Description: "What happened"},
			"blocked": {Type: "boolean", Description: "Record this as a blocker"},
		}, "text"),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			text, err := mcp.RequireArg(args, "text")
			if err != nil {
				return "", err
			}
			argv := []string{"task", "note"}
			if mcp.BoolArg(args, "blocked") {
				argv = append(argv, "--blocked")
			}
			return invoke(ctx, env, append(argv, text)...)
		},
	}, {
		Name: "nimbus_handoff",
		Description: "Leave a handoff before ending a session, so the next device — or this " +
			"one tomorrow — can continue. Record state that git will not carry in " +
			"outside_git; that is the field people forget and the one that wastes the most " +
			"time on the receiving machine.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"done":        {Type: "array", Items: &mcp.Property{Type: "string"}, Description: "What was finished"},
			"in_flight":   {Type: "array", Items: &mcp.Property{Type: "string"}, Description: "What is half-done"},
			"next":        {Type: "array", Items: &mcp.Property{Type: "string"}, Description: "What to do next"},
			"blocked":     {Type: "array", Items: &mcp.Property{Type: "string"}, Description: "What is stuck"},
			"outside_git": {Type: "array", Items: &mcp.Property{Type: "string"}, Description: "Running containers, databases, manual config"},
			"notes":       {Type: "string", Description: "Anything else"},
			"release":     {Type: "boolean", Description: "Drop this device's claim so another can pick it up"},
		}),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			argv := []string{"task", "handoff"}
			for flagName, key := range map[string]string{
				"--done": "done", "--in-flight": "in_flight", "--next": "next",
				"--blocked": "blocked", "--outside-git": "outside_git",
			} {
				for _, item := range mcp.StringsArg(args, key) {
					argv = append(argv, flagName, item)
				}
			}
			if mcp.BoolArg(args, "release") {
				argv = append(argv, "--release")
			}
			if notes := mcp.StringArg(args, "notes"); notes != "" {
				// Notes arrive on stdin, so a multi-line note needs no quoting.
				return invokeWithInput(ctx, env, notes, argv...)
			}
			return invoke(ctx, env, argv...)
		},
	}, {
		Name: "nimbus_memory_add",
		Description: "Remember a fact. Scratch memory expires in seven days; set long_term " +
			"for something durable about the user or their systems.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"text":      {Type: "string", Description: "The fact to remember"},
			"long_term": {Type: "boolean", Description: "Keep it permanently instead of letting it expire"},
			"tags":      {Type: "string", Description: "Comma-separated tags"},
		}, "text"),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			text, err := mcp.RequireArg(args, "text")
			if err != nil {
				return "", err
			}
			argv := []string{"memory", "add"}
			if mcp.BoolArg(args, "long_term") {
				argv = append(argv, "--long-term")
			}
			if tags := mcp.StringArg(args, "tags"); tags != "" {
				argv = append(argv, "--tags", tags)
			}
			return invoke(ctx, env, append(argv, text)...)
		},
	}, {
		Name:        "nimbus_memory_search",
		Description: "Search remembered facts by text, tag, or task.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"query": {Type: "string", Description: "What to look for"},
			"tier":  {Type: "string", Description: "Limit to \"scratch\" or \"long-term\""},
		}, "query"),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			query, err := mcp.RequireArg(args, "query")
			if err != nil {
				return "", err
			}
			argv := []string{"memory", "search"}
			if tier := mcp.StringArg(args, "tier"); tier != "" {
				argv = append(argv, "--tier", tier)
			}
			return invoke(ctx, env, append(argv, query)...)
		},
	}, {
		Name: "nimbus_system_apply",
		Description: "Make a change to this machine — install a package, edit a config, " +
			"restart a service — with the command that undoes it recorded first. Use this " +
			"instead of running the command directly whenever the change outlives the " +
			"session, so someone who did not watch it happen can still undo it. " +
			"Refused unless this device is at autonomy L3.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"command":  {Type: "string", Description: "The command to run"},
			"rollback": {Type: "string", Description: "The command that undoes it"},
			"kind":     {Type: "string", Description: "package, service, config, or command"},
			"target":   {Type: "string", Description: "What is being changed, e.g. the package name"},
			"dry_run":  {Type: "boolean", Description: "Report whether it would be permitted without running it"},
		}, "command", "rollback"),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			command, err := mcp.RequireArg(args, "command")
			if err != nil {
				return "", err
			}
			rollback, err := mcp.RequireArg(args, "rollback")
			if err != nil {
				return "", err
			}
			argv := []string{"system", "apply", command, "--rollback", rollback}
			if kind := mcp.StringArg(args, "kind"); kind != "" {
				argv = append(argv, "--kind", kind)
			}
			if target := mcp.StringArg(args, "target"); target != "" {
				argv = append(argv, "--target", target)
			}
			if mcp.BoolArg(args, "dry_run") {
				argv = append(argv, "--dry-run")
			}
			return invoke(ctx, env, argv...)
		},
	}, {
		Name: "nimbus_system_history",
		Description: "List OS changes made through nimbus on this device, each with the " +
			"command that undoes it. Read this before changing something that may already " +
			"have been changed, and to find the id to roll back.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"all": {Type: "boolean", Description: "Include changes made on other devices"},
		}),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			argv := []string{"system", "list"}
			if mcp.BoolArg(args, "all") {
				argv = append(argv, "--all")
			}
			return invoke(ctx, env, argv...)
		},
	}, {
		Name: "nimbus_system_rollback",
		Description: "Undo a change from nimbus_system_history by running its recorded " +
			"rollback command.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"id": {Type: "string", Description: "Change id from nimbus_system_history"},
		}, "id"),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			id, err := mcp.RequireArg(args, "id")
			if err != nil {
				return "", err
			}
			return invoke(ctx, env, "system", "rollback", id)
		},
	}, {
		Name: "nimbus_task_queue",
		Description: "List work this device could pick up: tasks nobody holds, plus tasks " +
			"whose holder has gone quiet long enough that the claim has lapsed. Filtered to " +
			"what this device's hardware can actually run.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"any": {Type: "boolean", Description: "Include work this device cannot meet the requirements for"},
		}),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			argv := []string{"task", "queue"}
			if mcp.BoolArg(args, "any") {
				argv = append(argv, "--any")
			}
			return invoke(ctx, env, argv...)
		},
	}, {
		Name: "nimbus_task_steal",
		Description: "Take a task from nimbus_task_queue whose holder has gone quiet. " +
			"Refused while the claim is still live — ask that device to release it instead.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.Property{
			"task": {Type: "string", Description: "Task id"},
		}, "task"),
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			taskID, err := mcp.RequireArg(args, "task")
			if err != nil {
				return "", err
			}
			return invoke(ctx, env, "task", "steal", taskID)
		},
	}}
}

// invokeWithInput runs a command with text piped to it.
func invokeWithInput(ctx context.Context, env *Env, input string, args ...string) (string, error) {
	var buf bytes.Buffer
	inner := &Env{Out: &buf, Err: &buf, In: bytes.NewReader([]byte(input)), Paths: env.Paths}

	err := run(ctx, inner, args)
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, buf.String())
	}
	return buf.String(), nil
}
