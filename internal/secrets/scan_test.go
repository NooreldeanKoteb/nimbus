package secrets

import (
	"strings"
	"testing"
)

func TestScanFindsRealCredentialShapes(t *testing.T) {
	cases := map[string]string{
		"a GitHub token":              "GITHUB_TOKEN=ghp_" + strings.Repeat("a1B2", 9),
		"a GitHub fine-grained token": "token: github_pat_" + strings.Repeat("x", 60),
		"an AWS access key id":        "aws_access_key_id = AKIAIOSFODNN7EXAMPLE",
		"an Anthropic API key":        "ANTHROPIC_API_KEY=sk-ant-api03-" + strings.Repeat("z", 40),
		"an OpenAI API key":           "OPENAI_API_KEY=sk-" + strings.Repeat("Q", 48),
		"a Slack token":               "SLACK=xoxb-123456789012-abcdefghijkl",
		"a Google API key":            "key=AIza" + strings.Repeat("b", 35),
		"a private key":               "-----BEGIN OPENSSH PRIVATE KEY-----",
		"an age private key":          "AGE-SECRET-KEY-1" + strings.Repeat("Q", 50),
	}

	for want, content := range cases {
		findings := Scan([]byte(content))
		if len(findings) == 0 {
			t.Errorf("%s went undetected in %q", want, truncate(content))
			continue
		}
		if findings[0].Kind != want {
			t.Errorf("detected %q, want %q", findings[0].Kind, want)
		}
	}
}

// A false positive does not warn — it refuses a commit, and a device that
// cannot commit cannot sync. So the ordinary contents of a state repo have to
// come back clean.
func TestScanLeavesOrdinaryContentAlone(t *testing.T) {
	clean := []string{
		"# Nimbus context\n\nDevice: pop-os — linux/amd64\n",
		`{"id":"e740c0eee3cac76e91ab724d693d7c43","alias":"pop-os"}`,
		"the password prompt uses sudo's own PAM path\n",
		"api_key = os.Getenv(\"NIMBUS_TOKEN\")\n",
		"sha256: 9f2c4e1a8b7d6503f1e2a4b6c8d0e2f4a6b8c0d2e4f6a8b0c2d4e6f8a0b2c4d6",
		"age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
		"", "no secrets here at all",
	}
	for _, content := range clean {
		if findings := Scan([]byte(content)); len(findings) > 0 {
			t.Errorf("%q was flagged as %s", truncate(content), findings[0].Kind)
		}
	}
}

// A leak report that quotes the leak gets pasted into issue trackers, which
// publishes the credential the refusal just prevented.
func TestFindingsNeverCarryTheValue(t *testing.T) {
	token := "ghp_" + strings.Repeat("s3cr3t0", 6)
	findings := Scan([]byte("token = " + token))
	if len(findings) == 0 {
		t.Fatal("the token was not detected")
	}
	if strings.Contains(findings[0].String(), "s3cr3t0") {
		t.Errorf("the finding quotes the credential: %s", findings[0])
	}
	if !strings.Contains(findings[0].String(), "line 1") {
		t.Errorf("the finding does not locate the credential: %s", findings[0])
	}
}

// One key repeated across a file is one problem to fix, not forty.
func TestOneFindingPerKindPerFile(t *testing.T) {
	token := "ghp_" + strings.Repeat("a1B2", 9)
	content := strings.Repeat(token+"\n", 40)

	if findings := Scan([]byte(content)); len(findings) != 1 {
		t.Errorf("got %d findings for the same key repeated, want 1", len(findings))
	}
}

func TestBinaryContentIsSkipped(t *testing.T) {
	content := append([]byte("ghp_"+strings.Repeat("a1B2", 9)), 0x00, 0x01, 0x02)
	if findings := Scan(content); len(findings) > 0 {
		t.Errorf("binary content was scanned and flagged as %s", findings[0].Kind)
	}
}

func TestEncryptedContentIsRecognised(t *testing.T) {
	if !Encrypted([]byte("-----BEGIN AGE ENCRYPTED FILE-----\nabc\n")) {
		t.Error("age ciphertext was not recognised as encrypted")
	}
	if Encrypted([]byte("-----BEGIN OPENSSH PRIVATE KEY-----")) {
		t.Error("a private key was mistaken for ciphertext")
	}
}

func truncate(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "…"
}
