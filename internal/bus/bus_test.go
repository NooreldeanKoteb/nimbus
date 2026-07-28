package bus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func send(t *testing.T, repo, from, to, text string) *Message {
	t.Helper()
	m, err := Send(repo, &Message{From: from, To: to, Text: text})
	if err != nil {
		t.Fatalf("Send(%s->%s): %v", from, to, err)
	}
	return m
}

func TestSendAndReceive(t *testing.T) {
	repo := t.TempDir()
	sent := send(t, repo, "laptop", "desktop", "the server build is failing")

	inbox, err := Inbox(repo, "desktop", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 {
		t.Fatalf("inbox has %d messages, want 1", len(inbox))
	}
	if inbox[0].Message.Text != sent.Text {
		t.Errorf("Text = %q, want %q", inbox[0].Message.Text, sent.Text)
	}
	if inbox[0].Delivered() {
		t.Error("a message nobody acknowledged reports as delivered")
	}

	// The sender must not see its own message in its inbox.
	if mine, _ := Inbox(repo, "laptop", false); len(mine) != 0 {
		t.Errorf("sender's inbox = %v, want empty", mine)
	}
}

// Every file belongs to exactly one device, which is what makes the bus
// conflict-free under a reconcile.
func TestSenderAndReceiverWriteSeparateFiles(t *testing.T) {
	repo := t.TempDir()
	m := send(t, repo, "laptop", "desktop", "hello")

	if _, err := os.Stat(filepath.Join(repo, "bus", "laptop", "messages", m.ID+".json")); err != nil {
		t.Errorf("message is not in the sender's directory: %v", err)
	}

	if _, err := Ack(repo, "desktop", m.ID, "on it"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "bus", "desktop", "receipts", m.ID+".json")); err != nil {
		t.Errorf("receipt is not in the receiver's directory: %v", err)
	}

	// The message itself must be untouched: the receiver never edits it.
	loaded, err := Load(repo, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Text != "hello" || loaded.From != "laptop" {
		t.Errorf("message was modified by acknowledging: %+v", loaded)
	}
}

func TestOwnedPatternsMatchWhatIsWritten(t *testing.T) {
	repo := t.TempDir()
	m := send(t, repo, "laptop", "desktop", "hello")
	if _, err := Ack(repo, "desktop", m.ID, ""); err != nil {
		t.Fatal(err)
	}

	// The sync layer reapplies these after a reset to origin, so each pattern
	// must match its own device's files and nothing else.
	mine, _ := filepath.Glob(filepath.Join(repo, MessagePattern("laptop")))
	if len(mine) != 1 {
		t.Errorf("message pattern matched %d files, want 1", len(mine))
	}
	theirs, _ := filepath.Glob(filepath.Join(repo, ReceiptPattern("desktop")))
	if len(theirs) != 1 {
		t.Errorf("receipt pattern matched %d files, want 1", len(theirs))
	}
	// A device must not own the other side of the exchange.
	if crossed, _ := filepath.Glob(filepath.Join(repo, ReceiptPattern("laptop"))); len(crossed) != 0 {
		t.Errorf("sender owns a receipt it did not write: %v", crossed)
	}
}

// Delivery is derived from the presence of a receipt rather than stored as a
// mutable flag, so two devices can never disagree about it.
func TestDeliveryStateIsDerived(t *testing.T) {
	repo := t.TempDir()
	m := send(t, repo, "laptop", "desktop", "please check the logs")

	out, _ := Outbox(repo, "laptop")
	if len(out) != 1 || out[0].Delivered() {
		t.Fatalf("outbox = %v, want one undelivered message", out)
	}

	if _, err := Ack(repo, "desktop", m.ID, "logs were clean"); err != nil {
		t.Fatal(err)
	}

	out, _ = Outbox(repo, "laptop")
	if len(out) != 1 || !out[0].Delivered() {
		t.Fatalf("outbox = %v, want the message marked delivered", out)
	}
	if out[0].Receipt.Note != "logs were clean" {
		t.Errorf("reply note = %q, want it carried back to the sender", out[0].Receipt.Note)
	}
}

func TestInboxHidesAcknowledged(t *testing.T) {
	repo := t.TempDir()
	m := send(t, repo, "laptop", "desktop", "first")
	send(t, repo, "laptop", "desktop", "second")

	if _, err := Ack(repo, "desktop", m.ID, ""); err != nil {
		t.Fatal(err)
	}

	unread, _ := Inbox(repo, "desktop", false)
	if len(unread) != 1 || unread[0].Message.Text != "second" {
		t.Errorf("unread = %v, want only the unacknowledged message", unread)
	}
	all, _ := Inbox(repo, "desktop", true)
	if len(all) != 2 {
		t.Errorf("--all returned %d, want 2", len(all))
	}
}

// Acknowledging someone else's mail would let a device silently swallow a
// message meant for a third machine.
func TestAckRejectsMessagesForOtherDevices(t *testing.T) {
	repo := t.TempDir()
	m := send(t, repo, "laptop", "desktop", "for desktop only")

	if _, err := Ack(repo, "server", m.ID, ""); err == nil {
		t.Error("a third device acknowledged a message it was not sent")
	}
	if _, err := Ack(repo, "desktop", "no-such-message", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Ack() on a missing message = %v, want ErrNotFound", err)
	}
}

func TestSendValidates(t *testing.T) {
	repo := t.TempDir()

	cases := map[string]*Message{
		"no sender":    {To: "desktop", Text: "x"},
		"no recipient": {From: "laptop", Text: "x"},
		"no content":   {From: "laptop", To: "desktop", Text: "   "},
		"self":         {From: "laptop", To: "laptop", Text: "x"},
	}
	for name, m := range cases {
		if _, err := Send(repo, m); err == nil {
			t.Errorf("Send(%s) = nil error, want rejection", name)
		}
	}

	// A dispatch carries a task instead of text, and that is enough.
	if _, err := Send(repo, &Message{From: "laptop", To: "desktop", Kind: KindDispatch, Task: "port"}); err != nil {
		t.Errorf("Send() rejected a task-only dispatch: %v", err)
	}
}

// Two devices sending in the same second must not collide on a path.
func TestIDsAreUniqueAndSortable(t *testing.T) {
	seen := make(map[string]bool)
	var previous string
	for range 200 {
		id := NewID()
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
		// The time prefix makes a directory listing chronological without
		// opening every file.
		if previous != "" && id[:15] < previous[:15] {
			t.Errorf("ids are not sortable: %q came after %q", id, previous)
		}
		previous = id
	}
}

func TestAllIsChronologicalAcrossSenders(t *testing.T) {
	repo := t.TempDir()
	send(t, repo, "laptop", "desktop", "first")
	send(t, repo, "server", "desktop", "second")
	send(t, repo, "laptop", "desktop", "third")

	all, err := All(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("All() returned %d, want 3", len(all))
	}
	for i, want := range []string{"first", "second", "third"} {
		if all[i].Message.Text != want {
			t.Errorf("message %d = %q, want %q", i, all[i].Message.Text, want)
		}
	}
}

func TestAllSkipsUnreadableMessages(t *testing.T) {
	repo := t.TempDir()
	send(t, repo, "laptop", "desktop", "good")

	broken := filepath.Join(repo, "bus", "laptop", "messages", "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	all, err := All(repo)
	if err != nil {
		t.Fatalf("All() error = %v, want the bad file skipped", err)
	}
	if len(all) != 1 {
		t.Errorf("All() returned %d, want the 1 readable message", len(all))
	}
}

func TestReplyThreading(t *testing.T) {
	repo := t.TempDir()
	question := send(t, repo, "laptop", "desktop", "why is the build failing?")

	reply, err := Send(repo, &Message{
		From: "desktop", To: "laptop", Kind: KindReply,
		Text: "missing libssl", ReplyTo: question.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ReplyTo != question.ID {
		t.Errorf("ReplyTo = %q, want %q", reply.ReplyTo, question.ID)
	}

	inbox, _ := Inbox(repo, "laptop", false)
	if len(inbox) != 1 || !strings.Contains(inbox[0].Message.Text, "libssl") {
		t.Errorf("reply did not reach the original sender: %v", inbox)
	}
}

// An exec message carries the command in its own field. Text is for people;
// putting the command there too would mean a recipient parsing a sentence to
// find the thing it is about to run.
func TestAnExecMessageNeedsACommand(t *testing.T) {
	repo := t.TempDir()

	if _, err := Send(repo, &Message{
		From: "laptop", To: "desktop", Kind: KindExec, Text: "please run something",
	}); err == nil {
		t.Fatal("an exec message with no command was accepted")
	}

	m, err := Send(repo, &Message{
		From: "laptop", To: "desktop", Kind: KindExec, Command: "systemctl status nimbus",
	})
	if err != nil {
		t.Fatalf("an exec message with a command but no text was rejected: %v", err)
	}

	inbox, _ := Inbox(repo, "desktop", false)
	if len(inbox) != 1 || inbox[0].Message.Command != "systemctl status nimbus" {
		t.Errorf("the command did not survive the round trip: %+v", inbox)
	}
	if inbox[0].Message.ID != m.ID {
		t.Errorf("id changed in transit")
	}
}

// The streaming half of peer execution: output has to be readable before the
// command that is producing it has finished.
func TestOutputIsReadableWhileItIsStillBeingWritten(t *testing.T) {
	repo := t.TempDir()
	m := send(t, repo, "laptop", "desktop", "run the build")

	if out, err := Output(repo, m.ID); err != nil || out != "" {
		t.Errorf("output before anything ran = %q, %v; want empty and no error", out, err)
	}

	if err := AppendOutput(repo, "desktop", m.ID, []byte("step 1\n")); err != nil {
		t.Fatal(err)
	}
	partial, err := Output(repo, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if partial != "step 1\n" {
		t.Errorf("partial output = %q, want the first chunk", partial)
	}

	if err := AppendOutput(repo, "desktop", m.ID, []byte("step 2\n")); err != nil {
		t.Fatal(err)
	}
	full, _ := Output(repo, m.ID)
	if full != "step 1\nstep 2\n" {
		t.Errorf("output = %q, want both chunks in order", full)
	}
}

// Message ids come from NewID, but a person types one at `nimbus exec --follow`.
// A path built from unvalidated input is a path that can be aimed anywhere.
func TestOutputPathsCannotBeAimedOutsideTheRepo(t *testing.T) {
	repo := t.TempDir()

	for _, id := range []string{"../../etc/passwd", "a/b", "", strings.Repeat("x", 65), "a;b"} {
		if err := AppendOutput(repo, "desktop", id, []byte("x")); err == nil {
			t.Errorf("AppendOutput accepted %q as a message id", id)
		}
		if _, err := Output(repo, id); err == nil {
			t.Errorf("Output accepted %q as a message id", id)
		}
	}
}

// A result is an ordinary message, found by threading rather than by a second
// delivery mechanism that could fall out of step with the first.
func TestAnswerFindsTheResultForARequest(t *testing.T) {
	repo := t.TempDir()
	request, err := Send(repo, &Message{
		From: "laptop", To: "desktop", Kind: KindExec, Command: "uname -a",
	})
	if err != nil {
		t.Fatal(err)
	}

	answer, err := Answer(repo, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if answer != nil {
		t.Fatalf("a result appeared before anything ran: %+v", answer)
	}

	if _, err := Send(repo, &Message{
		From: "desktop", To: "laptop", Kind: KindResult,
		ReplyTo: request.ID, Exit: 0, Text: "Linux",
	}); err != nil {
		t.Fatal(err)
	}

	answer, err = Answer(repo, request.ID)
	if err != nil || answer == nil {
		t.Fatalf("the result was not threaded back: %v %v", answer, err)
	}
	if answer.Exit != 0 || answer.Text != "Linux" {
		t.Errorf("result = %+v, want exit 0 and the output", answer)
	}

	// A plain reply is not a result: an exec is finished by a status, and a
	// person answering the thread must not look like the command returning.
	if _, err := Send(repo, &Message{
		From: "desktop", To: "laptop", Kind: KindReply,
		ReplyTo: request.ID, Text: "looks fine to me",
	}); err != nil {
		t.Fatal(err)
	}
	answer, _ = Answer(repo, request.ID)
	if answer == nil || answer.Kind != KindResult {
		t.Errorf("Answer returned a %q, want only a result", answer.Kind)
	}
}
