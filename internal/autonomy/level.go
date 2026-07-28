// Package autonomy decides what nimbus may do on a device without being asked.
//
// Two mechanisms, and the difference between them is the whole point:
//
//   - The **ladder** (L0–L3) is a per-device ceiling the owner chooses. It is a
//     dial: raise it to let more happen unattended, lower it to hold work back.
//   - The **invariants** are not a dial. They hold at every level including L3,
//     and there is no flag that turns them off. See guard.go.
//
// The reason the ladder is safe to run at L3 on your own machines is precisely
// that the invariants are enforced in code rather than by asking each time
// (DESIGN.md §12).
package autonomy

import (
	"fmt"
	"strings"
)

// Level is the ceiling on what may happen without a human present.
type Level int

// The ladder. Ordered, so a comparison is the whole permission check.
const (
	// L0 reads and proposes. It never writes.
	L0 Level = iota
	// L1 writes code, runs tests, and commits to a feature branch.
	L1
	// L2 pushes branches, installs packages, and dispatches to peers.
	L2
	// L3 configures the system, manages services, and resumes after a boot.
	L3
)

// Default is the level a device runs at until its owner chooses otherwise.
//
// L1 rather than L0 because a device that cannot write is a device that cannot
// do the work nimbus exists to carry between machines; L1 rather than L2 because
// nothing should reach another machine — a push, a peer dispatch — as a
// consequence of a default nobody chose.
const Default = L1

// Parse reads a level from a string, accepting "l2", "L2", and "2".
func Parse(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "l0", "0":
		return L0, nil
	case "l1", "1":
		return L1, nil
	case "l2", "2":
		return L2, nil
	case "l3", "3":
		return L3, nil
	default:
		return Default, fmt.Errorf("unknown autonomy level %q (use l0, l1, l2, or l3)", s)
	}
}

func (l Level) String() string {
	if l < L0 || l > L3 {
		return "L?"
	}
	return fmt.Sprintf("L%d", int(l))
}

// Describe is the one-line summary shown wherever a level is reported. Written
// for someone deciding whether to raise it.
func (l Level) Describe() string {
	switch l {
	case L0:
		return "read and propose only; no writes"
	case L1:
		return "write code, run tests, commit to a branch; no push, no system changes"
	case L2:
		return "push branches, install packages, dispatch to peers; no system changes"
	case L3:
		return "system configuration, service management, boot resume"
	default:
		return "unknown"
	}
}

// Valid reports whether a level is on the ladder. A level read from a state repo
// written by a newer nimbus could be anything.
func (l Level) Valid() bool { return l >= L0 && l <= L3 }
