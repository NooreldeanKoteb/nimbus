// Package device profiles the machine nimbus is running on.
//
// This is the foundation for both halves of the product: Claude needs to know
// what it is running on to act correctly (DESIGN.md §9), and a central operator
// needs the same profile to support a device they cannot physically see.
package device

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Identity is a device's stable name in the fleet.
type Identity struct {
	ID string `json:"id"`
	// Alias is the human-readable name for this device — "kali-thinkpad",
	// "build-server". It is a label over the ID, never a replacement for it:
	// the ID appears in every state repo path and inside every hash-chained
	// audit entry, so renaming it would orphan the history and invalidate the
	// chain. Aliases can therefore be changed freely and as often as you like.
	Alias    string    `json:"alias,omitempty"`
	Hostname string    `json:"hostname"`
	Created  time.Time `json:"created"`
}

// Label is what a person should see for this device.
func (i *Identity) Label() string {
	if i.Alias != "" {
		return i.Alias
	}
	return ShortID(i.ID)
}

// ShortID abbreviates a machine-derived id for display. A full one is 32 hex
// characters, which tells a reader nothing and wraps every table.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// AliasMaxLen bounds an alias so fleet listings stay aligned.
const AliasMaxLen = 32

// ValidateAlias rejects names that cannot be typed as a command argument or
// that would be mistaken for something else on the CLI.
func ValidateAlias(alias string) error {
	if alias == "" {
		return errors.New("alias is empty")
	}
	if len(alias) > AliasMaxLen {
		return fmt.Errorf("alias %q is longer than %d characters", alias, AliasMaxLen)
	}
	if strings.HasPrefix(alias, "-") {
		return fmt.Errorf("alias %q cannot start with a dash", alias)
	}
	for _, r := range alias {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("alias %q: use lowercase letters, digits, - and _ only", alias)
		}
	}
	return nil
}

// DefaultAlias derives a readable name from a hostname, so a device is never
// presented as a hex string just because nobody named it yet.
//
// The domain is dropped: "studio.local" and "studio.lan" are the same machine
// to a person, and the short form is what they would have typed anyway.
func DefaultAlias(hostname string) string {
	host, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(hostname)), ".")

	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			// Substitute rather than drop, so "my pc" reads as "my-pc" and
			// not "mypc".
			b.WriteRune('-')
		}
	}

	alias := strings.Trim(b.String(), "-")
	if len(alias) > AliasMaxLen {
		alias = strings.Trim(alias[:AliasMaxLen], "-")
	}
	if ValidateAlias(alias) != nil {
		return ""
	}
	return alias
}

// SetAlias renames this device and persists the change.
func (i *Identity) SetAlias(path, alias string) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	i.Alias = alias
	return i.Save(path)
}

// Save persists the identity. Written 0600 alongside credentials because it
// stays local: the ID travels to the fleet through the published profile.
func (i *Identity) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save identity: %w", err)
	}
	data, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return fmt.Errorf("save identity: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("save identity: %w", err)
	}
	return nil
}

// LoadOrCreateIdentity returns this device's identity, generating and
// persisting one on first run.
//
// The ID must survive reboots, hostname changes, and OS upgrades, because task
// affinity and audit records key off it. Where the platform offers a stable
// machine id we derive from that; otherwise we generate one and store it.
func LoadOrCreateIdentity(path string) (*Identity, error) {
	if data, err := os.ReadFile(path); err == nil {
		var id Identity
		if err := json.Unmarshal(data, &id); err == nil && id.ID != "" {
			// Hostname is refreshed on every load; it is a label, not the key.
			if hn, err := os.Hostname(); err == nil {
				id.Hostname = hn
			}
			// An explicit override has to beat the stored value, not merely seed
			// a new one. The case it exists for is a VM clone, where node.json
			// was copied along with /etc/machine-id — so by definition there is
			// already a stored identity, and only overriding it fixes anything.
			//
			// Deliberately not persisted. Writing it here would mean any command
			// run with the variable set silently and permanently rebrands the
			// device, which is far too much to happen as a side effect. A clone
			// exports it the way any other machine-scoped setting is exported.
			if override := nodeIDOverride(); override != "" {
				id.ID = override
			}
			// Devices enrolled before aliases existed have none. Fill one in
			// rather than showing a hex string forever.
			if id.Alias == "" {
				if alias := DefaultAlias(id.Hostname); alias != "" {
					id.Alias = alias
					_ = id.Save(path)
				}
			}
			return &id, nil
		}
		// Fall through and regenerate on a corrupt file rather than hard-fail;
		// a device that cannot identify itself is useless.
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}

	id := &Identity{
		ID:       machineID(),
		Alias:    DefaultAlias(hostname),
		Hostname: hostname,
		Created:  time.Now().UTC(),
	}

	if err := id.Save(path); err != nil {
		return nil, err
	}
	return id, nil
}

// NodeIDEnv overrides the derived machine identifier.
//
// Necessary because /etc/machine-id is not as unique as it looks: VM clones,
// golden images, and containers built from one base all carry the same value.
// Two nodes sharing an ID would overwrite each other's profile and, worse,
// each other's audit log — which is exactly the record that is supposed to be
// tamper-evident. Setting this on the clone is the fix.
const NodeIDEnv = "NIMBUS_NODE_ID"

// nodeIDOverride reads the explicit node id override, or "" when unset.
//
// The empty check must happen *before* sanitizing: sanitizeID never returns an
// empty string, it returns the "node-unknown" placeholder. Sanitizing first
// would make an unset variable look like a deliberate override to that
// placeholder — which would give every device the same id and point their
// profiles and audit logs at the same files.
func nodeIDOverride() string {
	raw := strings.TrimSpace(os.Getenv(NodeIDEnv))
	if raw == "" {
		return ""
	}
	return sanitizeID(raw)
}

// machineID derives a stable identifier from the platform, falling back to
// random bytes when no such identifier is available.
func machineID() string {
	if v := nodeIDOverride(); v != "" {
		return v
	}
	if id := platformMachineID(); id != "" {
		return sanitizeID(id)
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("node-%d", time.Now().UnixNano())
	}
	return "node-" + hex.EncodeToString(buf)
}

func platformMachineID() string {
	switch runtime.GOOS {
	case "linux":
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if data, err := os.ReadFile(p); err == nil {
				if v := strings.TrimSpace(string(data)); v != "" {
					return v
				}
			}
		}
	case "darwin":
		out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.Contains(line, "IOPlatformUUID") {
					continue
				}
				if _, v, ok := strings.Cut(line, "="); ok {
					return strings.Trim(strings.TrimSpace(v), `"`)
				}
			}
		}
	case "windows":
		if v := os.Getenv("COMPUTERNAME"); v != "" {
			return v
		}
	}
	return ""
}

// sanitizeID keeps ids usable as filenames in the state repo.
func sanitizeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			// Drop separators rather than substituting, to keep ids compact.
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	if out == "" {
		return "node-unknown"
	}
	return out
}
