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

func testPairingKey(b byte) []byte {
	k := make([]byte, PairingKeyLen)
	for i := range k {
		k[i] = b
	}
	return k
}

// The companion's half of the pairing wrap, written from the documented
// recipe rather than by calling the sealing code.
func openSealedSecret(t *testing.T, pairingKey []byte, w *WrappedSecret, deviceID string) []byte {
	t.Helper()

	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("base64: %v", err)
		}
		return b
	}

	wrapKey := make([]byte, 32)
	kdf := hkdf.New(sha256.New, pairingKey, dec(w.WrapSalt), []byte("prh-pairing-wrap-v1"))
	if _, err := io.ReadFull(kdf, wrapKey); err != nil {
		t.Fatalf("hkdf: %v", err)
	}
	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	out, err := gcm.Open(nil, dec(w.WrapNonce), dec(w.WrapSecret), []byte(deviceID))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return out
}

func TestPairingWindowOpensAndCloses(t *testing.T) {
	r := NewRegistry("plain:test")

	if r.PairingOpen() {
		t.Fatal("pairing open before anyone asked")
	}
	if _, err := r.PairingKey(); !errors.Is(err, ErrPairingClosed) {
		t.Fatalf("err = %v, want ErrPairingClosed", err)
	}

	if err := r.OpenPairing(testPairingKey(1), time.Minute); err != nil {
		t.Fatalf("open: %v", err)
	}
	if !r.PairingOpen() {
		t.Fatal("window did not open")
	}

	r.ClosePairing()
	if r.PairingOpen() {
		t.Fatal("window did not close")
	}
}

// The window is the whole defence; if it outlived its deadline it would be no
// better than the permanently-open endpoint it replaced.
func TestPairingWindowExpires(t *testing.T) {
	r := NewRegistry("plain:test")
	if err := r.OpenPairing(testPairingKey(2), -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PairingKey(); !errors.Is(err, ErrPairingClosed) {
		t.Fatalf("expired window still open: %v", err)
	}
}

func TestPairingRejectsAWeakKey(t *testing.T) {
	r := NewRegistry("plain:test")
	if err := r.OpenPairing(make([]byte, PairingKeyLen-1), time.Minute); !errors.Is(err, ErrPairingKeyLen) {
		t.Fatalf("short key accepted: %v", err)
	}
}

func TestOpeningTwiceReplacesTheFirstWindow(t *testing.T) {
	r := NewRegistry("plain:test")
	_ = r.OpenPairing(testPairingKey(3), time.Minute)
	_ = r.OpenPairing(testPairingKey(4), time.Minute)

	got, err := r.PairingKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(testPairingKey(4)) {
		t.Fatal("the older pairing key is still valid")
	}
}

func TestRegisterSealedRoundTrips(t *testing.T) {
	r := NewRegistry("plain:test")
	key := testPairingKey(5)
	_ = r.OpenPairing(key, time.Minute)

	dev, wrapped, err := r.RegisterSealed("Pixel 8", "android", key)
	if err != nil {
		t.Fatalf("register sealed: %v", err)
	}

	got := openSealedSecret(t, key, wrapped, dev.ID)
	want, _ := base64.RawURLEncoding.DecodeString(dev.Secret)
	if string(got) != string(want) {
		t.Fatal("unsealed secret does not match the stored one")
	}
	if len(got) != 32 {
		t.Errorf("secret is %d bytes, want 32", len(got))
	}
}

// The point of sealing: capturing the enrolment response gains nothing.
func TestSealedSecretDoesNotOpenWithoutThePairingKey(t *testing.T) {
	r := NewRegistry("plain:test")
	key := testPairingKey(6)
	_ = r.OpenPairing(key, time.Minute)
	dev, wrapped, err := r.RegisterSealed("Pixel", "android", key)
	if err != nil {
		t.Fatal(err)
	}

	wrapKey := make([]byte, 32)
	salt, _ := base64.RawURLEncoding.DecodeString(wrapped.WrapSalt)
	kdf := hkdf.New(sha256.New, testPairingKey(99), salt, []byte("prh-pairing-wrap-v1"))
	_, _ = io.ReadFull(kdf, wrapKey)
	block, _ := aes.NewCipher(wrapKey)
	gcm, _ := cipher.NewGCM(block)
	nonce, _ := base64.RawURLEncoding.DecodeString(wrapped.WrapNonce)
	sealed, _ := base64.RawURLEncoding.DecodeString(wrapped.WrapSecret)

	if _, err := gcm.Open(nil, nonce, sealed, []byte(dev.ID)); err == nil {
		t.Fatal("a wrong pairing key opened the sealed device secret")
	}
}

// One window enrols one device. A second phone is a second deliberate act.
func TestRegisterSealedClosesTheWindow(t *testing.T) {
	r := NewRegistry("plain:test")
	key := testPairingKey(7)
	_ = r.OpenPairing(key, time.Minute)

	if _, _, err := r.RegisterSealed("first", "android", key); err != nil {
		t.Fatal(err)
	}
	if r.PairingOpen() {
		t.Fatal("window stayed open after a device enrolled")
	}
}

// The session wrap and the pairing wrap must not derive the same key, or a
// blob sealed for one purpose could be opened as the other.
func TestPairingAndSessionWrapsAreDomainSeparated(t *testing.T) {
	secret := testPairingKey(8)
	salt := make([]byte, 16)
	nonce := make([]byte, 12)

	a, err := sealUnder(secret, salt, nonce, []byte("aad"), hkdfInfo, []byte("plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := sealUnder(secret, salt, nonce, []byte("aad"), pairingWrapInfo, []byte("plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Fatal("both wraps derived the same key; the info string is not separating them")
	}
}
