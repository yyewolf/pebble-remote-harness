package auth

import (
	"strings"
	"testing"
)

func TestRegisterAndAuthenticate(t *testing.T) {
	r := NewRegistry("plain:test")
	deviceID, token, err := r.Register("Pixel 8", "android")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if deviceID == "" || token == "" {
		t.Fatal("empty device ID or token")
	}
	if token[:4] != "prh_" {
		t.Errorf("token prefix = %q, want prh_", token[:4])
	}

	dev, err := r.Authenticate(token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if dev.ID != deviceID {
		t.Errorf("device ID = %q, want %q", dev.ID, deviceID)
	}
	if dev.Name != "Pixel 8" {
		t.Errorf("name = %q, want Pixel 8", dev.Name)
	}
	if r.Count() != 1 {
		t.Errorf("count = %d, want 1", r.Count())
	}
}

func TestAuthenticateBadToken(t *testing.T) {
	r := NewRegistry("plain:test")
	_, _, _ = r.Register("dev", "android")

	_, err := r.Authenticate("prh_bogus")
	if err != ErrBadToken {
		t.Fatalf("err = %v, want ErrBadToken", err)
	}
}

func TestRevoke(t *testing.T) {
	r := NewRegistry("plain:test")
	deviceID, token, _ := r.Register("dev", "android")

	if err := r.Revoke(deviceID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if r.Count() != 0 {
		t.Errorf("count = %d after revoke, want 0", r.Count())
	}
	_, err := r.Authenticate(token)
	if err != ErrBadToken {
		t.Fatalf("authenticate after revoke: err = %v, want ErrBadToken", err)
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
