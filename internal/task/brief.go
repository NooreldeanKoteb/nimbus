package task

import (
	"fmt"
	"strings"

	"github.com/nkoteb/nimbus/internal/autonomy"
	"github.com/nkoteb/nimbus/internal/device"
	"github.com/nkoteb/nimbus/internal/memory"
)

// DefaultBriefEvents bounds the timeline in a brief. Enough to see the shape of
// recent work; not so much that it crowds out the session itself.
const DefaultBriefEvents = 15

// Bounds on injected memory. Same reasoning as the timeline: past a point,
// injecting more degrades the context it was meant to improve.
const (
	briefMemoryLimit = 15
	briefMemoryWidth = 160
)

// Brief renders the context a Claude session needs at startup: where it is
// running, what it is working on, and what the last device left behind.
//
// This is the payload of the SessionStart hook (DESIGN.md §9) and of
// `nimbus resume`. It is markdown because that is what reaches Claude
// unmangled and what a person can read directly when something looks wrong.
//
// A nil task is normal — a device with nothing claimed still wants the hardware
// and tooling context, since that is what stops Claude assuming the wrong OS.
func Brief(repoPath string, profile *device.Profile, t *Task, maxEvents int) string {
	// Devices are named, not numbered, everywhere this is read: a handoff from
	// "8f3a2b1c" tells the reader nothing about which machine to go look at.
	var namer Namer
	if fleet, err := device.LoadFleet(repoPath); err == nil {
		namer = fleet
	}

	var b strings.Builder
	b.WriteString("# Nimbus context\n\n")

	if profile != nil {
		renderDevice(&b, profile)
		renderPolicy(&b, repoPath, profile.ID)
	}
	if t == nil {
		b.WriteString("\n## Task\n\nNo task claimed on this device. `nimbus task list` shows what is open.\n")
		return b.String()
	}

	renderTask(&b, profile, t, namer)

	if h, err := LatestHandoff(repoPath, t.ID, ""); err == nil {
		b.WriteString("\n## Handoff\n\n")
		h.Render(&b, namer)
	}

	if events, err := Progress(repoPath, t.ID); err == nil && len(events) > 0 {
		if maxEvents <= 0 {
			maxEvents = DefaultBriefEvents
		}
		if len(events) > maxEvents {
			events = events[len(events)-maxEvents:]
		}
		b.WriteString("\n## Recent progress\n\n")
		for _, e := range events {
			fmt.Fprintf(&b, "- %s  %-12s %-8s %s\n",
				e.Timestamp.Format("01-02 15:04"), name(namer, e.Node), e.Kind, e.Text)
		}
	}

	renderMemory(&b, repoPath, t.ID)

	b.WriteString("\nRecord progress with `nimbus task note \"...\"`, remember facts with " +
		"`nimbus memory add \"...\"`, and leave a handoff with `nimbus task handoff` " +
		"before ending the session.\n")
	return b.String()
}

// renderMemory injects durable facts and the notes taken against this task.
//
// Capped deliberately: memory is only worth injecting while it stays smaller
// than the context it is meant to improve. Long-term facts come first because
// they were explicitly kept; task scratch follows because it is the most
// specific. Everything else is a `nimbus memory search` away.
func renderMemory(b *strings.Builder, repoPath, taskID string) {
	longTerm, err := memory.List(repoPath, memory.Query{Tier: memory.TierLongTerm})
	if err != nil {
		return
	}
	scratch, err := memory.List(repoPath, memory.Query{Tier: memory.TierScratch, Task: taskID})
	if err != nil {
		return
	}
	if len(longTerm) == 0 && len(scratch) == 0 {
		return
	}

	b.WriteString("\n## Memory\n")
	remaining := briefMemoryLimit

	if len(longTerm) > 0 {
		b.WriteString("\nDurable facts:\n")
		remaining = renderEntries(b, longTerm, remaining)
	}
	if len(scratch) > 0 && remaining > 0 {
		b.WriteString("\nNotes on this task:\n")
		remaining = renderEntries(b, scratch, remaining)
	}

	if dropped := len(longTerm) + len(scratch) - briefMemoryLimit; dropped > 0 {
		fmt.Fprintf(b, "\n%d more — `nimbus memory search <term>`\n", dropped)
	}
}

func renderEntries(b *strings.Builder, entries []*memory.Entry, remaining int) int {
	for _, e := range entries {
		if remaining == 0 {
			return 0
		}
		fmt.Fprintf(b, "- %s\n", e.Summary(briefMemoryWidth))
		remaining--
	}
	return remaining
}

func renderDevice(b *strings.Builder, p *device.Profile) {
	fmt.Fprintf(b, "## Device\n\n**%s** (host `%s`) — %s\n", p.Label(), p.Hostname, p.Summary())
	if p.PkgManager != "" {
		fmt.Fprintf(b, "Package manager: `%s`\n", p.PkgManager)
	}

	var present []string
	for _, t := range p.Tools {
		if t.Present {
			present = append(present, t.Name)
		}
	}
	if len(present) > 0 {
		fmt.Fprintf(b, "Installed: %s\n", strings.Join(present, ", "))
	}
	if len(p.Missing) > 0 {
		// Named explicitly so Claude proposes installing a tool rather than
		// silently failing on a command that is not there.
		fmt.Fprintf(b, "Not installed: %s\n", strings.Join(p.Missing, ", "))
	}
	if p.OS.Container {
		b.WriteString("This is a container: host hardware and reboots are not reachable from here.\n")
	}
}

// renderPolicy tells the session what it is permitted to do here.
//
// Without this, Claude has to guess its own ceiling, and it guesses by trying:
// proposing a system change on an L1 laptop, being refused, and spending a turn
// working out why. Stating the level and the invariants up front turns a
// refusal into something it can route around before it happens.
func renderPolicy(b *strings.Builder, repoPath, nodeID string) {
	p := autonomy.Load(repoPath, nodeID)
	fmt.Fprintf(b, "Autonomy: **%s** — %s.\n", p.Level, p.Level.Describe())

	if p.Level < autonomy.L3 {
		b.WriteString("System changes are refused here unless a person runs them; " +
			"`nimbus autonomy l3` raises it.\n")
	}
	b.WriteString("Always, at every level: no force-push, no pushing a default branch, " +
		"no committing unencrypted secrets, and every system change goes through " +
		"`nimbus system apply \"<cmd>\" --rollback \"<undo>\"` so it can be undone.\n")
}

func renderTask(b *strings.Builder, profile *device.Profile, t *Task, n Namer) {
	fmt.Fprintf(b, "\n## Task: %s\n\n%s\n\n", t.ID, t.Goal)
	if t.Repo != "" {
		fmt.Fprintf(b, "- Repo: `%s`\n", t.Repo)
	}
	if t.Branch != "" {
		fmt.Fprintf(b, "- Branch: `%s`\n", t.Branch)
	}
	fmt.Fprintf(b, "- Status: %s\n", t.Status)
	if t.Claim != nil {
		fmt.Fprintf(b, "- Held by: %s (since %s)\n", name(n, t.Claim.Node), t.Claim.At.Format("2006-01-02 15:04 MST"))
	}

	if profile == nil {
		return
	}
	if unmet := t.Unmet(profile.Capabilities()); len(unmet) > 0 {
		// A capability gap changes what Claude should attempt, so it belongs in
		// the brief and not only in the CLI warning the human already dismissed.
		fmt.Fprintf(b, "\n**This device does not meet every requirement for this task.** "+
			"Missing: %s. Steps needing those must wait for a device that has them.\n",
			strings.Join(unmet, ", "))
	}
}
