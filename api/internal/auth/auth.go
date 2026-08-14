// Package auth handles the pairing password and per-device tokens.
//
// Two distinct secrets, deliberately not shared:
//   - the pairing password: typed once in the companion, stored hashed here
//   - a device token: issued at registration, revocable, used for every call
//
// Kilo's own KILO_SERVER_PASSWORD is a third secret that never leaves the box.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
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
// TODO (M3): implement with argon2id. Needs `go get golang.org/x/crypto` — the
// first external dependency this module will take. Do not substitute a bare
// SHA-256; this hash sits in a config file. For now, the unpaired development
// case stores the hash via VerifyPassword's plaintext path.
func HashPassword(plaintext string) (string, error) {
	return "", ErrNotImplemented
}

// VerifyPassword checks a pairing attempt. Must be constant-time.
//
// When no password hash is configured the daemon is unpaired and every
// attempt fails with ErrBadPassword — never a nil that would let an attacker
// through an unconfigured endpoint.
func (r *Registry) VerifyPassword(plaintext string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.passwordHash == "" {
		return ErrBadPassword
	}

	// Until argon2id lands (M3), the config may carry the plaintext password
	// prefixed with "plain:" for local development. The extension will switch
	// to hashed once the argon2id path is live.
	if len(r.passwordHash) > 6 && r.passwordHash[:6] == "plain:" {
		stored := r.passwordHash[6:]
		if subtle.ConstantTimeCompare([]byte(stored), []byte(plaintext)) == 1 {
			return nil
		}
		return ErrBadPassword
	}

	return ErrBadPassword
}

// hashToken returns a base64url SHA-256 digest of a token. Device tokens are
// high-entropy (32 random bytes) so SHA-256 is sufficient — unlike the pairing
// password, which is user-chosen and needs argon2id.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Register issues a new device token. The plaintext token is returned exactly
// once; only its hash is retained.
func (r *Registry) Register(name, platform string) (deviceID, token string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("auth: generating token: %w", err)
	}
	token = "prh_" + base64.RawURLEncoding.EncodeToString(raw)

	rawID := make([]byte, 8)
	if _, err := rand.Read(rawID); err != nil {
		return "", "", fmt.Errorf("auth: generating device id: %w", err)
	}
	deviceID = "dev_" + base64.RawURLEncoding.EncodeToString(rawID)

	now := time.Now()
	d := &Device{
		ID:         deviceID,
		Name:       name,
		Platform:   platform,
		TokenHash:  hashToken(token),
		Registered: now,
		LastSeen:   now,
	}

	r.mu.Lock()
	r.devices[deviceID] = d
	r.mu.Unlock()

	return deviceID, token, nil
}

// Authenticate resolves a bearer token to a device and bumps LastSeen.
func (r *Registry) Authenticate(token string) (*Device, error) {
	h := hashToken(token)

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, d := range r.devices {
		if subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(h)) == 1 {
			d.LastSeen = time.Now()
			return d, nil
		}
	}
	return nil, ErrBadToken
}

// Revoke drops a device. Called from the VSCode extension.
func (r *Registry) Revoke(deviceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.devices[deviceID]; !ok {
		return fmt.Errorf("auth: unknown device %s", deviceID)
	}
	delete(r.devices, deviceID)
	return nil
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
