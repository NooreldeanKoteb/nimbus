package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveHonorsNimbusHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NIMBUS_HOME", root)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	for name, got := range map[string]string{
		"Config": p.Config, "Data": p.Data, "State": p.State, "Repo": p.Repo,
	} {
		if !strings.HasPrefix(got, root) {
			t.Errorf("%s = %q, want prefix %q", name, got, root)
		}
	}
}

func TestResolveUsesXDGWhenAbsolute(t *testing.T) {
	t.Setenv("NIMBUS_HOME", "")
	t.Setenv("HOME", t.TempDir())
	xdgConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfig)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if want := filepath.Join(xdgConfig, "nimbus"); p.Config != want {
		t.Errorf("Config = %q, want %q", p.Config, want)
	}
}

// The XDG spec says relative values must be ignored, not joined onto cwd.
func TestResolveIgnoresRelativeXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NIMBUS_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative/path")

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if want := filepath.Join(home, ".config", "nimbus"); p.Config != want {
		t.Errorf("Config = %q, want %q", p.Config, want)
	}
}

func TestResolveStateRepoAtDotNimbus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NIMBUS_HOME", "")
	t.Setenv("HOME", home)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if want := filepath.Join(home, ".nimbus"); p.Repo != want {
		t.Errorf("Repo = %q, want %q", p.Repo, want)
	}
}

// Credentials live in these directories, so the 0700 mode is load-bearing.
func TestEnsureDirsIsOwnerOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NIMBUS_HOME", root)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}

	for _, dir := range []string{p.Config, p.Data, p.State} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 700", dir, perm)
		}
	}
}

func TestEnsureDirsIsIdempotent(t *testing.T) {
	t.Setenv("NIMBUS_HOME", t.TempDir())
	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	for i := range 3 {
		if err := p.EnsureDirs(); err != nil {
			t.Fatalf("EnsureDirs() call %d error = %v", i, err)
		}
	}
}
