package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/yyewolf/pebble-remote-harness/api/internal/protocol"
)

// Session is one device's signing key, valid until it expires or prh restarts.
//
// Sessions are memory-only on purpose. Persisting them would keep a signing
// key alive across a restart with no way for the server to know it had been
// captured in the meantime, and the companion already recovers from a lost
// session in one round trip. The device secret is the thing worth persisting;
// this is not.
type Session struct {
	KeyID    string
	DeviceID string
	Key      []byte
	Expires  time.Time
}

// hkdfInfo domain-separates the wrapping key. If another key is ever derived
// from a device secret it must use a different info string, or the two become
// the same key.
const hkdfInfo = "prh-session-wrap-v1"

// WrappedSession is a session key sealed for delivery to one device.
type WrappedSession struct {
	KeyID      string
	WrapSalt   string
	WrapNonce  string
	WrappedKey string
	Expires    time.Time
}

// NewSession mints a session key for a device and seals it under a key derived
// from that device's secret.
//
// The session key is generated here rather than by the client so that a
// compromised or lazy client cannot pick a weak one, and it is sealed rather
// than sent plainly so that Hop 1 never carries a usable credential even once.
// The key ID is authenticated as AEAD additional data, so a swapped key ID
// fails to open rather than silently binding the key to the wrong session.
func (r *Registry) NewSession(deviceID string) (*Session, *WrappedSession, error) {
	r.mu.RLock()
	d, ok := r.devices[deviceID]
	r.mu.RUnlock()
	if !ok {
		return nil, nil, ErrBadDevice
	}

	secret, err := d.key()
	if err != nil {
		return nil, nil, fmt.Errorf("auth: decoding device secret: %w", err)
	}

	sessionKey := make([]byte, 32)
	if _, err := rand.Read(sessionKey); err != nil {
		return nil, nil, fmt.Errorf("auth: generating session key: %w", err)
	}
	keyID, err := randomID("key_", 8)
	if err != nil {
		return nil, nil, err
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("auth: generating wrap salt: %w", err)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("auth: generating wrap nonce: %w", err)
	}

	sealed, err := sealUnder(secret, salt, nonce, []byte(keyID), hkdfInfo, sessionKey)
	if err != nil {
		return nil, nil, err
	}

	s := &Session{
		KeyID:    keyID,
		DeviceID: deviceID,
		Key:      sessionKey,
		Expires:  time.Now().Add(protocol.SessionTTLSec * time.Second),
	}

	r.mu.Lock()
	// One live session per device. Logging in again is what a client does
	// after a restart or a suspected compromise, and both cases want the old
	// key gone rather than lingering until its TTL.
	for id, old := range r.sessions {
		if old.DeviceID == deviceID {
			delete(r.sessions, id)
		}
	}
	r.sessions[keyID] = s
	r.mu.Unlock()

	return s, &WrappedSession{
		KeyID:      keyID,
		WrapSalt:   base64.RawURLEncoding.EncodeToString(salt),
		WrapNonce:  base64.RawURLEncoding.EncodeToString(nonce),
		WrappedKey: base64.RawURLEncoding.EncodeToString(sealed),
		Expires:    s.Expires,
	}, nil
}

// sealUnder derives a wrapping key from secret and AES-256-GCM seals plaintext
// under it. Shared by the session wrap and the pairing wrap.
//
// AES-GCM rather than anything more exotic because the other end is Android,
// where it is in the platform library; the companion must be able to open this
// without pulling in a crypto dependency.
//
// info is what keeps the two uses apart. Two wraps deriving the same key from
// the same secret would let a blob sealed for one purpose be opened as the
// other, so every caller passes its own constant and none of them share.
func sealUnder(secret, salt, nonce, aad []byte, info string, plaintext []byte) ([]byte, error) {
	wrapKey := make([]byte, 32)
	kdf := hkdf.New(sha256.New, secret, salt, []byte(info))
	if _, err := io.ReadFull(kdf, wrapKey); err != nil {
		return nil, fmt.Errorf("auth: deriving wrap key: %w", err)
	}

	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, fmt.Errorf("auth: wrap cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: wrap gcm: %w", err)
	}
	return gcm.Seal(nil, nonce, plaintext, aad), nil
}

// Session resolves a key ID, rejecting expired sessions.
//
// Expiry is enforced on read rather than by a sweeper: a session nobody
// presents does no harm, and the check has to happen here regardless.
func (r *Registry) Session(keyID string) (*Session, error) {
	r.mu.RLock()
	s, ok := r.sessions[keyID]
	r.mu.RUnlock()
	if !ok {
		return nil, ErrBadSession
	}
	if time.Now().After(s.Expires) {
		r.mu.Lock()
		delete(r.sessions, keyID)
		r.mu.Unlock()
		return nil, ErrBadSession
	}
	return s, nil
}

// Device resolves a device ID and bumps LastSeen.
//
// LastSeen is not persisted on every call — that would mean a disk write per
// request. It is written out with the next registration or revocation, which
// is accurate enough for a status view.
func (r *Registry) Device(id string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.devices[id]
	if !ok {
		return nil, ErrBadDevice
	}
	d.LastSeen = time.Now()
	return d, nil
}

// Sessions reports how many session keys are live, for /v1/health.
func (r *Registry) Sessions() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}
