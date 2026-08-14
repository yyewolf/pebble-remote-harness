// Command prh is the Pebble Remote Harness daemon.
//
// It consumes events from one or more headless `kilo serve` instances,
// collapses them into watch-sized envelopes, and serves them to the Android
// companion over a long-poll API.
//
// Normally started and stopped by the VSCode extension, but it runs
// standalone so it survives a VSCode restart:
//
//	prh serve --config ~/.config/prh/config.json
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yyewolf/pebble-remote-harness/api/internal/auth"
	"github.com/yyewolf/pebble-remote-harness/api/internal/config"
	"github.com/yyewolf/pebble-remote-harness/api/internal/httpapi"
	"github.com/yyewolf/pebble-remote-harness/api/internal/hub"
	"github.com/yyewolf/pebble-remote-harness/api/internal/kilo"
	"github.com/yyewolf/pebble-remote-harness/api/internal/protocol"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)"
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "prh:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("prh", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config.json")
	listen := fs.String("listen", "", "override the bind address")
	showVersion := fs.Bool("version", false, "print version and exit")

	// "prh serve" and "prh" both run the daemon; the subcommand is
	// optional and exists so docs and Makefiles read naturally. Unknown
	// subcommands are not rejected — flag.Parse stops at the first
	// non-flag, so we strip a leading "serve" and parse the rest.
	rest := args
	if len(rest) > 0 && rest[0] == "serve" {
		rest = rest[1:]
	}

	if err := fs.Parse(rest); err != nil {
		return err
	}

	if *showVersion {
		fmt.Printf("prh %s (protocol %s)\n", version, protocol.Version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	// An unpaired daemon still serves /v1/health so the extension can show
	// "running, not paired" rather than "dead".
	if err := cfg.Validate(); err != nil {
		log.Warn("daemon is unpaired", "reason", err)
	}

	h := hub.New(cfg.RingSize)
	for _, up := range cfg.Upstreams {
		h.AddUpstream(up.Name, kilo.New(up.BaseURL, up.Password))
		log.Info("upstream configured", "name", up.Name, "url", up.BaseURL)
	}

	devices := auth.NewRegistry(cfg.PasswordHash)
	api := httpapi.New(cfg, h, devices, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// TODO: hub.Run is a stub. Once implemented this must restart on error
	// rather than logging once — upstreams die whenever VSCode reloads.
	go func() {
		if err := h.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("hub stopped", "err", err)
		}
	}()

	// The plugin channel. Never on the network: wiring these routes into the
	// TCP listener would expose upstream registration to the LAN.
	pluginLn, err := listenPluginSocket(cfg.SocketPath)
	if err != nil {
		return err
	}
	defer os.Remove(cfg.SocketPath)

	pluginSrv := &http.Server{Handler: api.PluginHandler()}
	go func() {
		if err := pluginSrv.Serve(pluginLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("plugin socket stopped", "err", err)
		}
	}()
	log.Info("plugin socket listening", "path", cfg.SocketPath)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: long-poll holds responses open for up to
		// MaxPollWaitSec. Bound the hold in the handler instead.
		IdleTimeout: 90 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = pluginSrv.Shutdown(shutdownCtx)
	}()

	log.Info("prh listening", "addr", cfg.Listen, "version", version, "protocol", protocol.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	log.Info("prh stopped")
	return nil
}

// listenPluginSocket binds the plugin channel, enforcing the singleton.
//
// There is one prh per user per machine — the phone pairs with one endpoint
// and sees every window. The socket is the election token: if something
// answers on it, a daemon is already running and this process must not start
// a second one. If the file exists but refuses connections, it is the debris
// of a crash and is safe to remove.
func listenPluginSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	// 0700: the directory permission is what authenticates us to the plugin,
	// since only this user can place a socket at this path.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}

	if _, err := os.Stat(path); err == nil {
		conn, derr := net.DialTimeout("unix", path, time.Second)
		if derr == nil {
			conn.Close()
			return nil, fmt.Errorf("another prh is already running on %s", path)
		}
		// Refused or timed out: stale socket from a crash.
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing stale socket: %w", err)
		}
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("securing %s: %w", path, err)
	}

	// TODO: verify peer UID on accept (SO_PEERCRED on Linux, LOCAL_PEERCRED
	// on macOS). The 0700 directory already excludes other users; this turns
	// that assumption into a checked fact.
	return ln, nil
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "config.json"
	}
	return dir + "/prh/config.json"
}
