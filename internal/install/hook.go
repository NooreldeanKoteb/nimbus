package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SessionHookCommand is what the SessionStart hook runs.
//
// Deliberately a bare command rather than an absolute path: settings.json
// travels through the state repo to every device, and /home/you/.local/bin
// does not exist on the Mac. PATH resolution is the only thing that means the
// same on all of them.
const SessionHookCommand = "nimbus context"

// EnsureSessionHook registers the SessionStart hook in Claude Code's settings,
// which is how Claude learns what device it is on and what task is in flight
// without being told (DESIGN.md §9).
//
// The file is merged rather than rewritten. It is the user's settings file and
// may hold hooks, permissions, and model preferences nimbus knows nothing
// about; decoding through map[string]any preserves all of it.
func EnsureSessionHook(settingsPath, command string) Step {
	step := Step{Name: "session hook"}
	if command == "" {
		command = SessionHookCommand
	}

	settings, err := readSettings(settingsPath)
	if err != nil {
		step.Err = err
		return step
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	events, _ := hooks["SessionStart"].([]any)
	if hookPresent(events, command) {
		step.Skipped = true
		step.Detail = "already registered"
		return step
	}

	hooks["SessionStart"] = append(events, map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": command}},
	})
	settings["hooks"] = hooks

	if err := writeSettings(settingsPath, settings); err != nil {
		step.Err = err
		return step
	}

	step.Changed = true
	step.Detail = command + " on session start"
	step.Rollback = "remove the SessionStart hook from " + settingsPath
	return step
}

// hookPresent reports whether the command is already wired up, so a second
// init does not stack duplicate hooks that each inject the same context.
func hookPresent(events []any, command string) bool {
	for _, entry := range events {
		group, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := group["hooks"].([]any)
		for _, h := range inner {
			hook, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := hook["command"].(string); cmd == command {
				return true
			}
		}
	}
	return false
}

func readSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		// Refusing here is the safe move: overwriting a settings file we could
		// not parse would destroy configuration we cannot see.
		return nil, fmt.Errorf("parse %s: %w (fix or remove it, then re-run)", filepath.Base(path), err)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	return settings, nil
}

func writeSettings(path string, settings map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	// Write-then-rename: a crash mid-write must not leave Claude Code with a
	// truncated settings file it will refuse to start with.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
