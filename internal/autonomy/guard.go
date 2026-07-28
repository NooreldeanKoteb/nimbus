package autonomy

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Action kinds. Each maps to the rung of the ladder it needs, so a caller
// cannot disagree with the policy about what an action costs.
const (
	ActRead     = "read"
	ActWrite    = "write"
	ActCommit   = "commit"
	ActPush     = "push"
	ActInstall  = "package.install"
	ActDispatch = "peer.dispatch"
	ActExec     = "peer.exec"
	ActSystem   = "system.change"
	ActBoot     = "boot.resume"
)

// needs is the rung each action sits on.
var needs = map[string]Level{
	ActRead:     L0,
	ActWrite:    L1,
	ActCommit:   L1,
	ActPush:     L2,
	ActInstall:  L2,
	ActDispatch: L2,
	ActExec:     L3,
	ActSystem:   L3,
	ActBoot:     L3,
}

// Needs reports the level an action requires. An unknown action needs L3: a
// nimbus that does not recognise what it is about to do should assume the worst.
func Needs(kind string) Level {
	if l, ok := needs[kind]; ok {
		return l
	}
	return L3
}

// protectedBranches are never pushed to (invariant 1). Names, not patterns:
// the point is to catch the branch a repository actually deploys from.
var protectedBranches = []string{"main", "master", "trunk", "release", "production"}

// Action describes something about to happen, in enough detail for the
// invariants to be checked rather than trusted.
type Action struct {
	Kind   string
	Target string
	Detail string

	// Rollback undoes this action. Required for a system change (invariant 4).
	Rollback string
	// Irreversible marks a change that genuinely cannot be undone, with the
	// reason in Detail. It is still refused unattended — the point is that
	// "no rollback" must be a decision someone made, never an omission.
	Irreversible bool

	// Branch and Force describe a git push (invariant 1).
	Branch string
	Force  bool

	// Command is the shell command to be run. Peer-originated commands are
	// checked against the allowlist (invariant 3).
	Command string
	// FromPeer marks work that arrived over the mesh rather than from the
	// person sitting at this machine.
	FromPeer bool

	// Path and Destructive describe a filesystem operation (invariant 5).
	Path        string
	Destructive bool

	// Secrets names findings from a secret scan of the content being written.
	// The unbypassable enforcement is in state.Repo.Commit, which scans what is
	// actually staged; this field covers callers that scan earlier.
	Secrets []string
}

// Decision is the answer, with the reason attached. Refusals are explained
// because the caller is often Claude, which can act on a reason and cannot act
// on a boolean.
type Decision struct {
	Allowed bool
	Reason  string
	// Invariant is the number of the invariant that refused, 0 when the ladder
	// refused instead. An invariant refusal cannot be lifted by raising the
	// level, and the two need to be distinguishable in the message.
	Invariant int
}

func (d Decision) Error() string { return d.Reason }

// Guard applies one device's policy to a proposed action.
type Guard struct {
	Level Level
	// Attended is true when a person ran the command that led here. The ladder
	// caps what happens *unattended* (DESIGN.md §12), so a human at the keyboard
	// is the confirmation the ladder would otherwise have to ask for. It never
	// lifts an invariant — invariant 5 is the single one that consults it, and
	// only because it is defined as "always require a human".
	Attended bool
	// WorkTree bounds destructive filesystem operations. Empty means every
	// destructive path is outside it, which is the safe reading.
	WorkTree string
	// AllowedCommands are the commands a peer may run here (invariant 3).
	AllowedCommands []string
}

// Allow decides whether an action may proceed.
//
// Invariants are checked before the ladder, so a refusal always names the
// strongest reason: being told "raise your level" about something no level
// permits would be actively misleading.
func (g Guard) Allow(a Action) Decision {
	if d := g.invariants(a); !d.Allowed {
		return d
	}

	need := Needs(a.Kind)
	if g.Attended || g.Level >= need {
		return Decision{Allowed: true}
	}
	return Decision{Reason: fmt.Sprintf(
		"%s needs %s and this device is at %s — run `nimbus autonomy %s` to allow it unattended",
		a.Kind, need, g.Level, strings.ToLower(need.String()))}
}

func (g Guard) invariants(a Action) Decision {
	// 1. Never force-push, and never push to a default branch.
	if a.Kind == ActPush {
		if a.Force {
			return refuse(1, "force-push is never permitted")
		}
		if isProtected(a.Branch) {
			return refuse(1, fmt.Sprintf("pushing to %q is never permitted; use a feature branch", a.Branch))
		}
	}

	// 2. Never commit unencrypted secrets.
	if len(a.Secrets) > 0 {
		return refuse(2, "content contains "+strings.Join(a.Secrets, ", ")+
			"; remove it, or encrypt it with `nimbus secrets encrypt`")
	}

	// 3. A peer may only run allowlisted commands.
	if a.FromPeer && a.Command != "" && !g.commandAllowed(a.Command) {
		return refuse(3, fmt.Sprintf("%q did not arrive on this device's allowlist for peer execution", a.Command))
	}

	// 4. Every system change is journaled with a rollback before it is applied.
	if a.Kind == ActSystem && a.Rollback == "" && !a.Irreversible {
		return refuse(4, "a system change must record how to undo it "+
			"(pass --rollback \"...\", or --irreversible with a reason)")
	}
	if a.Kind == ActSystem && a.Irreversible && !g.Attended {
		return refuse(4, "an irreversible system change needs a human present")
	}

	// 5. Destructive filesystem operations outside the working tree need a human.
	if a.Destructive && !g.Attended && !g.inWorkTree(a.Path) {
		return refuse(5, fmt.Sprintf("deleting %s is outside the working tree and needs a human", orUnknown(a.Path)))
	}

	return Decision{Allowed: true}
}

func refuse(n int, why string) Decision {
	return Decision{Reason: fmt.Sprintf("invariant %d: %s", n, why), Invariant: n}
}

func isProtected(branch string) bool {
	branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	for _, p := range protectedBranches {
		if strings.EqualFold(branch, p) {
			return true
		}
	}
	return false
}

// commandAllowed matches the first word of the command against the allowlist,
// so an entry of "systemctl" permits `systemctl status nimbus` but a command
// smuggled in as an argument — `echo x; rm -rf /` — does not match at all.
func (g Guard) commandAllowed(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	// Shell metacharacters mean the first word is no longer the whole story.
	if strings.ContainsAny(command, ";&|`$><\n") {
		return false
	}
	head := filepath.Base(fields[0])
	for _, allowed := range g.AllowedCommands {
		if allowed == command || filepath.Base(allowed) == head {
			return true
		}
	}
	return false
}

// inWorkTree reports whether a path is inside the guarded tree. A relative path
// or a traversal that climbs out is treated as outside.
func (g Guard) inWorkTree(path string) bool {
	if g.WorkTree == "" || path == "" {
		return false
	}
	root, err := filepath.Abs(g.WorkTree)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func orUnknown(path string) string {
	if path == "" {
		return "an unnamed path"
	}
	return path
}
