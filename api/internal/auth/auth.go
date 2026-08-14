// Package auth handles the pairing password and per-device tokens.
//
// Two distinct secrets, deliberately not shared:
//   - the pairing password: typed once in the companion, stored hashed here
//   - a device token: issued at registration, revocable, used for every call
//
// Kilo's own KILO_SERVER_PASSWORD is a third secret that never leaves the box.
package auth

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrNotImplemented = errors.New("auth: not implemented")
	ErrBadPassword    = errors.New("auth: bad password")
	ErrBadToken       = errors.New("auth: unknown or revoked token")
	ErrRateLimited    = errors.New("auth: too many attempts")
)

// Device is one registered companion.
type Device struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Platform   string    `json:"platform"`
	TokenHash  string    `json:"token_hash"` // never store the token itself
	Registered time.Time `json:"registered"`
	LastSeen   time.Time `json:"last_seen"`
}

// Registry owns devices and verifies credentials.
type Registry struct {
	mu           sync.RWMutex
	passwordHash string
	devices      map[string]*Device // keyed by device ID
}

func NewRegistry(passwordHash string) *Registry {
	return &Registry{
		passwordHash: passwordHash,
		devices:      make(map[string]*Device),
	}
}

// HashPassword derives a storable hash from a plaintext pairing password.
//
// TODO: implement with argon2id. Needs `go get golang.org/x/crypto` — the
// first external dependency this module will take. Do not substitute a bare
// SHA-256; this hash sits in a config file.
func HashPassword(plaintext string) (string, error) {
	return "", ErrNotImplemented
}

// VerifyPassword checks a pairing attempt. Must be constant-time.
//
// TODO: implement, and rate-limit by source IP at the caller — registration
// is the only endpoint where an attacker can guess.
func (r *Registry) VerifyPassword(plaintext string) error {
	return ErrNotImplemented
}

// Register issues a new device token. The plaintext token is returned exactly
// once; only its hash is retained.
//
// TODO: implement. Token should be >=32 bytes from crypto/rand, base64url.
func (r *Registry) Register(name, platform string) (deviceID, token string, err error) {
	return "", "", ErrNotImplemented
}

// Authenticate resolves a bearer token to a device and bumps LastSeen.
func (r *Registry) Authenticate(token string) (*Device, error) {
	return nil, ErrNotImplemented
}

// Revoke drops a device. Called from the VSCode extension.
func (r *Registry) Revoke(deviceID string) error {
	return ErrNotImplemented
}

// List returns registered devices, for the extension's status view.
func (r *Registry) List() []*Device {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Device, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, d)
	}
	return out
}

// Count reports how many devices are registered.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.devices)
}
