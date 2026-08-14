// Package waker is the seam for app-closed alerts.
//
// The chosen design does not need it: the Android companion holds the
// long-poll itself and calls startAppOnPebble to wake the watch. This package
// exists so the fallbacks in docs/notifications.md can be added without
// touching the hub, if the PebbleKit compatibility probe fails.
package waker

import (
	"context"
	"errors"

	"github.com/yyewolf/pebble-remote-harness/api/internal/protocol"
)

var ErrNotImplemented = errors.New("waker: not implemented")

// Waker delivers an envelope through an out-of-band channel.
type Waker interface {
	// Wake is best-effort and must not block the hub. Errors are logged, not
	// propagated: a failed alert must never stall event delivery.
	Wake(ctx context.Context, env protocol.Envelope) error
	Name() string
}

// Noop is the default: the companion does the waking.
type Noop struct{}

func (Noop) Wake(context.Context, protocol.Envelope) error { return nil }
func (Noop) Name() string                                  { return "noop" }

// Timeline pushes a pin with an openWatchApp action.
//
// Outbound-only — the dev box calls the timeline API and nothing needs to
// reach the dev box. Needs a per-user-per-app token from
// Pebble.getTimelineToken().
//
// TODO: unverified whether Rebble still issues timeline tokens for
// unpublished sideloaded apps. Confirm before implementing.
type Timeline struct {
	APIBase   string
	UserToken string
}

func (t *Timeline) Wake(ctx context.Context, env protocol.Envelope) error {
	return ErrNotImplemented
}

func (t *Timeline) Name() string { return "timeline" }

// Ntfy publishes to an ntfy topic; the Android client posts a notification
// that the Pebble app mirrors to the watch. Action buttons can call
// /v1/reply directly, so approve/deny works without the watchapp at all.
type Ntfy struct {
	Server string
	Topic  string
	Token  string
}

func (n *Ntfy) Wake(ctx context.Context, env protocol.Envelope) error {
	return ErrNotImplemented
}

func (n *Ntfy) Name() string { return "ntfy" }
