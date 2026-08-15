package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// The point of persistence: a prh restart must not deauthorise a paired
// phone. If this breaks, the companion cannot log in again and the user is
// sent back to retyping the passphrase.
func TestDevicesSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")

	first, err := LoadRegistry(path, "plain:test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dev, err := first.Register("Pixel 8", "android")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// A whole new process would see exactly this.
	second, err := LoadRegistry(path, "plain:test")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	got, err := second.Device(dev.ID)
	if err != nil {
		t.Fatalf("device lost across restart: %v", err)
	}
	if got.Secret != dev.Secret {
		t.Fatal("device secret changed across restart; the phone could not sign")
	}
	if got.Name != "Pixel 8" {
		t.Errorf("name = %q, want Pixel 8", got.Name)
	}
}

func TestRevocationSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")

	first, _ := LoadRegistry(path, "plain:test")
	dev, _ := first.Register("dev", "android")
	if err := first.Revoke(dev.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	second, _ := LoadRegistry(path, "plain:test")
	if second.Count() != 0 {
		t.Fatal("a revoked device came back after restart")
	}
}

// A daemon that has never been paired has no file, and that is not an error.
func TestLoadMissingFile(t *testing.T) {
	r, err := LoadRegistry(filepath.Join(t.TempDir(), "devices.json"), "plain:test")
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if r.Count() != 0 {
		t.Errorf("count = %d, want 0", r.Count())
	}
}

// Corruption must be loud. Silently starting empty is indistinguishable from a
// fresh install and would discard working pairings without telling anyone.
func TestLoadCorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(path, "plain:test"); err == nil {
		t.Fatal("corrupt device file loaded without error")
	}
}

func TestLoadRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"devices":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(path, "plain:test"); err == nil {
		t.Fatal("unknown version loaded without error")
	}
}

// The file holds device secrets in plaintext, so its mode is load-bearing.
func TestDeviceFileIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	r, _ := LoadRegistry(path, "plain:test")
	if _, err := r.Register("dev", "android"); err != nil {
		t.Fatalf("register: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("devices.json mode = %o, want 600", mode)
	}
}

// A registry with no path is what the tests use; it must not try to write.
func TestPersistenceDisabledWithoutAPath(t *testing.T) {
	r := NewRegistry("plain:test")
	if _, err := r.Register("dev", "android"); err != nil {
		t.Fatalf("register without persistence: %v", err)
	}
}
