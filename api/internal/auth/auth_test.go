package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestRegisterIssuesAUsableSecret(t *testing.T) {
	r := NewRegistry("plain:test")
	dev, err := r.Register("Pixel 8", "android")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if dev.ID == "" || dev.Secret == "" {
		t.Fatal("empty device ID or secret")
	}

	// 32 bytes, or the HMAC key is weaker than the hash it feeds.
	raw, err := base64.RawURLEncoding.DecodeString(dev.Secret)
	if err != nil {
		t.Fatalf("secret is not base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("secret is %d bytes, want 32", len(raw))
	}

	got, err := r.Device(dev.ID)
	if err != nil {
		t.Fatalf("device lookup: %v", err)
	}
	if got.Name != "Pixel 8" {
		t.Errorf("name = %q, want Pixel 8", got.Name)
	}
	if r.Count() != 1 {
		t.Errorf("count = %d, want 1", r.Count())
	}
}

func TestRegisterSecretsAreDistinct(t *testing.T) {
	r := NewRegistry("plain:test")
	a, _ := r.Register("a", "android")
	b, _ := r.Register("b", "android")
	if a.Secret == b.Secret {
		t.Fatal("two devices got the same secret")
	}
}

// The secret must not escape via any listing path — that is the whole reason
// Redacted exists.
func TestListRedactsSecrets(t *testing.T) {
	r := NewRegistry("plain:test")
	dev, _ := r.Register("Pixel 8", "android")

	for _, d := range r.List() {
		if d.Secret != "" {
			t.Fatalf("List leaked the device secret for %s", d.ID)
		}
	}
	if dev.Secret == "" {
		t.Fatal("Redacted mutated the stored device")
	}
}

func TestDeviceUnknown(t *testing.T) {
	r := NewRegistry("plain:test")
	if _, err := r.Device("dev_nope"); !errors.Is(err, ErrBadDevice) {
		t.Fatalf("err = %v, want ErrBadDevice", err)
	}
}

// Revoking must kill live sessions too, or a revoked phone keeps approving
// commands until its key expires.
func TestRevokeDropsSessions(t *testing.T) {
	r := NewRegistry("plain:test")
	dev, _ := r.Register("dev", "android")
	sess, _, err := r.NewSession(dev.ID)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := r.Session(sess.KeyID); err != nil {
		t.Fatalf("session not usable before revoke: %v", err)
	}

	if err := r.Revoke(dev.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if r.Count() != 0 {
		t.Errorf("count = %d after revoke, want 0", r.Count())
	}
	if _, err := r.Session(sess.KeyID); !errors.Is(err, ErrBadSession) {
		t.Fatalf("session survived revocation: err = %v", err)
	}
	if _, err := r.DeviceKey(dev.ID); !errors.Is(err, ErrBadDevice) {
		t.Fatalf("device key survived revocation: err = %v", err)
	}
}

func TestVerifyPassword(t *testing.T) {
	r := NewRegistry("plain:secret")
	if err := r.VerifyPassword("secret"); err != nil {
		t.Fatalf("correct password: %v", err)
	}
	if err := r.VerifyPassword("wrong"); err != ErrBadPassword {
		t.Fatalf("wrong password: err = %v, want ErrBadPassword", err)
	}
}

func TestVerifyPasswordUnpaired(t *testing.T) {
	r := NewRegistry("")
	if err := r.VerifyPassword("anything"); err != ErrBadPassword {
		t.Fatalf("unpaired: err = %v, want ErrBadPassword", err)
	}
}

func TestHashAndVerifyArgon2id(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash prefix = %q, want $argon2id$", hash[:10])
	}

	r := NewRegistry(hash)
	if err := r.VerifyPassword("correct horse battery"); err != nil {
		t.Fatalf("correct: %v", err)
	}
	if err := r.VerifyPassword("wrong"); err != ErrBadPassword {
		t.Fatalf("wrong: err = %v, want ErrBadPassword", err)
	}
}

func TestHashPasswordUniqueSalt(t *testing.T) {
	h1, _ := HashPassword("same")
	h2, _ := HashPassword("same")
	if h1 == h2 {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}

	r := NewRegistry(h1)
	if err := r.VerifyPassword("same"); err != nil {
		t.Fatalf("verify h1: %v", err)
	}
	r2 := NewRegistry(h2)
	if err := r2.VerifyPassword("same"); err != nil {
		t.Fatalf("verify h2: %v", err)
	}
}
