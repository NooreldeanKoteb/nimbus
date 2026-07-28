// Package claude locates Claude Code's local session transcripts.
//
// Sessions live at ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl. The
// directory name encodes the working directory the session ran in, which is why
// a transcript is not portable between devices (DESIGN.md §7a): the same project
// is checked out at a different path on every machine.
package claude

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// NewSessionID generates a UUIDv4, the only form `claude --session-id` accepts.
//
// Generating the id rather than discovering it afterwards is what makes session
// continuity possible: nimbus can record which session belongs to which task
// before the session exists.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ProjectsDir is where Claude Code keeps session transcripts.
func ProjectsDir() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return filepath.Join(v, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// TranscriptExists reports whether a session can actually be resumed here.
//
// A recorded binding is not proof: the machine may have been reimaged, the
// transcript pruned, or the binding may have come from another device through
// the state repo. `claude --resume` on a missing session fails outright, so the
// caller needs to know to start fresh instead.
//
// Searched across every project directory because the session may have been
// started from a different working directory than the one we are in now.
func TranscriptExists(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	root := ProjectsDir()
	if root == "" {
		return false
	}

	matches, err := filepath.Glob(filepath.Join(root, "*", sessionID+".jsonl"))
	if err != nil {
		return false
	}
	return len(matches) > 0
}
