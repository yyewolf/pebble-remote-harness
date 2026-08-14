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
	"net/http"
	"os"
	"os/signal"
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
	if err := fs.Parse(args); err != nil {
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
	}()

	log.Info("prh listening", "addr", cfg.Listen, "version", version, "protocol", protocol.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	log.Info("prh stopped")
	return nil
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "config.json"
	}
	return dir + "/prh/config.json"
}
