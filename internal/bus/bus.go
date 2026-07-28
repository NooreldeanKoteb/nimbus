// Package bus carries messages between devices through the state repo.
//
// DESIGN.md §6 originally specified a tailnet for this, embedded via tsnet.
// That is 547 Go modules and, more importantly, a second account to log into —
// which contradicts the zero-dependency rule (§2a) and the promise that logging
// into git is enough. Git is already the durable channel, already authenticated,
// already synced, and already survives a device being offline. So the mesh runs
// on git first; a tailnet can be added later behind this same interface purely
// as a latency optimization.
//
// The layout keeps every file owned by exactly one device, which is what makes
// it conflict-free under the reconcile in §4a:
//
//	bus/<from>/messages/<id>.json   written only by the sender
//	bus/<from>/receipts/<id>.json   written only by the receiver
//
// A sender never writes into the recipient's directory, and a recipient never
// edits a message. Delivery state is *derived* from the presence of a receipt
// rather than stored as a mutable field, so two devices can never disagree
// about it.
package bus

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Message kinds.
const (
	// KindNote is a message for a person or a Claude session to read.
	KindNote = "note"
	// KindDispatch hands a task to another device.
	KindDispatch = "dispatch"
	// KindReply answers an earlier message.
	KindReply = "reply"
)

// ErrNotFound is returned when no message has the given id.
var ErrNotFound = errors.New("message not found")

// Message is one thing sent from one device to another.
type Message struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Task links the message to a unit of work, so a dispatch carries the
	// context needed to act on it rather than just a sentence.
	Task string `json:"task,omitempty"`
	// ReplyTo threads an answer back to its question.
	ReplyTo string    `json:"reply_to,omitempty"`
	Sent    time.Time `json:"sent"`
}

// Receipt records that a device has seen a message. Written by the receiver
// into its own directory, which is why acknowledging never conflicts with the
// sender writing more messages.
type Receipt struct {
	ID       string    `json:"id"`
	Node     string    `json:"node"`
	Received time.Time `json:"received"`
	// Note optionally says what was done about it.
	Note string `json:"note,omitempty"`
}

// MessagePattern matches every message a device has sent.
func MessagePattern(nodeID string) string {
	return filepath.Join("bus", nodeID, "messages", "*.json")
}

// ReceiptPattern matches every receipt a device has written.
func ReceiptPattern(nodeID string) string {
	return filepath.Join("bus", nodeID, "receipts", "*.json")
}

func messageDir(repoPath, nodeID string) string {
	return filepath.Join(repoPath, "bus", nodeID, "messages")
}

func receiptDir(repoPath, nodeID string) string {
	return filepath.Join(repoPath, "bus", nodeID, "receipts")
}

// NewID returns a sortable, collision-resistant message id.
//
// Time-prefixed so a directory listing is chronological without opening every
// file, and random-suffixed so two devices sending in the same second cannot
// produce the same path.
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// Send writes a message into the sender's own outbox.
func Send(repoPath string, m *Message) (*Message, error) {
	if m.From == "" {
		return nil, errors.New("message has no sender")
	}
	if m.To == "" {
		return nil, errors.New("message has no recipient")
	}
	if m.To == m.From {
		return nil, errors.New("cannot send a message to this device")
	}
	if strings.TrimSpace(m.Text) == "" && m.Task == "" {
		return nil, errors.New("a message needs text or a task")
	}

	if m.ID == "" {
		m.ID = NewID()
	}
	if m.Kind == "" {
		m.Kind = KindNote
	}
	m.Sent = time.Now().UTC()

	dir := messageDir(repoPath, m.From)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, m.ID+".json"), append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	return m, nil
}

// Envelope pairs a message with its delivery state.
type Envelope struct {
	Message *Message
	Receipt *Receipt
}

// Delivered reports whether the recipient has acknowledged.
func (e Envelope) Delivered() bool { return e.Receipt != nil }

// All reads every message on the bus, oldest first.
//
// One unreadable message must not hide the rest of the traffic, matching how
// profiles, tasks, and memory already behave.
func All(repoPath string) ([]Envelope, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, "bus", "*", "messages", "*.json"))
	if err != nil {
		return nil, err
	}

	receipts, err := loadReceipts(repoPath)
	if err != nil {
		return nil, err
	}

	envelopes := make([]Envelope, 0, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil || m.ID == "" {
			continue
		}
		envelopes = append(envelopes, Envelope{Message: &m, Receipt: receipts[m.ID]})
	}

	sort.SliceStable(envelopes, func(i, j int) bool {
		return envelopes[i].Message.Sent.Before(envelopes[j].Message.Sent)
	})
	return envelopes, nil
}

func loadReceipts(repoPath string) (map[string]*Receipt, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, "bus", "*", "receipts", "*.json"))
	if err != nil {
		return nil, err
	}

	receipts := make(map[string]*Receipt, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r Receipt
		if err := json.Unmarshal(data, &r); err != nil || r.ID == "" {
			continue
		}
		receipts[r.ID] = &r
	}
	return receipts, nil
}

// Inbox returns messages addressed to a device. Unacknowledged only, unless
// includeRead is set.
func Inbox(repoPath, nodeID string, includeRead bool) ([]Envelope, error) {
	all, err := All(repoPath)
	if err != nil {
		return nil, err
	}

	var inbox []Envelope
	for _, e := range all {
		if e.Message.To != nodeID {
			continue
		}
		if !includeRead && e.Delivered() {
			continue
		}
		inbox = append(inbox, e)
	}
	return inbox, nil
}

// Outbox returns messages a device has sent, newest first, so the most recent
// thing you said is the first thing you see.
func Outbox(repoPath, nodeID string) ([]Envelope, error) {
	all, err := All(repoPath)
	if err != nil {
		return nil, err
	}

	var outbox []Envelope
	for _, e := range all {
		if e.Message.From == nodeID {
			outbox = append(outbox, e)
		}
	}
	for i, j := 0, len(outbox)-1; i < j; i, j = i+1, j-1 {
		outbox[i], outbox[j] = outbox[j], outbox[i]
	}
	return outbox, nil
}

// Load finds one message by id.
func Load(repoPath, id string) (*Message, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, "bus", "*", "messages", id+".json"))
	if err != nil || len(matches) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	data, err := os.ReadFile(matches[0])
	if err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse message %s: %w", id, err)
	}
	return &m, nil
}

// Ack records that this device has seen a message.
//
// Written into the receiver's own directory rather than as a flag on the
// message, so acknowledging cannot race the sender and the sender's copy stays
// exactly as it was sent.
func Ack(repoPath, nodeID, id, note string) (*Receipt, error) {
	m, err := Load(repoPath, id)
	if err != nil {
		return nil, err
	}
	if m.To != nodeID {
		return nil, fmt.Errorf("message %s was not addressed to this device", id)
	}

	r := &Receipt{ID: id, Node: nodeID, Received: time.Now().UTC(), Note: note}
	dir := receiptDir(repoPath, nodeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ack: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ack: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("ack: %w", err)
	}
	return r, nil
}
