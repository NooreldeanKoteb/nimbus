package task

import (
	"fmt"
	"time"
)

// DefaultLease is how long a claim survives without the holder doing anything.
//
// Two hours rather than minutes: a device working a task can legitimately go
// quiet while a build runs or a person goes to lunch, and stealing work that is
// still in progress is worse than waiting. It is a lease, not a heartbeat.
const DefaultLease = 2 * time.Hour

// LastActivity is when the holding device last touched this task — its newest
// progress event, falling back to when the claim was taken.
//
// Derived from the timeline rather than stored, for the same reason Active is:
// a stored "last seen" is one more field that can disagree with the events it
// claims to summarise.
func (t *Task) LastActivity(repoPath string) time.Time {
	var last time.Time
	if t.Claim != nil {
		last = t.Claim.At
	}

	events, err := Progress(repoPath, t.ID)
	if err != nil {
		return last
	}
	for _, e := range events {
		if t.Claim != nil && e.Node != t.Claim.Node {
			continue
		}
		if e.Timestamp.After(last) {
			last = e.Timestamp
		}
	}
	return last
}

// LeaseExpired reports whether the holder has gone quiet long enough that
// another device may take the task.
//
// An unclaimed task has no lease to expire — it is simply free, which callers
// distinguish by checking Claim themselves.
func (t *Task) LeaseExpired(repoPath string, lease time.Duration, now time.Time) bool {
	if t.Claim == nil {
		return false
	}
	if lease <= 0 {
		lease = DefaultLease
	}
	return now.Sub(t.LastActivity(repoPath)) > lease
}

// Stealable lists work this device could pick up right now: open tasks that are
// unclaimed or whose lease has lapsed, and whose requirements this device meets.
//
// The capability filter is what makes the queue a queue rather than a list. A
// task needing a GPU is not work an idle laptop can take, and offering it there
// only produces a claim that has to be handed back.
func Stealable(repoPath, selfID string, capabilities []string, lease time.Duration) ([]*Task, error) {
	tasks, err := List(repoPath)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	var open []*Task
	for _, t := range tasks {
		if t.Status != StatusActive || t.HeldBy(selfID) {
			continue
		}
		if t.Claim != nil && !t.LeaseExpired(repoPath, lease, now) {
			continue
		}
		if len(t.Unmet(capabilities)) > 0 {
			continue
		}
		open = append(open, t)
	}
	return open, nil
}

// StealReason explains why a task is available, for a listing that should say
// why rather than only what.
func (t *Task) StealReason(repoPath string, lease time.Duration, n Namer) string {
	if t.Claim == nil {
		return "unclaimed"
	}
	idle := time.Since(t.LastActivity(repoPath)).Round(time.Minute)
	return fmt.Sprintf("held by %s, idle %s", name(n, t.Claim.Node), roughDuration(idle))
}

func roughDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
