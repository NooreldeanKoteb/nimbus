// Package audit records every action nimbus takes on a device.
//
// The log is append-only and hash-chained: each entry commits to the hash of
// its predecessor, so removing or editing a past entry invalidates every entry
// after it. That property is what makes the log usable as evidence rather than
// merely as a debugging aid — which matters most in the support case, where the
// person reading the log is not the person who ran the commands.
//
// Consent gating is deliberately not implemented yet; when it is, it hooks in
// here, because every gated action is already an audited action.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Result classifies how an action ended.
const (
	ResultOK      = "ok"
	ResultError   = "error"
	ResultSkipped = "skipped"
)

// Entry is one recorded action.
type Entry struct {
	Seq       uint64    `json:"seq"`
	Timestamp time.Time `json:"ts"`
	Node      string    `json:"node"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Target    string    `json:"target,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	Result    string    `json:"result"`
	// Rollback is the command that undoes this action, recorded before the
	// action runs. An unrecoverable change with no rollback must say so.
	Rollback string `json:"rollback,omitempty"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// Log is an append-only audit file for one device.
type Log struct {
	Path string
	Node string

	mu sync.Mutex
}

// Open prepares a log, creating parent directories as needed.
func Open(path, node string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return &Log{Path: path, Node: node}, nil
}

// Append writes an entry, chaining it to the current tail.
func (l *Log) Append(e Entry) (*Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := l.readAll()
	if err != nil {
		return nil, err
	}

	e.Seq = uint64(len(entries)) + 1
	e.Node = l.Node
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.Result == "" {
		e.Result = ResultOK
	}
	if len(entries) > 0 {
		e.PrevHash = entries[len(entries)-1].Hash
	}
	e.Hash = hashEntry(e)

	line, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("append audit entry: %w", err)
	}

	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("append audit entry: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("append audit entry: %w", err)
	}
	return &e, nil
}

// Record is the common case: log an action and its outcome in one call.
func (l *Log) Record(actor, action, target, detail string, err error) {
	e := Entry{Actor: actor, Action: action, Target: target, Detail: detail, Result: ResultOK}
	if err != nil {
		e.Result = ResultError
		e.Detail = strings.TrimSpace(detail + ": " + err.Error())
	}
	// An audit write failure must not mask the caller's own error, so this
	// deliberately returns nothing; Verify surfaces gaps later.
	_, _ = l.Append(e)
}

// Entries returns the full log in order.
func (l *Log) Entries() ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readAll()
}

// Verify walks the hash chain and reports the first inconsistency.
func (l *Log) Verify() error {
	entries, err := l.Entries()
	if err != nil {
		return err
	}

	var prev string
	for i, e := range entries {
		if want := uint64(i + 1); e.Seq != want {
			return fmt.Errorf("entry %d: sequence is %d, expected %d", i+1, e.Seq, want)
		}
		if e.PrevHash != prev {
			return fmt.Errorf("entry %d: chain broken (prev_hash does not match entry %d)", e.Seq, i)
		}
		if got := hashEntry(e); got != e.Hash {
			return fmt.Errorf("entry %d: content has been modified", e.Seq)
		}
		prev = e.Hash
	}
	return nil
}

func (l *Log) readAll() ([]Entry, error) {
	f, err := os.Open(l.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read audit log: %w", err)
	}
	defer f.Close()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	// Detail fields can carry command output, so allow generous lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("read audit log: malformed entry %d: %w", len(entries)+1, err)
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read audit log: %w", err)
	}
	return entries, nil
}

// hashEntry computes the chain hash over every field except Hash itself.
func hashEntry(e Entry) string {
	e.Hash = ""
	// json.Marshal on a struct is field-ordered and therefore deterministic,
	// which is what the chain requires.
	data, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
