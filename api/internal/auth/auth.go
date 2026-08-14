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
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
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

// argon2id parameters. Memory is in KiB. These are moderate parameters
// suitable for a single-user daemon: fast enough not to lag a pairing
// attempt, expensive enough to deter brute force on a config-file hash.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword derives a storable argon2id hash from a plaintext pairing
// password. The hash encodes its own parameters and salt so verification
// does not need to match the creation config:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<key>
//
// Do not substitute a bare SHA-256; this hash sits in a config file.
func HashPassword(plaintext string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generating salt: %w", err)
	}
	key := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a pairing attempt. Must be constant-time.
//
// When no password hash is configured the daemon is unpaired and every
// attempt fails with ErrBadPassword — never a nil that would let an attacker
// through an unconfigured endpoint.
//
// Supports two stored formats:
//   - "$argon2id$..." — the production format from HashPassword
//   - "plain:..." — local development only; the extension will switch to
//     hashed once the argon2id path is live
func (r *Registry) VerifyPassword(plaintext string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.passwordHash == "" {
		return ErrBadPassword
	}

	stored := r.passwordHash

	if strings.HasPrefix(stored, "$argon2id$") {
		return verifyArgon2id(stored, plaintext)
	}

	// Local development fallback.
	if strings.HasPrefix(stored, "plain:") {
		pw := stored[6:]
		if subtle.ConstantTimeCompare([]byte(pw), []byte(plaintext)) == 1 {
			return nil
		}
		return ErrBadPassword
	}

	// Unknown format: reject. Never guess.
	return ErrBadPassword
}

// verifyArgon2id parses the encoded hash, re-derives a key with the same
// parameters and salt, and compares in constant time.
//
// Format: $argon2id$v=19$m=65536,t=3,p=4$<salt>$<key>
// After splitting on "$": [0]="" [1]="argon2id" [2]="v=19" [3]="m=...,t=...,p=..."
// [4]=salt [5]=key
func verifyArgon2id(encoded, plaintext string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return ErrBadPassword
	}

	var m, t, p int
	for _, kv := range strings.Split(parts[3], ",") {
		kvParts := strings.SplitN(kv, "=", 2)
		if len(kvParts) != 2 {
			return ErrBadPassword
		}
		val, err := strconv.Atoi(kvParts[1])
		if err != nil {
			return ErrBadPassword
		}
		switch kvParts[0] {
		case "m":
			m = val
		case "t":
			t = val
		case "p":
			p = val
		}
	}
	if m == 0 || t == 0 || p == 0 {
		return ErrBadPassword
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return ErrBadPassword
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return ErrBadPassword
	}

	derived := argon2.IDKey([]byte(plaintext), salt, uint32(t), uint32(m), uint8(p), uint32(len(expected)))
	if subtle.ConstantTimeCompare(derived, expected) == 1 {
		return nil
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
