package task

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/nkoteb/nimbus/internal/claude"
)

// LoadSession reads one device's record for a task — its session binding and
// its most recent handoff, which share a file.
func LoadSession(repoPath, id, nodeID string) (*Handoff, error) {
	data, err := os.ReadFile(SessionFile(repoPath, id, nodeID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	var h Handoff
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Resumption describes how a device should re-enter a task.
type Resumption struct {
	SessionID string
	// Resume is true when a real conversation exists on this machine to
	// continue. False means a fresh session seeded with the handoff brief.
	Resume bool
	// Reason explains a fresh start, so the user is never left wondering why
	// their conversation did not come back.
	Reason string
}

// BindSession decides which Claude Code session this device should use for a
// task, and records the choice.
//
// The rule that matters: **a session is per device, not per task.** Transcripts
// are keyed by working directory and routinely run to megabytes, so they are
// deliberately not synced (DESIGN.md §7a). Returning to the machine you were on
// therefore restores the actual conversation; arriving on a new machine gets a
// fresh session seeded with the handoff. Pretending otherwise would mean
// promising continuity the data cannot deliver.
func BindSession(repoPath, id, nodeID, hostname string) (*Resumption, error) {
	h, err := LoadSession(repoPath, id, nodeID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if h == nil {
		h = &Handoff{Node: nodeID, Host: hostname}
	}

	switch {
	case h.SessionID == "":
		// First time this device has worked this task.
	case claude.TranscriptExists(h.SessionID):
		return &Resumption{SessionID: h.SessionID, Resume: true}, nil
	default:
		// The binding outlived the transcript: reimaged machine, pruned
		// history, or a config directory that moved. Resuming would fail
		// outright, so start clean rather than hand the user an error.
		newID, err := claude.NewSessionID()
		if err != nil {
			return nil, err
		}
		h.SessionID = newID
		if err := h.Save(repoPath, id); err != nil {
			return nil, err
		}
		return &Resumption{
			SessionID: newID,
			Reason:    "previous session transcript is no longer on this device",
		}, nil
	}

	newID, err := claude.NewSessionID()
	if err != nil {
		return nil, err
	}
	h.SessionID = newID
	if err := h.Save(repoPath, id); err != nil {
		return nil, err
	}
	return &Resumption{
		SessionID: newID,
		Reason:    "first session for this task on this device",
	}, nil
}
