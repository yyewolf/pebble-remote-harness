package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"
)

// openSessionKey is the companion's half of the wrap, reimplemented here from
// the documented recipe rather than by calling the sealing code. If this
// drifts from LoginResponse's doc comment, the Android client breaks — so the
// test asserts the contract, not the implementation.
func openSessionKey(t *testing.T, secretB64, saltB64, nonceB64, wrappedB64, keyID string) []byte {
	t.Helper()

	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("base64: %v", err)
		}
		return b
	}

	wrapKey := make([]byte, 32)
	kdf := hkdf.New(sha256.New, dec(secretB64), dec(saltB64), []byte("prh-session-wrap-v1"))
	if _, err := io.ReadFull(kdf, wrapKey); err != nil {
		t.Fatalf("hkdf: %v", err)
	}

	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	out, err := gcm.Open(nil, dec(nonceB64), dec(wrappedB64), []byte(keyID))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return out
}

func TestSessionKeyUnwrapsWithTheDeviceSecret(t *testing.T) {
	r := NewRegistry("plain:test")
	dev, _ := r.Register("Pixel 8", "android")

	sess, wrapped, err := r.NewSession(dev.ID)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	got := openSessionKey(t, dev.Secret, wrapped.WrapSalt, wrapped.WrapNonce, wrapped.WrappedKey, wrapped.KeyID)
	if string(got) != string(sess.Key) {
		t.Fatal("unwrapped key does not match the server's session key")
	}
	if len(got) != 32 {
		t.Errorf("session key is %d bytes, want 32", len(got))
	}
}

// The wrapped key is worthless to anyone who did not register: that is what
// keeps the session key off the wire in usable form.
func TestSessionKeyDoesNotUnwrapWithTheWrongSecret(t *testing.T) {
	r := NewRegistry("plain:test")
	dev, _ := r.Register("mine", "android")
	other, _ := r.Register("theirs", "android")

	_, wrapped, err := r.NewSession(dev.ID)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	wrapKey := make([]byte, 32)
	secret, _ := base64.RawURLEncoding.DecodeString(other.Secret)
	salt, _ := base64.RawURLEncoding.DecodeString(wrapped.WrapSalt)
	kdf := hkdf.New(sha256.New, secret, salt, []byte("prh-session-wrap-v1"))
	_, _ = io.ReadFull(kdf, wrapKey)

	block, _ := aes.NewCipher(wrapKey)
	gcm, _ := cipher.NewGCM(block)
	nonce, _ := base64.RawURLEncoding.DecodeString(wrapped.WrapNonce)
	sealed, _ := base64.RawURLEncoding.DecodeString(wrapped.WrappedKey)

	if _, err := gcm.Open(nil, nonce, sealed, []byte(wrapped.KeyID)); err == nil {
		t.Fatal("another device's secret opened the wrapped key")
	}
}

// The key ID is AEAD additional data, so swapping it must fail loudly rather
// than binding a good key to the wrong session.
func TestSessionKeyIsBoundToItsKeyID(t *testing.T) {
	r := NewRegistry("plain:test")
	dev, _ := r.Register("dev", "android")
	_, wrapped, _ := r.NewSession(dev.ID)

	wrapKey := make([]byte, 32)
	secret, _ := base64.RawURLEncoding.DecodeString(dev.Secret)
	salt, _ := base64.RawURLEncoding.DecodeString(wrapped.WrapSalt)
	kdf := hkdf.New(sha256.New, secret, salt, []byte("prh-session-wrap-v1"))
	_, _ = io.ReadFull(kdf, wrapKey)

	block, _ := aes.NewCipher(wrapKey)
	gcm, _ := cipher.NewGCM(block)
	nonce, _ := base64.RawURLEncoding.DecodeString(wrapped.WrapNonce)
	sealed, _ := base64.RawURLEncoding.DecodeString(wrapped.WrappedKey)

	if _, err := gcm.Open(nil, nonce, sealed, []byte("key_someoneelse")); err == nil {
		t.Fatal("wrapped key opened under a different key ID")
	}
}

func TestNewSessionUnknownDevice(t *testing.T) {
	r := NewRegistry("plain:test")
	if _, _, err := r.NewSession("dev_nope"); !errors.Is(err, ErrBadDevice) {
		t.Fatalf("err = %v, want ErrBadDevice", err)
	}
}

// Logging in again replaces the old key rather than accumulating keys, so a
// client that reconnects repeatedly does not leave a trail of valid ones.
func TestLoginReplacesTheDevicesPreviousSession(t *testing.T) {
	r := NewRegistry("plain:test")
	dev, _ := r.Register("dev", "android")

	first, _, _ := r.NewSession(dev.ID)
	second, _, _ := r.NewSession(dev.ID)

	if _, err := r.Session(first.KeyID); !errors.Is(err, ErrBadSession) {
		t.Fatalf("old session still valid after re-login: %v", err)
	}
	if _, err := r.Session(second.KeyID); err != nil {
		t.Fatalf("new session not valid: %v", err)
	}
	if n := r.Sessions(); n != 1 {
		t.Errorf("sessions = %d, want 1", n)
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	r := NewRegistry("plain:test")
	dev, _ := r.Register("dev", "android")
	sess, _, _ := r.NewSession(dev.ID)

	r.mu.Lock()
	r.sessions[sess.KeyID].Expires = time.Now().Add(-time.Second)
	r.mu.Unlock()

	if _, err := r.Session(sess.KeyID); !errors.Is(err, ErrBadSession) {
		t.Fatalf("expired session accepted: %v", err)
	}
	if n := r.Sessions(); n != 0 {
		t.Errorf("expired session not dropped: %d remain", n)
	}
}
