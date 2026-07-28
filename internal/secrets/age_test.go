package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	plaintext := []byte("ANTHROPIC_API_KEY=sk-ant-not-a-real-key")
	ciphertext, err := Encrypt(plaintext, []string{kp.Recipient()})
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("ciphertext contains plaintext")
	}

	got, err := kp.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Decrypt() = %q, want %q", got, plaintext)
	}
}

// Armored output is what makes encrypted secrets reviewable in a git diff.
func TestEncryptOutputIsArmored(t *testing.T) {
	kp, _ := Generate()
	ciphertext, err := Encrypt([]byte("x"), []string{kp.Recipient()})
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if !strings.HasPrefix(string(ciphertext), "-----BEGIN AGE ENCRYPTED FILE-----") {
		t.Errorf("output not armored, got prefix %q", string(ciphertext[:min(40, len(ciphertext))]))
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	mine, _ := Generate()
	theirs, _ := Generate()

	ciphertext, err := Encrypt([]byte("secret"), []string{theirs.Recipient()})
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if _, err := mine.Decrypt(ciphertext); err == nil {
		t.Fatal("Decrypt() with wrong key should fail")
	}
}

func TestEncryptToMultipleRecipients(t *testing.T) {
	a, _ := Generate()
	b, _ := Generate()

	ciphertext, err := Encrypt([]byte("shared"), []string{a.Recipient(), b.Recipient()})
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	for name, kp := range map[string]*Keypair{"a": a, "b": b} {
		got, err := kp.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("%s could not decrypt: %v", name, err)
		}
		if string(got) != "shared" {
			t.Errorf("%s decrypted %q, want shared", name, got)
		}
	}
}

func TestEncryptRequiresRecipients(t *testing.T) {
	if _, err := Encrypt([]byte("x"), nil); err == nil {
		t.Fatal("Encrypt() with no recipients should fail")
	}
}

func TestEncryptRejectsBadRecipient(t *testing.T) {
	if _, err := Encrypt([]byte("x"), []string{"not-a-key"}); err == nil {
		t.Fatal("Encrypt() with malformed recipient should fail")
	}
}

func TestSaveAndLoadKey(t *testing.T) {
	kp, _ := Generate()
	path := filepath.Join(t.TempDir(), "nested", "age.key")

	if err := kp.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey() error = %v", err)
	}
	if loaded.Recipient() != kp.Recipient() {
		t.Errorf("recipient = %q, want %q", loaded.Recipient(), kp.Recipient())
	}

	// A round trip through disk must preserve decryption ability.
	ciphertext, _ := Encrypt([]byte("persisted"), []string{kp.Recipient()})
	got, err := loaded.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt() after reload error = %v", err)
	}
	if string(got) != "persisted" {
		t.Errorf("decrypted %q, want persisted", got)
	}
}

// The private key must never be group- or world-readable.
func TestSavedKeyIsOwnerOnly(t *testing.T) {
	kp, _ := Generate()
	path := filepath.Join(t.TempDir(), "age.key")
	if err := kp.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key mode = %o, want 600", perm)
	}
}

func TestLoadKeyMissingReturnsErrNoKey(t *testing.T) {
	_, err := LoadKey(filepath.Join(t.TempDir(), "absent.key"))
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("LoadKey() error = %v, want ErrNoKey", err)
	}
}

func TestLoadKeySkipsComments(t *testing.T) {
	kp, _ := Generate()
	path := filepath.Join(t.TempDir(), "age.key")
	if err := kp.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, _ := os.ReadFile(path)
	if !bytes.HasPrefix(data, []byte("#")) {
		t.Fatal("expected saved key to start with a comment header")
	}
	if _, err := LoadKey(path); err != nil {
		t.Errorf("LoadKey() should skip comment lines, got %v", err)
	}
}

func TestLoadKeyRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(path, []byte("# header\nnot-a-valid-identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(path); err == nil {
		t.Fatal("LoadKey() with garbage should fail")
	}
}
