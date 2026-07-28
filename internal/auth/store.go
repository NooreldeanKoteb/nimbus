package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store persists identities to a single owner-readable JSON file.
//
// This is deliberately the weakest link in Phase 0: tokens sit on disk at 0600
// rather than in the OS keychain. Phase 1 moves them behind the keyring
// abstraction (DESIGN.md §11); the interface here does not change when it does.
type Store struct {
	Path string
}

type credentials struct {
	Identities map[string]*Identity `json:"identities"`
}

// Save writes id under its provider name, preserving other providers' entries.
func (s *Store) Save(id *Identity) error {
	if id == nil || id.Provider == "" {
		return errors.New("save: identity missing provider")
	}

	creds, err := s.load()
	if err != nil {
		return err
	}
	creds.Identities[id.Provider] = id

	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("save: %w", err)
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("save: %w", err)
	}

	// Write-then-rename so a crash mid-write cannot truncate existing tokens.
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("save: %w", err)
	}
	return nil
}

// Get returns the stored identity for a provider, or ErrNotLoggedIn.
func (s *Store) Get(provider string) (*Identity, error) {
	creds, err := s.load()
	if err != nil {
		return nil, err
	}
	id, ok := creds.Identities[provider]
	if !ok || id == nil || id.Token == "" {
		return nil, ErrNotLoggedIn
	}
	return id, nil
}

// Delete removes a provider's identity. Absent entries are not an error.
func (s *Store) Delete(provider string) error {
	creds, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := creds.Identities[provider]; !ok {
		return nil
	}
	delete(creds.Identities, provider)

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if err := os.WriteFile(s.Path, data, 0o600); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// List returns every stored provider name.
func (s *Store) List() ([]string, error) {
	creds, err := s.load()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(creds.Identities))
	for name := range creds.Identities {
		names = append(names, name)
	}
	return names, nil
}

func (s *Store) load() (*credentials, error) {
	creds := &credentials{Identities: map[string]*Identity{}}

	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return creds, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	if len(data) == 0 {
		return creds, nil
	}
	if err := json.Unmarshal(data, creds); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	if creds.Identities == nil {
		creds.Identities = map[string]*Identity{}
	}
	return creds, nil
}
