package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func settingsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "settings.json")
}

func readBack(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	return settings
}

func TestEnsureSessionHookCreatesSettings(t *testing.T) {
	path := settingsPath(t)

	step := EnsureSessionHook(path, SessionHookCommand)
	if step.Err != nil {
		t.Fatalf("EnsureSessionHook() error = %v", step.Err)
	}
	if !step.Changed {
		t.Error("step did not report a change")
	}
	if step.Rollback == "" {
		t.Error("a change to the user's settings file recorded no rollback")
	}

	settings := readBack(t, path)
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("no hooks section: %v", settings)
	}
	if _, ok := hooks["SessionStart"].([]any); !ok {
		t.Errorf("no SessionStart hook: %v", hooks)
	}
}

func TestEnsureSessionHookIsIdempotent(t *testing.T) {
	path := settingsPath(t)

	if step := EnsureSessionHook(path, SessionHookCommand); step.Err != nil {
		t.Fatal(step.Err)
	}
	step := EnsureSessionHook(path, SessionHookCommand)
	if step.Err != nil {
		t.Fatal(step.Err)
	}
	if !step.Skipped || step.Changed {
		t.Errorf("second run = %+v, want skipped", step)
	}

	// Stacked duplicates would inject the same context twice on every session.
	hooks := readBack(t, path)["hooks"].(map[string]any)
	if events := hooks["SessionStart"].([]any); len(events) != 1 {
		t.Errorf("SessionStart has %d entries, want 1", len(events))
	}
}

// This is the user's settings file. Anything nimbus does not understand has to
// survive, or init quietly destroys their configuration.
func TestEnsureSessionHookPreservesExistingSettings(t *testing.T) {
	path := settingsPath(t)
	original := `{
	  "model": "opus",
	  "permissions": {"allow": ["Bash(git *)"]},
	  "hooks": {
	    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}],
	    "SessionStart": [{"hooks": [{"type": "command", "command": "existing.sh"}]}]
	  }
	}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if step := EnsureSessionHook(path, SessionHookCommand); step.Err != nil {
		t.Fatal(step.Err)
	}

	settings := readBack(t, path)
	if settings["model"] != "opus" {
		t.Errorf("model = %v, want the user's value preserved", settings["model"])
	}
	if _, ok := settings["permissions"]; !ok {
		t.Error("permissions block was dropped")
	}

	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("an unrelated hook event was dropped")
	}

	events := hooks["SessionStart"].([]any)
	if len(events) != 2 {
		t.Fatalf("SessionStart has %d entries, want the existing one plus ours", len(events))
	}
	if !hookPresent(events, "existing.sh") {
		t.Error("the user's own SessionStart hook was replaced")
	}
	if !hookPresent(events, SessionHookCommand) {
		t.Error("the nimbus hook was not added")
	}
}

// Overwriting a file we could not parse would destroy configuration we cannot
// see, so refusing is the safe outcome.
func TestEnsureSessionHookRefusesUnparseableSettings(t *testing.T) {
	path := settingsPath(t)
	if err := os.WriteFile(path, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	step := EnsureSessionHook(path, SessionHookCommand)
	if step.Err == nil {
		t.Fatal("EnsureSessionHook() overwrote an unparseable settings file")
	}

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{ not json" {
		t.Errorf("file = %q, want it left untouched", data)
	}
}

func TestEnsureSessionHookHandlesEmptyFile(t *testing.T) {
	path := settingsPath(t)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if step := EnsureSessionHook(path, SessionHookCommand); step.Err != nil {
		t.Fatalf("EnsureSessionHook() on an empty file = %v", step.Err)
	}
	if _, ok := readBack(t, path)["hooks"]; !ok {
		t.Error("hook was not added to a file that existed but was empty")
	}
}

// The command travels to every device through the state repo, so it must not
// bake in a path that only exists on the machine that wrote it.
func TestSessionHookCommandIsPortable(t *testing.T) {
	if filepath.IsAbs(SessionHookCommand) {
		t.Errorf("SessionHookCommand = %q, want a PATH-resolved command", SessionHookCommand)
	}
}
