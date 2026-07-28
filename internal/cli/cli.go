// Package cli implements nimbus subcommand dispatch.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/nkoteb/nimbus/internal/config"
	"github.com/nkoteb/nimbus/internal/version"
)

// Env carries everything a command needs, so commands stay testable without
// touching global state.
type Env struct {
	Out io.Writer
	Err io.Writer
	// In is read only by commands that accept piped input, and is nil-safe:
	// they fall back to os.Stdin.
	In    io.Reader
	Paths *config.Paths
	// Attended records whether a person ran this command. It is true for the
	// real CLI and false for the MCP server and the boot path, which is what
	// the autonomy ladder is measured against: the ladder caps what happens
	// with nobody watching (DESIGN.md §12).
	Attended bool
}

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, env *Env, args []string) error
}

// Populated in init rather than at declaration: runHelp reads this table to
// render usage, which is an initialization cycle if declared inline.
var commands []command

func init() {
	commands = []command{
		{"init", "set up this device from the state repo", runInit},
		{"repo", "create or inspect the remote state repo", runRepo},
		{"doctor", "profile this device and report what is missing", runDoctor},
		{"fleet", "list every device published to the state repo", runFleet},
		{"alias", "show or set this device's name", runAlias},
		{"task", "create, inspect, and hand off units of work", runTask},
		{"resume", "pick up a task on this device", runResume},
		{"boot", "pick up work left running when this machine restarted", runBoot},
		{"autonomy", "show or set what this device may do unattended", runAutonomy},
		{"system", "journal, apply, and roll back OS changes", runSystem},
		{"context", "print the device and task brief for a Claude session", runContext},
		{"memory", "remember, promote, and search facts across devices", runMemory},
		{"send", "message another device", runSend},
		{"dispatch", "hand a task to another device", runDispatch},
		{"inbox", "read messages addressed to this device", runInbox},
		{"outbox", "show what this device has sent", runOutbox},
		{"ack", "acknowledge a message", runAck},
		{"mcp", "serve the Nimbus MCP server on stdio", runMCP},
		{"daemon", "keep this device synced in the background", runDaemon},
		{"audit", "show or verify this device's action log", runAudit},
		{"login", "authenticate with a git provider via device flow", runLogin},
		{"logout", "remove stored credentials for a provider", runLogout},
		{"whoami", "show the currently authenticated identity", runWhoami},
		{"state", "manage the git-backed state repo", runState},
		{"secrets", "manage the age keypair and encrypted values", runSecrets},
		{"version", "print version information", runVersion},
		{"help", "show this message", runHelp},
	}
}

// Run dispatches args to a subcommand. args excludes the program name.
func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	return run(ctx, &Env{Out: out, Err: errOut, In: os.Stdin, Attended: true}, args)
}

// run is the dispatch loop, separated from Run so the MCP server can invoke
// commands against its own Env and capture their output. Sharing this path is
// what keeps the tool surface and the CLI from drifting apart.
func run(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return runHelp(ctx, env, nil)
	}

	name := args[0]
	if name == "-h" || name == "--help" {
		return runHelp(ctx, env, nil)
	}
	if name == "-v" || name == "--version" {
		return runVersion(ctx, env, nil)
	}

	for _, c := range commands {
		if c.name != name {
			continue
		}
		if env.Paths == nil {
			// Resolved lazily: `help` and `version` must work even when the
			// home directory is unreadable, the bare-device case.
			paths, err := config.Resolve()
			if err != nil && c.name != "help" && c.name != "version" {
				return err
			}
			env.Paths = paths
		}
		return c.run(ctx, env, args[1:])
	}

	return fmt.Errorf("unknown command %q (run `nimbus help`)", name)
}

func runHelp(_ context.Context, env *Env, _ []string) error {
	var b strings.Builder
	b.WriteString("nimbus — your Claude Code sessions, memory, and devices in one mesh\n\n")
	b.WriteString("usage: nimbus <command> [args]\n\ncommands:\n")

	sorted := append([]command(nil), commands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	for _, c := range sorted {
		b.WriteString(fmt.Sprintf("  %-10s %s\n", c.name, c.summary))
	}
	b.WriteString("\nrun `nimbus <command> --help` for details\n")

	_, err := io.WriteString(env.Out, b.String())
	return err
}

func runVersion(_ context.Context, env *Env, _ []string) error {
	_, err := fmt.Fprintln(env.Out, version.String())
	return err
}
