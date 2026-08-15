// Package auth handles the pairing passphrase, per-device secrets, and the
// session keys that sign requests.
//
// Three secrets, deliberately not shared, each crossing the wire as rarely as
// it can:
//   - the pairing passphrase: typed once in the companion, argon2id-hashed
//     here, and transmitted exactly once per device
//   - a device secret: issued at registration, persisted on both sides, and
//     never transmitted again — it only signs
//   - a session key: wrapped under the device secret at login, held in memory
//     only, and likewise only ever signs
//
// Kilo's own KILO_SERVER_PASSWORD is a fourth secret that never leaves the box.
//
// Note the asymmetry with the previous bearer-token design: verifying an HMAC
// requires the key, so device secrets are stored in plaintext rather than
// hashed. That trade is deliberate and is argued in docs/protocol.md — Hop 1
// is unencrypted HTTP on a LAN, where a credential replayed on every request
// is a far larger exposure than one sitting in a 0600 file on a machine an
// attacker would already have to own.
package auth

import (
	"crypto/rand"
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
	ErrBadDevice      = errors.New("auth: unknown or revoked device")
	ErrBadSession     = errors.New("auth: unknown or expired session")
	ErrRateLimited    = errors.New("auth: too many attempts")
)

// Device is one registered companion.
type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`

	// Secret is the base64url device secret, 32 random bytes. Stored in
	// plaintext because HMAC verification needs the key itself; see the
	// package comment. This is why devices.json is 0600 and why Redacted()
	// exists for anything that leaves the process.
	Secret string `json:"secret"`

	Registered time.Time `json:"registered"`
	LastSeen   time.Time `json:"last_seen"`
}

// Redacted returns a copy safe to log or serve. Every path that exposes a
// Device outside this package must go through it.
func (d *Device) Redacted() Device {
	c := *d
	c.Secret = ""
	return c
}

// key decodes the device secret for use as an HMAC key.
func (d *Device) key() ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(d.Secret)
}

// Registry owns devices and sessions and verifies credentials.
type Registry struct {
	mu           sync.RWMutex
	passwordHash string
	devices      map[string]*Device  // keyed by device ID
	sessions     map[string]*Session // keyed by session key ID

	// path is where devices are persisted. Empty disables persistence, which
	// is what the tests use.
	path string

	// nonces backs replay rejection for signed requests.
	nonces *nonceCache

	// pairing is the currently-armed enrolment window, nil when shut. Not
	// persisted: a window must never survive a restart the user did not watch.
	pairing *pairingWindow
}

func NewRegistry(passwordHash string) *Registry {
	return &Registry{
		passwordHash: passwordHash,
		devices:      make(map[string]*Device),
		sessions:     make(map[string]*Session),
		nonces:       newNonceCache(),
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

// randomID returns a prefixed, base64url-encoded random identifier.
func randomID(prefix string, n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generating %s id: %w", prefix, err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Register issues a new device secret. The secret is returned to the caller
// exactly once — it is stored here so that HMACs can be verified, but it is
// never sent over the wire again, and no endpoint reads it back out.
func (r *Registry) Register(name, platform string) (*Device, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("auth: generating device secret: %w", err)
	}
	deviceID, err := randomID("dev_", 8)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	d := &Device{
		ID:         deviceID,
		Name:       name,
		Platform:   platform,
		Secret:     base64.RawURLEncoding.EncodeToString(raw),
		Registered: now,
		LastSeen:   now,
	}

	r.mu.Lock()
	r.devices[deviceID] = d
	r.mu.Unlock()

	// A device that is not persisted stops working at the next restart, which
	// is precisely the failure the persistence exists to prevent. Report the
	// error rather than handing back a secret that will not survive.
	if err := r.persist(); err != nil {
		return nil, err
	}
	return d, nil
}

// Revoke drops a device and every session it holds.
//
// Dropping the sessions is the part that matters: leaving them alive would
// mean a revoked phone keeps approving commands until its session key expires.
func (r *Registry) Revoke(deviceID string) error {
	r.mu.Lock()
	if _, ok := r.devices[deviceID]; !ok {
		r.mu.Unlock()
		return fmt.Errorf("auth: unknown device %s", deviceID)
	}
	delete(r.devices, deviceID)
	for id, s := range r.sessions {
		if s.DeviceID == deviceID {
			delete(r.sessions, id)
		}
	}
	r.mu.Unlock()

	return r.persist()
}

// List returns redacted devices, for the extension's status view. The secret
// is stripped: this crosses a process boundary.
func (r *Registry) List() []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Device, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, d.Redacted())
	}
	return out
}

// Count reports how many devices are registered.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.devices)
}
