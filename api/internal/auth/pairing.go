package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

var (
	ErrPairingClosed = errors.New("auth: pairing window is not open")
	ErrPairingKeyLen = errors.New("auth: pairing key too short")
)

// PairingKeyLen is the required pairing key length in bytes.
//
// The pairing key is what an attacker who captured the enrolment exchange
// would have to guess offline, and everything the device holds afterwards is
// derived from it. It is generated, never typed, so there is no reason for it
// to be anything but full strength.
const PairingKeyLen = 32

// pairingWindow is a bounded, single-use opportunity to enrol one device.
//
// Enrolment is the one exchange whose compromise hands over everything, so it
// is not left standing open. Before this, POST /v1/register answered at any
// hour to anyone holding the passphrase; a passphrase learned by any means —
// sniffed, shoulder-surfed, read off a screenshot or a clipboard — was a
// permanent invitation. Now it is only useful during a window the user opened
// deliberately, seconds ago, and which closes the moment one device enrols.
type pairingWindow struct {
	key     []byte
	expires time.Time
}

// OpenPairing arms a pairing window.
//
// Opening a second window replaces the first rather than adding to it: two
// simultaneously valid enrolment keys is never what anyone wants, and the
// user's most recent action is the one they are looking at.
func (r *Registry) OpenPairing(key []byte, ttl time.Duration) error {
	if len(key) < PairingKeyLen {
		return fmt.Errorf("%w: got %d bytes, need %d", ErrPairingKeyLen, len(key), PairingKeyLen)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.pairing = &pairingWindow{
		key:     append([]byte(nil), key...),
		expires: time.Now().Add(ttl),
	}
	return nil
}

// ClosePairing disarms the window immediately.
func (r *Registry) ClosePairing() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pairing = nil
}

// PairingKey returns the armed key, or ErrPairingClosed.
//
// Expiry is enforced here rather than by a timer, so a window that nobody uses
// simply stops working at its deadline with nothing to go wrong.
func (r *Registry) PairingKey() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.pairing == nil {
		return nil, ErrPairingClosed
	}
	if time.Now().After(r.pairing.expires) {
		r.pairing = nil
		return nil, ErrPairingClosed
	}
	return r.pairing.key, nil
}

// PairingOpen reports whether enrolment is currently possible, for /v1/health
// and the extension's status view. It deliberately exposes only the fact, not
// the key.
func (r *Registry) PairingOpen() bool {
	_, err := r.PairingKey()
	return err == nil
}

// PairingExpiry reports when the window closes, or the zero time if shut.
func (r *Registry) PairingExpiry() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.pairing == nil {
		return time.Time{}
	}
	return r.pairing.expires
}

// NewPairingKey generates a key suitable for OpenPairing. Used by the `prh
// pair` command; the extension generates its own.
func NewPairingKey() ([]byte, error) {
	key := make([]byte, PairingKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("auth: generating pairing key: %w", err)
	}
	return key, nil
}

// pairingWrapInfo domain-separates the pairing wrap from the session wrap.
// The two must never derive the same key from the same input.
const pairingWrapInfo = "prh-pairing-wrap-v1"

// WrappedSecret is a device secret sealed for delivery to one enrolling phone.
type WrappedSecret struct {
	WrapSalt   string
	WrapNonce  string
	WrapSecret string
}

// RegisterSealed enrols a device and seals its secret under the pairing key,
// then closes the window.
//
// This is the whole point of the pairing key: the device secret never appears
// on the wire in a usable form, so an attacker who captured the entire
// enrolment exchange holds a signature and a sealed blob and can do nothing
// with either. Contrast Register, which returns the secret in the clear and is
// only safe if the transport is.
//
// The window closes on success rather than on expiry — one window enrols one
// device. A second phone means a second deliberate act by the user.
func (r *Registry) RegisterSealed(name, platform string, pairingKey []byte) (*Device, *WrappedSecret, error) {
	dev, err := r.Register(name, platform)
	if err != nil {
		return nil, nil, err
	}

	secret, err := dev.key()
	if err != nil {
		return nil, nil, fmt.Errorf("auth: decoding new device secret: %w", err)
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("auth: generating pairing salt: %w", err)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("auth: generating pairing nonce: %w", err)
	}

	// The device ID is the additional data, so a response whose ID was swapped
	// in flight fails to open rather than binding a good secret to an identity
	// prh never issued.
	sealed, err := sealUnder(pairingKey, salt, nonce, []byte(dev.ID), pairingWrapInfo, secret)
	if err != nil {
		return nil, nil, err
	}

	r.ClosePairing()

	return dev, &WrappedSecret{
		WrapSalt:   base64.RawURLEncoding.EncodeToString(salt),
		WrapNonce:  base64.RawURLEncoding.EncodeToString(nonce),
		WrapSecret: base64.RawURLEncoding.EncodeToString(sealed),
	}, nil
}
