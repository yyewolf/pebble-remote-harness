package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// deviceFile is the on-disk shape. Versioned so that a future format change
// can be detected rather than mis-parsed into an empty registry, which would
// silently deauthorise every paired phone.
type deviceFile struct {
	Version int       `json:"version"`
	Devices []*Device `json:"devices"`
}

const deviceFileVersion = 1

// LoadRegistry reads persisted devices from path.
//
// This is what makes a prh restart survivable. Without it every restart
// deauthorises every device, the companion cannot re-establish a session
// signed with a secret the daemon has forgotten, and the user is sent back to
// retyping the pairing passphrase — which is exactly the friction that leads
// people to pick a short one.
//
// A missing file is not an error: it is a daemon that has never been paired.
// A corrupt one is, and loudly — continuing with an empty registry would look
// identical to a fresh install and quietly discard working pairings.
func LoadRegistry(path, passwordHash string) (*Registry, error) {
	r := NewRegistry(passwordHash)
	r.path = path

	buf, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return r, nil
	case err != nil:
		return nil, fmt.Errorf("auth: reading %s: %w", path, err)
	}

	var f deviceFile
	if err := json.Unmarshal(buf, &f); err != nil {
		return nil, fmt.Errorf("auth: parsing %s: %w", path, err)
	}
	if f.Version != deviceFileVersion {
		return nil, fmt.Errorf("auth: %s has version %d, expected %d", path, f.Version, deviceFileVersion)
	}

	for _, d := range f.Devices {
		if d.ID == "" || d.Secret == "" {
			continue // unusable entry; a device with no key can never sign
		}
		r.devices[d.ID] = d
	}
	return r, nil
}

// persist writes the device list atomically.
//
// Atomic because this file is the only copy of every pairing: a crash midway
// through a plain overwrite would leave a truncated file that LoadRegistry
// rejects, and the user re-pairs every device. Write to a temp file in the
// same directory, fsync, then rename.
func (r *Registry) persist() error {
	if r.path == "" {
		return nil // persistence disabled (tests)
	}

	r.mu.RLock()
	f := deviceFile{Version: deviceFileVersion, Devices: make([]*Device, 0, len(r.devices))}
	for _, d := range r.devices {
		f.Devices = append(f.Devices, d)
	}
	buf, err := json.MarshalIndent(f, "", "  ")
	r.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("auth: encoding devices: %w", err)
	}

	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: creating %s: %w", dir, err)
	}

	// Same directory, or the rename stops being atomic across filesystems.
	tmp, err := os.CreateTemp(dir, ".devices-*.json")
	if err != nil {
		return fmt.Errorf("auth: creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	// 0600 before any content is written: this file holds device secrets in
	// plaintext, and CreateTemp's default is already 0600, but say so.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: writing devices: %w", err)
	}
	// Without the sync the rename can land before the data does, which on a
	// power loss yields an empty file where the pairings were.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: syncing devices: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: closing temp file: %w", err)
	}

	if err := os.Rename(tmpName, r.path); err != nil {
		return fmt.Errorf("auth: replacing %s: %w", r.path, err)
	}
	return nil
}

// DefaultDevicesPath puts devices.json beside the config it belongs to.
func DefaultDevicesPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "devices.json")
}
