// Package secrets provides encryption for values stored in the state repo.
//
// age is embedded as a library rather than invoked as the age or sops binary,
// so encrypted secrets can live in git on a device with nothing installed
// (DESIGN.md §11).
package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// ErrNoKey means no identity has been generated on this device yet.
var ErrNoKey = errors.New("no age key found (run `nimbus secrets keygen`)")

// Keypair is one device's age identity.
type Keypair struct {
	identity *age.X25519Identity
}

// Generate creates a fresh keypair.
func Generate() (*Keypair, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return &Keypair{identity: id}, nil
}

// Recipient returns the public half, safe to commit to the state repo.
func (k *Keypair) Recipient() string {
	return k.identity.Recipient().String()
}

// Save writes the private key with owner-only permissions.
func (k *Keypair) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save key: %w", err)
	}
	content := fmt.Sprintf(
		"# nimbus age identity — keep private, never commit\n# public key: %s\n%s\n",
		k.Recipient(), k.identity.String())
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("save key: %w", err)
	}
	return nil
}

// LoadKey reads a private key previously written by Save.
func LoadKey(path string) (*Keypair, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, fmt.Errorf("load key: %w", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, err := age.ParseX25519Identity(line)
		if err != nil {
			return nil, fmt.Errorf("load key: %w", err)
		}
		return &Keypair{identity: id}, nil
	}
	return nil, fmt.Errorf("load key: no identity found in %s", path)
}

// Encrypt seals plaintext to the given recipients, ASCII-armored so the output
// is diffable in git.
func Encrypt(plaintext []byte, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("encrypt: no recipients")
	}

	parsed := make([]age.Recipient, 0, len(recipients))
	for _, r := range recipients {
		rec, err := age.ParseX25519Recipient(r)
		if err != nil {
			return nil, fmt.Errorf("encrypt: bad recipient %q: %w", r, err)
		}
		parsed = append(parsed, rec)
	}

	var out bytes.Buffer
	armorWriter := armor.NewWriter(&out)
	w, err := age.Encrypt(armorWriter, parsed...)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	// Both writers must close in order: age first to flush its stream, then
	// armor to emit the trailing footer.
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if err := armorWriter.Close(); err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	return out.Bytes(), nil
}

// Decrypt opens ciphertext with this device's key.
func (k *Keypair) Decrypt(ciphertext []byte) ([]byte, error) {
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(ciphertext)), k.identity)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return out, nil
}
