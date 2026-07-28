package audit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newLog(t *testing.T) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "sub", "audit.jsonl"), "desktop")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return l
}

func TestAppendAssignsSequenceAndNode(t *testing.T) {
	l := newLog(t)

	for i := 1; i <= 3; i++ {
		e, err := l.Append(Entry{Actor: "local", Action: "install"})
		if err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		if e.Seq != uint64(i) {
			t.Errorf("Seq = %d, want %d", e.Seq, i)
		}
		if e.Node != "desktop" {
			t.Errorf("Node = %q, want desktop", e.Node)
		}
		if e.Timestamp.IsZero() {
			t.Error("Timestamp not set")
		}
	}
}

func TestEmptyLogReadsAsEmpty(t *testing.T) {
	entries, err := newLog(t).Entries()
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Entries() = %d, want 0", len(entries))
	}
}

func TestChainLinksEntries(t *testing.T) {
	l := newLog(t)
	first, _ := l.Append(Entry{Action: "a"})
	second, _ := l.Append(Entry{Action: "b"})

	if first.PrevHash != "" {
		t.Errorf("first PrevHash = %q, want empty", first.PrevHash)
	}
	if second.PrevHash != first.Hash {
		t.Errorf("second PrevHash = %q, want %q", second.PrevHash, first.Hash)
	}
	if second.Hash == first.Hash {
		t.Error("distinct entries produced identical hashes")
	}
}

func TestVerifyAcceptsIntactLog(t *testing.T) {
	l := newLog(t)
	for _, a := range []string{"a", "b", "c"} {
		if _, err := l.Append(Entry{Action: a}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("Verify() on intact log error = %v", err)
	}
}

// Editing history is the attack this design exists to catch.
func TestVerifyDetectsEditedEntry(t *testing.T) {
	l := newLog(t)
	_, _ = l.Append(Entry{Action: "install", Target: "curl"})
	_, _ = l.Append(Entry{Action: "install", Target: "wget"})

	data, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"target":"curl"`, `"target":"rm-rf"`, 1)
	if tampered == string(data) {
		t.Fatal("test setup failed: no substitution made")
	}
	if err := os.WriteFile(l.Path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	err = l.Verify()
	if err == nil {
		t.Fatal("Verify() accepted a tampered log")
	}
	if !strings.Contains(err.Error(), "modified") {
		t.Errorf("Verify() error = %v, want a modification error", err)
	}
}

// Deleting an entry breaks both sequence and chain continuity.
func TestVerifyDetectsDeletedEntry(t *testing.T) {
	l := newLog(t)
	for _, a := range []string{"a", "b", "c"} {
		_, _ = l.Append(Entry{Action: a})
	}

	data, _ := os.ReadFile(l.Path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	// Drop the middle entry.
	kept := lines[0] + "\n" + lines[2] + "\n"
	if err := os.WriteFile(l.Path, []byte(kept), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := l.Verify(); err == nil {
		t.Fatal("Verify() accepted a log with a deleted entry")
	}
}

func TestVerifyDetectsAppendedForgery(t *testing.T) {
	l := newLog(t)
	_, _ = l.Append(Entry{Action: "real"})

	// An attacker appending a well-formed line cannot know the correct chain
	// hash without recomputing it, so a naive forgery is caught.
	forged := `{"seq":2,"ts":"2026-07-26T00:00:00Z","node":"desktop","actor":"x",` +
		`"action":"forged","result":"ok","prev_hash":"wrong","hash":"alsowrong"}`
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(forged + "\n")
	f.Close()

	if err := l.Verify(); err == nil {
		t.Fatal("Verify() accepted a forged entry")
	}
}

func TestRecordCapturesError(t *testing.T) {
	l := newLog(t)
	l.Record("local", "install", "docker", "apt-get install docker", errors.New("exit status 100"))

	entries, err := l.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() = %d, want 1", len(entries))
	}
	if entries[0].Result != ResultError {
		t.Errorf("Result = %q, want %q", entries[0].Result, ResultError)
	}
	if !strings.Contains(entries[0].Detail, "exit status 100") {
		t.Errorf("Detail = %q, want it to include the error", entries[0].Detail)
	}
}

func TestRecordSuccessDefaultsToOK(t *testing.T) {
	l := newLog(t)
	l.Record("local", "install", "curl", "", nil)

	entries, _ := l.Entries()
	if len(entries) != 1 || entries[0].Result != ResultOK {
		t.Fatalf("entries = %+v, want one ok entry", entries)
	}
}

// Rollback is recorded so an operator can undo a change they did not make.
func TestRollbackIsPersisted(t *testing.T) {
	l := newLog(t)
	_, err := l.Append(Entry{
		Action:   "system.change",
		Target:   "/etc/fstab",
		Rollback: "cp /etc/fstab.nimbus-backup /etc/fstab",
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, _ := l.Entries()
	if entries[0].Rollback == "" {
		t.Error("Rollback not persisted")
	}
	if err := l.Verify(); err != nil {
		t.Errorf("Verify() error = %v", err)
	}
}

func TestEntriesSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	first, _ := Open(path, "desktop")
	_, _ = first.Append(Entry{Action: "a"})

	second, err := Open(path, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Append(Entry{Action: "b"}); err != nil {
		t.Fatal(err)
	}

	entries, _ := second.Entries()
	if len(entries) != 2 {
		t.Fatalf("Entries() = %d, want 2", len(entries))
	}
	if err := second.Verify(); err != nil {
		t.Errorf("Verify() across reopen error = %v", err)
	}
}
