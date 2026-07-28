// Package auth handles identity for nimbus.
//
// Login must work on a bare device with no CLI tooling installed, so every
// provider here speaks plain HTTPS from the standard library. The Provider
// interface exists so that non-git backends can be added later without
// rearchitecting (DESIGN.md §2a).
package auth

import (
	"context"
	"errors"
	"time"
)

// Common login failures callers are expected to distinguish.
var (
	ErrDenied      = errors.New("authorization denied by user")
	ErrExpired     = errors.New("authorization code expired before approval")
	ErrNotLoggedIn = errors.New("not logged in")
)

// Identity is a successful authentication against a provider.
type Identity struct {
	Provider  string    `json:"provider"`
	Login     string    `json:"login"`
	Token     string    `json:"token"`
	Scopes    []string  `json:"scopes,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Verification is what the user must do in a browser to approve the device.
type Verification struct {
	UserCode        string
	VerificationURI string
	ExpiresIn       time.Duration
	Interval        time.Duration
}

// PromptFunc displays a Verification to the user. It is called once, before
// polling begins, and must not block.
type PromptFunc func(Verification)

// Provider authenticates a device and identifies the account behind it.
type Provider interface {
	// Name is the stable key stored in credentials.json.
	Name() string
	// Login runs the full device-authorization flow to completion.
	Login(ctx context.Context, prompt PromptFunc) (*Identity, error)
	// Validate reports whether a stored Identity is still usable.
	Validate(ctx context.Context, id *Identity) error
}
