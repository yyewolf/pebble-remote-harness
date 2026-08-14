// Package config holds prh's runtime configuration.
//
// The VSCode extension writes this file; prh also runs standalone, so every
// field has a usable default and an environment override.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
)

// Config is the on-disk configuration, typically ~/.config/prh/config.json.
type Config struct {
	// Listen is the bind address. Defaults to all interfaces because the
	// phone has to reach it; narrow this on a multi-homed host.
	Listen string `json:"listen"`

	// PasswordHash is argon2id over the pairing password. The plaintext lives
	// only in VSCode SecretStorage and is never written here.
	PasswordHash string `json:"password_hash"`

	// ServerName is shown in the companion's pairing screen.
	ServerName string `json:"server_name"`

	// SocketPath is the plugin channel: an AF_UNIX socket in a 0700
	// directory. It carries no credentials and is never on the network. It is
	// also the singleton token — a window that can connect to it adopts the
	// running daemon instead of starting a second one.
	SocketPath string `json:"socket_path"`

	// Upstreams is the fallback for a standalone prh with no plugin
	// installed. The plugin path needs none of this: plugins register
	// themselves over SocketPath, and Kilo's ports and passwords rotate on
	// every VSCode reload anyway. See docs/plugin.md.
	Upstreams []Upstream `json:"upstreams,omitempty"`

	// RingSize bounds the per-device replay buffer.
	RingSize int `json:"ring_size"`

	// MaxPollWaitSec caps the long-poll hold. Keep it under typical NAT idle
	// timeouts.
	MaxPollWaitSec int `json:"max_poll_wait_sec"`
}

// Upstream is one kilo serve instance.
type Upstream struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"` // e.g. http://127.0.0.1:4096
	// Password is the KILO_SERVER_PASSWORD for HTTP Basic. Any username works.
	Password string `json:"password"`
}

// Default returns a Config with everything but credentials filled in.
func Default() Config {
	return Config{
		Listen:         "0.0.0.0:8477",
		ServerName:     hostname(),
		SocketPath:     DefaultSocketPath(),
		RingSize:       200,
		MaxPollWaitSec: 55,
	}
}

// DefaultSocketPath prefers XDG_RUNTIME_DIR, which is already per-user and
// mode 0700, and is cleaned up on logout.
//
// The plugin computes this same path independently — it cannot be passed one,
// since it is loaded by Kilo and not by us. Any change here must be mirrored
// in plugin/.
func DefaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir + "/prh/plugin.sock"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "prh.sock"
	}
	return home + "/.local/state/prh/plugin.sock"
}

// ErrNoPassword means the daemon has never been paired.
var ErrNoPassword = errors.New("config: no password set; pair from the VSCode extension")

// Load reads path, applies defaults for missing fields, then applies
// environment overrides. A missing file is not an error — it yields defaults.
func Load(path string) (Config, error) {
	cfg := Default()

	buf, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(buf, &cfg); err != nil {
			return cfg, err
		}
	case errors.Is(err, os.ErrNotExist):
		// defaults
	default:
		return cfg, err
	}

	applyEnv(&cfg)
	return cfg, nil
}

// Save writes cfg to path with 0600 permissions.
//
// TODO: write to a temp file and rename, so a crash mid-write cannot leave a
// truncated config that loses the pairing.
func Save(path string, cfg Config) error {
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o600)
}

// Validate reports whether the daemon can serve traffic.
func (c Config) Validate() error {
	if c.PasswordHash == "" {
		return ErrNoPassword
	}
	return nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("PRH_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("PRH_SERVER_NAME"); v != "" {
		cfg.ServerName = v
	}
	if v := os.Getenv("PRH_RING_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RingSize = n
		}
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "prh"
	}
	return h
}
