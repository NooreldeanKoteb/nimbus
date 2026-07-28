package task

import (
	"errors"
	"testing"
	"time"
)

// seed creates a task claimed by a device, with its claim backdated so the
// lease can be reasoned about without waiting.
func seed(t *testing.T, repo, id, node string, claimedAgo time.Duration) *Task {
	t.Helper()
	task := &Task{ID: id, Goal: "goal for " + id, Status: StatusActive}
	if err := New(repo, task); err != nil {
		t.Fatal(err)
	}
	if node != "" {
		task.Claim = &Claim{Node: node, At: time.Now().UTC().Add(-claimedAgo)}
		if err := task.Save(repo); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

// A device working a task records progress; a device that has gone away does
// not. That is why the lease is measured against the timeline rather than
// against the claim, which never moves once taken.
func TestActivityRefreshesTheLease(t *testing.T) {
	repo := t.TempDir()
	task := seed(t, repo, "port-server", "laptop", 5*time.Hour)
	now := time.Now().UTC()

	if !task.LeaseExpired(repo, DefaultLease, now) {
		t.Fatal("a claim taken five hours ago with no activity is still live")
	}

	if err := Record(repo, task.ID, "laptop", KindNote, "still working"); err != nil {
		t.Fatal(err)
	}
	if task.LeaseExpired(repo, DefaultLease, now) {
		t.Error("recording progress did not refresh the lease")
	}
}

// Another device's activity is not evidence that the holder is alive.
func TestOnlyTheHoldersActivityCounts(t *testing.T) {
	repo := t.TempDir()
	task := seed(t, repo, "port-server", "laptop", 5*time.Hour)

	if err := Record(repo, task.ID, "desktop", KindNote, "watching from here"); err != nil {
		t.Fatal(err)
	}
	if !task.LeaseExpired(repo, DefaultLease, time.Now().UTC()) {
		t.Error("a different device's note kept the holder's lease alive")
	}
}

func TestUnclaimedTasksHaveNoLeaseToExpire(t *testing.T) {
	repo := t.TempDir()
	task := seed(t, repo, "port-server", "", 0)

	if task.LeaseExpired(repo, DefaultLease, time.Now().UTC()) {
		t.Error("an unclaimed task reported an expired lease rather than being free")
	}
}

// The capability filter is what makes the queue a queue: offering GPU work to
// a laptop only produces a claim that has to be handed back.
func TestStealableSkipsWorkThisDeviceCannotDo(t *testing.T) {
	repo := t.TempDir()

	gpu := &Task{ID: "train-model", Goal: "train it", Status: StatusActive, Needs: []string{"gpu:nvidia"}}
	if err := New(repo, gpu); err != nil {
		t.Fatal(err)
	}
	seed(t, repo, "port-server", "", 0)

	open, err := Stealable(repo, "laptop", []string{"os:linux"}, DefaultLease)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != "port-server" {
		t.Fatalf("queue = %v, want only the task this device can run", ids(open))
	}

	withGPU, err := Stealable(repo, "laptop", []string{"os:linux", "gpu:nvidia"}, DefaultLease)
	if err != nil {
		t.Fatal(err)
	}
	if len(withGPU) != 2 {
		t.Errorf("a GPU device sees %v, want both tasks", ids(withGPU))
	}
}

func TestStealableExcludesLiveClaimsAndOwnWork(t *testing.T) {
	repo := t.TempDir()
	seed(t, repo, "mine", "laptop", time.Minute)
	seed(t, repo, "theirs-live", "desktop", time.Minute)
	seed(t, repo, "theirs-lapsed", "desktop", 5*time.Hour)

	done := seed(t, repo, "finished", "", 0)
	done.Status = StatusDone
	if err := done.Save(repo); err != nil {
		t.Fatal(err)
	}

	open, err := Stealable(repo, "laptop", nil, DefaultLease)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != "theirs-lapsed" {
		t.Errorf("queue = %v, want only the task whose lease lapsed", ids(open))
	}
}

func TestContractBlocksCommandsThatAlwaysNeedAPerson(t *testing.T) {
	c := &Contract{RequireHuman: []string{"rm -rf *", "mkfs*", "reboot", "*--force*"}}

	blocked := []string{
		// The wildcard has to cross path separators, or the obvious pattern
		// silently matches nothing.
		"rm -rf /var/lib/postgresql",
		"mkfs.ext4 /dev/sdb1",
		"reboot",
		"sudo reboot now",
		"git push --force origin main",
	}
	for _, command := range blocked {
		if _, ok := c.NeedsHuman(command); !ok {
			t.Errorf("%q was not blocked", command)
		}
	}

	allowed := []string{"systemctl status nginx", "apt install -y ripgrep", "go test ./..."}
	for _, command := range allowed {
		if pattern, ok := c.NeedsHuman(command); ok {
			t.Errorf("%q was blocked by %q", command, pattern)
		}
	}

	// A task with no contract blocks nothing, and a nil one must not panic.
	var none *Contract
	if _, ok := none.NeedsHuman("rm -rf /"); ok {
		t.Error("a task with no contract blocked a command")
	}
}

func TestBootTaskFindsOnlyWorkThatAskedToSurviveARestart(t *testing.T) {
	repo := t.TempDir()

	ordinary := seed(t, repo, "ordinary", "laptop", time.Minute)
	if _, err := BootTask(repo, "laptop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a task with no contract was resumed on boot: %v", err)
	}

	ordinary.Resume = &Contract{OnBoot: true}
	if err := ordinary.Save(repo); err != nil {
		t.Fatal(err)
	}
	found, err := BootTask(repo, "laptop")
	if err != nil {
		t.Fatalf("the on-boot task was not found: %v", err)
	}
	if found.ID != "ordinary" {
		t.Errorf("found %s, want ordinary", found.ID)
	}

	// It is this device's task, not the fleet's.
	if _, err := BootTask(repo, "desktop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another device picked up this one's boot task: %v", err)
	}

	// A finished task does not come back after a reboot.
	found.Status = StatusDone
	if err := found.Save(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := BootTask(repo, "laptop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a completed task was resumed on boot: %v", err)
	}
}

func ids(tasks []*Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	return out
}
