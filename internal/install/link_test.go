package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stepByName(steps []Step, name string) (Step, bool) {
	for _, s := range steps {
		if s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

// The first-device case: existing config must be captured, never discarded.
func TestAdoptCapturesExistingLocalConfig(t *testing.T) {
	local, repo := t.TempDir(), t.TempDir()
	write(t, filepath.Join(local, "CLAUDE.md"), "# my instructions")
	write(t, filepath.Join(local, "skills", "a", "SKILL.md"), "skill body")

	steps := AdoptClaudeConfig(local, repo, []string{"CLAUDE.md", "skills", "settings.json"})

	for _, s := range steps {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.Name, s.Err)
		}
	}

	got, err := os.ReadFile(filepath.Join(repo, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("CLAUDE.md not adopted: %v", err)
	}
	if string(got) != "# my instructions" {
		t.Errorf("adopted content = %q", got)
	}

	// Directory trees must be copied recursively, not just their top level.
	if _, err := os.Stat(filepath.Join(repo, "skills", "a", "SKILL.md")); err != nil {
		t.Errorf("nested skill not adopted: %v", err)
	}

	// Absent entries are skipped, not errors.
	if s, ok := stepByName(steps, "adopt:settings.json"); !ok || !s.Skipped {
		t.Errorf("absent entry should be skipped, got %+v", s)
	}
}

// The second-device case: the repo already holds config, so adopt must not
// overwrite it with whatever happens to be on this machine.
func TestAdoptDoesNotOverwriteRepo(t *testing.T) {
	local, repo := t.TempDir(), t.TempDir()
	write(t, filepath.Join(local, "CLAUDE.md"), "local version")
	write(t, filepath.Join(repo, "CLAUDE.md"), "canonical version")

	steps := AdoptClaudeConfig(local, repo, []string{"CLAUDE.md"})
	if !steps[0].Skipped {
		t.Error("adopt should skip when the repo already has the entry")
	}

	got, _ := os.ReadFile(filepath.Join(repo, "CLAUDE.md"))
	if string(got) != "canonical version" {
		t.Errorf("repo content = %q, want it untouched", got)
	}
}

// Re-running init must not copy the repo onto itself through its own symlink.
func TestAdoptSkipsAlreadyLinkedConfig(t *testing.T) {
	local, repo := t.TempDir(), t.TempDir()
	write(t, filepath.Join(repo, "CLAUDE.md"), "canonical")
	if err := os.Symlink(filepath.Join(repo, "CLAUDE.md"), filepath.Join(local, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}

	steps := AdoptClaudeConfig(local, repo, []string{"CLAUDE.md"})
	if !steps[0].Skipped {
		t.Errorf("adopt should skip a symlinked entry, got %+v", steps[0])
	}
}

func TestLinkCreatesSymlinks(t *testing.T) {
	local, repo := t.TempDir(), t.TempDir()
	write(t, filepath.Join(repo, "CLAUDE.md"), "shared")

	steps := LinkClaudeConfig(repo, local, []string{"CLAUDE.md"})
	if steps[0].Err != nil {
		t.Fatalf("link error = %v", steps[0].Err)
	}
	if !steps[0].Changed {
		t.Error("link reported no change")
	}

	target, err := os.Readlink(filepath.Join(local, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("not a symlink: %v", err)
	}
	if target != filepath.Join(repo, "CLAUDE.md") {
		t.Errorf("symlink -> %q", target)
	}
}

// Losing a hand-written CLAUDE.md to an automated setup step is unacceptable,
// so an existing real file must be preserved.
func TestLinkBacksUpExistingFile(t *testing.T) {
	local, repo := t.TempDir(), t.TempDir()
	write(t, filepath.Join(repo, "CLAUDE.md"), "from repo")
	write(t, filepath.Join(local, "CLAUDE.md"), "precious local content")

	steps := LinkClaudeConfig(repo, local, []string{"CLAUDE.md"})
	if steps[0].Err != nil {
		t.Fatalf("link error = %v", steps[0].Err)
	}

	matches, err := filepath.Glob(filepath.Join(local, "CLAUDE.md.nimbus-backup-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one backup, got %v (err %v)", matches, err)
	}
	got, _ := os.ReadFile(matches[0])
	if string(got) != "precious local content" {
		t.Errorf("backup content = %q, want the original local file", got)
	}

	linked, _ := os.ReadFile(filepath.Join(local, "CLAUDE.md"))
	if string(linked) != "from repo" {
		t.Errorf("linked content = %q, want repo version", linked)
	}
}

func TestLinkIsIdempotent(t *testing.T) {
	local, repo := t.TempDir(), t.TempDir()
	write(t, filepath.Join(repo, "CLAUDE.md"), "shared")

	first := LinkClaudeConfig(repo, local, []string{"CLAUDE.md"})
	if !first[0].Changed {
		t.Fatal("first link should report a change")
	}

	second := LinkClaudeConfig(repo, local, []string{"CLAUDE.md"})
	if !second[0].Skipped {
		t.Errorf("second link should be a no-op, got %+v", second[0])
	}

	// Re-running must not accumulate backups.
	matches, _ := filepath.Glob(filepath.Join(local, "CLAUDE.md.nimbus-backup-*"))
	if len(matches) != 0 {
		t.Errorf("idempotent re-link created backups: %v", matches)
	}
}

// A symlink pointing somewhere stale is replaced without a backup, since there
// is no user content to lose.
func TestLinkReplacesStaleSymlink(t *testing.T) {
	local, repo := t.TempDir(), t.TempDir()
	write(t, filepath.Join(repo, "CLAUDE.md"), "current")

	stale := filepath.Join(t.TempDir(), "old-target")
	write(t, stale, "old")
	if err := os.Symlink(stale, filepath.Join(local, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}

	steps := LinkClaudeConfig(repo, local, []string{"CLAUDE.md"})
	if steps[0].Err != nil {
		t.Fatalf("link error = %v", steps[0].Err)
	}

	target, _ := os.Readlink(filepath.Join(local, "CLAUDE.md"))
	if target != filepath.Join(repo, "CLAUDE.md") {
		t.Errorf("symlink -> %q, want the repo path", target)
	}
	matches, _ := filepath.Glob(filepath.Join(local, "*.nimbus-backup-*"))
	if len(matches) != 0 {
		t.Errorf("stale symlink should not produce a backup, got %v", matches)
	}
}

func TestLinkSkipsEntriesAbsentFromRepo(t *testing.T) {
	local, repo := t.TempDir(), t.TempDir()

	steps := LinkClaudeConfig(repo, local, []string{"CLAUDE.md"})
	if !steps[0].Skipped {
		t.Errorf("missing repo entry should be skipped, got %+v", steps[0])
	}
	if _, err := os.Lstat(filepath.Join(local, "CLAUDE.md")); err == nil {
		t.Error("link created a dangling symlink for an absent source")
	}
}

func TestAdoptThenLinkRoundTrip(t *testing.T) {
	local, repo := t.TempDir(), t.TempDir()
	write(t, filepath.Join(local, "CLAUDE.md"), "original")

	for _, s := range AdoptClaudeConfig(local, repo, []string{"CLAUDE.md"}) {
		if s.Err != nil {
			t.Fatalf("adopt: %v", s.Err)
		}
	}
	for _, s := range LinkClaudeConfig(repo, local, []string{"CLAUDE.md"}) {
		if s.Err != nil {
			t.Fatalf("link: %v", s.Err)
		}
	}

	// After the round trip the content must survive, now via the repo.
	got, err := os.ReadFile(filepath.Join(local, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Errorf("content = %q, want original", got)
	}
	if _, err := os.Readlink(filepath.Join(local, "CLAUDE.md")); err != nil {
		t.Error("local path is not a symlink after adopt+link")
	}

	// Editing through the repo must be visible locally.
	write(t, filepath.Join(repo, "CLAUDE.md"), "updated centrally")
	got, _ = os.ReadFile(filepath.Join(local, "CLAUDE.md"))
	if string(got) != "updated centrally" {
		t.Errorf("content = %q, want the central update", got)
	}
}

func TestLoadManifestDefaultsWhenAbsent(t *testing.T) {
	m, err := LoadManifest(filepath.Join(t.TempDir(), "manifest.json"))
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	if !m.ClaudeCode.Install {
		t.Error("default manifest does not install Claude Code")
	}
	if len(m.LinkConfig) == 0 {
		t.Error("default manifest links nothing")
	}
}

func TestManifestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "manifest.json")
	original := &Manifest{
		ClaudeCode: ClaudeCodeSpec{Install: true, Version: "stable"},
		MCPServers: []MCPServer{
			{Name: "context7", Transport: "http", URL: "https://example.com/mcp"},
			{Name: "playwright", Transport: "stdio", Command: "npx", Args: []string{"-y", "@playwright/mcp"}},
		},
		LinkConfig: []string{"CLAUDE.md"},
	}
	if err := original.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	if len(loaded.MCPServers) != 2 {
		t.Fatalf("MCPServers = %d, want 2", len(loaded.MCPServers))
	}
	if loaded.MCPServers[1].Args[1] != "@playwright/mcp" {
		t.Errorf("args round-tripped as %v", loaded.MCPServers[1].Args)
	}
}

func TestLoadManifestRejectsInvalidServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	write(t, path, `{"mcp_servers":[{"name":"broken","transport":"stdio"}]}`)

	_, err := LoadManifest(path)
	if err == nil {
		t.Fatal("LoadManifest() accepted a stdio server with no command")
	}
	if !strings.Contains(err.Error(), "command") {
		t.Errorf("error = %v, want it to mention the missing command", err)
	}
}

func TestMCPServerValidate(t *testing.T) {
	cases := []struct {
		name    string
		server  MCPServer
		wantErr bool
	}{
		{"valid stdio", MCPServer{Name: "a", Transport: "stdio", Command: "npx"}, false},
		{"valid http", MCPServer{Name: "b", Transport: "http", URL: "https://x"}, false},
		{"valid sse", MCPServer{Name: "c", Transport: "sse", URL: "https://x"}, false},
		{"no name", MCPServer{Transport: "stdio", Command: "npx"}, true},
		{"no transport", MCPServer{Name: "d"}, true},
		{"bad transport", MCPServer{Name: "e", Transport: "carrier-pigeon"}, true},
		{"http without url", MCPServer{Name: "f", Transport: "http"}, true},
		{"stdio without command", MCPServer{Name: "g", Transport: "stdio"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.server.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestMCPAddArgs(t *testing.T) {
	http := mcpAddArgs(MCPServer{Name: "ctx", Transport: "http", URL: "https://x/mcp"})
	joined := strings.Join(http, " ")
	if !strings.Contains(joined, "--transport http") || !strings.Contains(joined, "https://x/mcp") {
		t.Errorf("http args = %v", http)
	}
	// Defaulting to user scope is what makes a server available in every repo.
	if !strings.Contains(joined, "--scope user") {
		t.Errorf("http args missing default user scope: %v", http)
	}

	stdio := mcpAddArgs(MCPServer{Name: "pw", Transport: "stdio", Command: "npx", Args: []string{"-y", "pkg"}})
	joined = strings.Join(stdio, " ")
	// The -- separator keeps server flags from being parsed by claude itself.
	if !strings.Contains(joined, "-- npx -y pkg") {
		t.Errorf("stdio args = %v", stdio)
	}
}

func TestInstallClaudeCodeRespectsManifestOptOut(t *testing.T) {
	step := InstallClaudeCode(t.Context(), ClaudeCodeSpec{Install: false})
	if !step.Skipped {
		t.Errorf("step = %+v, want skipped", step)
	}
	if step.Err != nil {
		t.Errorf("opting out should not error, got %v", step.Err)
	}
}

func TestInstallMCPServersSkipsWithoutClaude(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	steps := InstallMCPServers(t.Context(), []MCPServer{
		{Name: "ctx", Transport: "http", URL: "https://x"},
	})
	if len(steps) != 1 || !steps[0].Skipped {
		t.Fatalf("steps = %+v, want one skipped step", steps)
	}
	if !strings.Contains(steps[0].Detail, "claude not installed") {
		t.Errorf("detail = %q", steps[0].Detail)
	}
}

func TestLastLines(t *testing.T) {
	got := lastLines("one\ntwo\nthree\nfour", 2)
	if got != "three; four" {
		t.Errorf("lastLines() = %q, want %q", got, "three; four")
	}
	if got := lastLines("only", 5); got != "only" {
		t.Errorf("lastLines() = %q", got)
	}
}
