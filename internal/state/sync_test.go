package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
)

// device sets up a local clone standing in for one machine in the fleet.
func device(t *testing.T, origin, name string) *Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)

	repo, err := Clone(context.Background(), origin, path, nil)
	if err != nil {
		t.Fatalf("clone for %s: %v", name, err)
	}
	return repo
}

// seededRemote returns a bare remote that already has one commit, since an
// empty repo cannot be cloned.
func seededRemote(t *testing.T) string {
	t.Helper()
	origin := bareRemote(t)

	seed := filepath.Join(t.TempDir(), "seed")
	repo, err := Init(seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRemote(origin); err != nil {
		t.Fatal(err)
	}
	writeFile(t, seed, "config/manifest.json", "{}")
	if _, err := repo.Commit("seed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	return origin
}

func TestSyncPushesToOrigin(t *testing.T) {
	origin := seededRemote(t)
	dev := device(t, origin, "desktop")

	writeFile(t, dev.Path, "nodes/desktop.json", `{"id":"desktop"}`)

	result, err := dev.Sync(context.Background(), "publish desktop", ownedFor("desktop"))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !result.Committed || !result.Pushed {
		t.Errorf("result = %+v, want committed and pushed", result)
	}

	// Prove it landed by cloning fresh.
	other := device(t, origin, "verify")
	if _, err := os.Stat(filepath.Join(other.Path, "nodes", "desktop.json")); err != nil {
		t.Errorf("pushed profile missing from origin: %v", err)
	}
}

func TestSyncOnCleanTreeDoesNothing(t *testing.T) {
	dev := device(t, seededRemote(t), "desktop")

	result, err := dev.Sync(context.Background(), "nothing", ownedFor("desktop"))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if result.Committed {
		t.Errorf("result = %+v, want no commit on a clean tree", result)
	}
}

func TestSyncWithoutRemoteCommitsLocally(t *testing.T) {
	repo, err := Init(filepath.Join(t.TempDir(), "local"), nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo.Path, "nodes/solo.json", "{}")

	result, err := repo.Sync(context.Background(), "local only", ownedFor("solo"))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !result.Committed {
		t.Error("change was not committed locally")
	}
	if result.Pushed {
		t.Error("reported a push with no origin configured")
	}
	if result.Detail == "" {
		t.Error("no explanation given for not pushing")
	}
}

// The case auto-sync exists to survive: two devices push without seeing each
// other's work, and both results must end up on the remote.
func TestSyncReconcilesDivergedHistory(t *testing.T) {
	origin := seededRemote(t)
	laptop := device(t, origin, "laptop")
	desktop := device(t, origin, "desktop")

	// Laptop publishes first and wins the race.
	writeFile(t, laptop.Path, "nodes/laptop.json", `{"id":"laptop"}`)
	if _, err := laptop.Sync(context.Background(), "publish laptop", ownedFor("laptop")); err != nil {
		t.Fatalf("laptop sync: %v", err)
	}

	// Desktop has not seen that commit, so its push is rejected and must
	// reconcile rather than fail or clobber.
	writeFile(t, desktop.Path, "nodes/desktop.json", `{"id":"desktop"}`)
	result, err := desktop.Sync(context.Background(), "publish desktop", ownedFor("desktop"))
	if err != nil {
		t.Fatalf("desktop sync: %v", err)
	}
	if !result.Pushed {
		t.Fatalf("result = %+v, want pushed after reconcile", result)
	}

	// Both devices' profiles must survive on the remote.
	final := device(t, origin, "verify")
	for _, name := range []string{"laptop.json", "desktop.json"} {
		if _, err := os.Stat(filepath.Join(final.Path, "nodes", name)); err != nil {
			t.Errorf("%s lost during reconcile: %v", name, err)
		}
	}
}

// Reconcile must not resurrect our stale copy of a file another device edited.
func TestReconcileKeepsRemoteVersionOfSharedFiles(t *testing.T) {
	origin := seededRemote(t)
	laptop := device(t, origin, "laptop")
	desktop := device(t, origin, "desktop")

	writeFile(t, laptop.Path, "config/manifest.json", `{"owner":"laptop"}`)
	if _, err := laptop.Sync(context.Background(), "laptop edits manifest", ownedFor("laptop")); err != nil {
		t.Fatal(err)
	}

	// Desktop only touches its own profile; the shared file is untouched here.
	writeFile(t, desktop.Path, "nodes/desktop.json", `{"id":"desktop"}`)
	if _, err := desktop.Sync(context.Background(), "publish desktop", ownedFor("desktop")); err != nil {
		t.Fatal(err)
	}

	final := device(t, origin, "verify")
	got, err := os.ReadFile(filepath.Join(final.Path, "config", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"owner":"laptop"}` {
		t.Errorf("manifest = %q, want the laptop's version preserved", got)
	}
}

// A device's own audit log must never be replaced by another device's history.
func TestReconcilePreservesOwnAuditLog(t *testing.T) {
	origin := seededRemote(t)
	laptop := device(t, origin, "laptop")
	desktop := device(t, origin, "desktop")

	writeFile(t, laptop.Path, "audit/laptop.jsonl", `{"seq":1,"node":"laptop"}`)
	if _, err := laptop.Sync(context.Background(), "laptop audit", ownedFor("laptop")); err != nil {
		t.Fatal(err)
	}

	writeFile(t, desktop.Path, "audit/desktop.jsonl", `{"seq":1,"node":"desktop"}`)
	if _, err := desktop.Sync(context.Background(), "desktop audit", ownedFor("desktop")); err != nil {
		t.Fatal(err)
	}

	final := device(t, origin, "verify")
	got, err := os.ReadFile(filepath.Join(final.Path, "audit", "desktop.jsonl"))
	if err != nil {
		t.Fatalf("desktop audit log lost: %v", err)
	}
	if string(got) != `{"seq":1,"node":"desktop"}` {
		t.Errorf("desktop audit = %q, want its own content", got)
	}
}

// Three devices publishing in sequence is the ordinary fleet case.
func TestSyncAcrossThreeDevices(t *testing.T) {
	origin := seededRemote(t)

	for _, name := range []string{"laptop", "desktop", "server"} {
		dev := device(t, origin, name)
		writeFile(t, dev.Path, filepath.Join("nodes", name+".json"), `{"id":"`+name+`"}`)
		if _, err := dev.Sync(context.Background(), "publish "+name, ownedFor(name)); err != nil {
			t.Fatalf("%s sync: %v", name, err)
		}
	}

	final := device(t, origin, "verify")
	entries, err := os.ReadDir(filepath.Join(final.Path, "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("nodes/ has %d profiles, want 3", len(entries))
	}
}

func TestRefreshPullsRemoteChanges(t *testing.T) {
	origin := seededRemote(t)
	laptop := device(t, origin, "laptop")
	desktop := device(t, origin, "desktop")

	writeFile(t, laptop.Path, "nodes/laptop.json", `{"id":"laptop"}`)
	if _, err := laptop.Sync(context.Background(), "publish laptop", ownedFor("laptop")); err != nil {
		t.Fatal(err)
	}

	if err := desktop.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(desktop.Path, "nodes", "laptop.json")); err != nil {
		t.Errorf("Refresh() did not pull the laptop's profile: %v", err)
	}
}

func TestRefreshWithoutRemoteIsNoop(t *testing.T) {
	repo, err := Init(filepath.Join(t.TempDir(), "local"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Refresh(context.Background()); err != nil {
		t.Errorf("Refresh() with no origin error = %v", err)
	}
}

// Being offline must be distinguished from being rejected: one is retried
// silently, the other needs reconciling.
func TestErrorClassification(t *testing.T) {
	offline := []error{
		errors.New("dial tcp: lookup github.com: no such host"),
		errors.New("dial tcp 1.2.3.4:443: connect: network is unreachable"),
		errors.New("read tcp: connection reset by peer"),
		errors.New("Get \"https://x\": net/http: TLS handshake timeout"),
	}
	for _, err := range offline {
		if !isOffline(err) {
			t.Errorf("isOffline(%v) = false, want true", err)
		}
		if isDiverged(err) {
			t.Errorf("isDiverged(%v) = true for a network error", err)
		}
	}

	diverged := []error{
		git.ErrNonFastForwardUpdate,
		errors.New("command error on refs/heads/main: non-fast-forward update"),
		errors.New("failed to push some refs, fetch first"),
	}
	for _, err := range diverged {
		if !isDiverged(err) {
			t.Errorf("isDiverged(%v) = false, want true", err)
		}
	}

	if isOffline(nil) || isDiverged(nil) {
		t.Error("nil error classified as a failure")
	}

	// A missing repo is a real error, not a transient network blip.
	if isOffline(errors.New("authentication required")) {
		t.Error("auth failure misclassified as offline")
	}
}

func ownedFor(nodeID string) []string {
	return []string{
		filepath.Join("nodes", nodeID+".json"),
		filepath.Join("audit", nodeID+".jsonl"),
		filepath.Join("tasks", "*", "progress", nodeID+".jsonl"),
		filepath.Join("tasks", "*", "sessions", nodeID+".json"),
	}
}

// Task shards are matched by glob because a device cannot enumerate them: the
// set grows whenever any device in the fleet creates a task.
func TestReconcilePreservesTaskShardsAcrossTasks(t *testing.T) {
	origin := seededRemote(t)
	laptop := device(t, origin, "laptop")
	desktop := device(t, origin, "desktop")

	// Laptop works two tasks and pushes first.
	writeFile(t, laptop.Path, "tasks/auth/progress/laptop.jsonl", `{"node":"laptop"}`)
	writeFile(t, laptop.Path, "tasks/ui/progress/laptop.jsonl", `{"node":"laptop"}`)
	if _, err := laptop.Sync(context.Background(), "laptop works", ownedFor("laptop")); err != nil {
		t.Fatal(err)
	}

	// Desktop has not seen any of that, and writes shards of its own across
	// both tasks plus a handoff. All of it must survive the reconcile.
	writeFile(t, desktop.Path, "tasks/auth/progress/desktop.jsonl", `{"node":"desktop"}`)
	writeFile(t, desktop.Path, "tasks/ui/progress/desktop.jsonl", `{"node":"desktop"}`)
	writeFile(t, desktop.Path, "tasks/auth/sessions/desktop.json", `{"node":"desktop"}`)

	result, err := desktop.Sync(context.Background(), "desktop works", ownedFor("desktop"))
	if err != nil {
		t.Fatalf("desktop sync: %v", err)
	}
	if !result.Reconciled || !result.Pushed {
		t.Fatalf("result = %+v, want reconciled and pushed", result)
	}

	final := device(t, origin, "verify")
	for _, rel := range []string{
		"tasks/auth/progress/laptop.jsonl", "tasks/ui/progress/laptop.jsonl",
		"tasks/auth/progress/desktop.jsonl", "tasks/ui/progress/desktop.jsonl",
		"tasks/auth/sessions/desktop.json",
	} {
		if _, err := os.Stat(filepath.Join(final.Path, rel)); err != nil {
			t.Errorf("%s lost during reconcile: %v", rel, err)
		}
	}
}

// A hard reset to origin throws away every commit origin has not seen. Files
// this device *added* have to survive that, or two machines creating different
// tasks means the second one's work silently disappears.
func TestReconcileKeepsNewSharedFiles(t *testing.T) {
	origin := seededRemote(t)
	laptop := device(t, origin, "laptop")
	desktop := device(t, origin, "desktop")

	writeFile(t, laptop.Path, "tasks/auth/task.json", `{"id":"auth"}`)
	writeFile(t, laptop.Path, "memory/long-term/boot-failure.md", "dkms breaks on kernel upgrade")
	if _, err := laptop.Sync(context.Background(), "laptop", ownedFor("laptop")); err != nil {
		t.Fatal(err)
	}

	// Desktop has seen none of that and adds different shared files.
	writeFile(t, desktop.Path, "tasks/ui/task.json", `{"id":"ui"}`)
	writeFile(t, desktop.Path, "memory/long-term/gpu-driver.md", "nouveau only on this box")
	result, err := desktop.Sync(context.Background(), "desktop", ownedFor("desktop"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reconciled {
		t.Fatalf("result = %+v, want a reconcile", result)
	}

	final := device(t, origin, "verify")
	for _, rel := range []string{
		"tasks/auth/task.json", "tasks/ui/task.json",
		"memory/long-term/boot-failure.md", "memory/long-term/gpu-driver.md",
	} {
		if _, err := os.Stat(filepath.Join(final.Path, rel)); err != nil {
			t.Errorf("%s lost during reconcile: %v", rel, err)
		}
	}
}

// Rescuing additions must not resurrect our stale copy of a file origin also
// has — that would undo the concurrent edit the reconcile exists to respect.
func TestReconcileDoesNotResurrectStaleSharedFiles(t *testing.T) {
	origin := seededRemote(t)
	laptop := device(t, origin, "laptop")
	desktop := device(t, origin, "desktop")

	writeFile(t, laptop.Path, "config/manifest.json", `{"owner":"laptop"}`)
	if _, err := laptop.Sync(context.Background(), "laptop edits", ownedFor("laptop")); err != nil {
		t.Fatal(err)
	}

	// Desktop edits the same shared file from a stale base, plus adds one.
	writeFile(t, desktop.Path, "config/manifest.json", `{"owner":"desktop"}`)
	writeFile(t, desktop.Path, "memory/long-term/new.md", "brand new")
	if _, err := desktop.Sync(context.Background(), "desktop edits", ownedFor("desktop")); err != nil {
		t.Fatal(err)
	}

	final := device(t, origin, "verify")
	got, err := os.ReadFile(filepath.Join(final.Path, "config", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"owner":"laptop"}` {
		t.Errorf("manifest = %q, want origin's version to win", got)
	}
	if _, err := os.Stat(filepath.Join(final.Path, "memory", "long-term", "new.md")); err != nil {
		t.Errorf("the genuinely new file was lost: %v", err)
	}
}

// task.json is shared and deliberately NOT owned: whoever pushes first keeps
// the claim, which is what makes it a lease rather than an advisory note.
func TestReconcileLetsOriginWinTheTaskClaim(t *testing.T) {
	origin := seededRemote(t)
	laptop := device(t, origin, "laptop")
	desktop := device(t, origin, "desktop")

	writeFile(t, laptop.Path, "tasks/auth/task.json", `{"id":"auth","claim":{"node":"laptop"}}`)
	if _, err := laptop.Sync(context.Background(), "laptop claims", ownedFor("laptop")); err != nil {
		t.Fatal(err)
	}

	// Desktop claims the same task without having seen the laptop's push.
	writeFile(t, desktop.Path, "tasks/auth/task.json", `{"id":"auth","claim":{"node":"desktop"}}`)
	writeFile(t, desktop.Path, "tasks/auth/progress/desktop.jsonl", `{"node":"desktop"}`)
	if _, err := desktop.Sync(context.Background(), "desktop claims", ownedFor("desktop")); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(desktop.Path, "tasks", "auth", "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"node":"laptop"`) {
		t.Errorf("claim = %s, want the first pusher (laptop) to keep it", got)
	}

	// Losing the claim must not cost the device its own recorded work.
	if _, err := os.Stat(filepath.Join(desktop.Path, "tasks", "auth", "progress", "desktop.jsonl")); err != nil {
		t.Errorf("desktop lost its own progress along with the claim: %v", err)
	}
}
