// Package hub turns Kilo's event firehose into watch-sized envelopes and
// holds them in a replayable, acked queue.
//
// This is the component that justifies prh's existence: Kilo emits ~200 event
// types including per-token deltas, the watch needs five, and Bluetooth drops
// often enough that events must survive a disconnect.
package hub

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yyewolf/pebble-remote-harness/api/internal/kilo"
	"github.com/yyewolf/pebble-remote-harness/api/internal/protocol"
)

var (
	ErrNotImplemented = errors.New("hub: not implemented")
	// ErrCursorTooOld means the caller's cursor fell out of the ring buffer.
	// The companion should reset to the newest cursor and accept the gap.
	ErrCursorTooOld = errors.New("hub: cursor older than retained history")
)

// pending is an envelope awaiting a reply, with what's needed to route the
// answer back to the right Kilo endpoint.
type pending struct {
	env       protocol.Envelope
	upstream  string // which kilo instance it came from
	requestID string // Kilo's "per..." or "que..." id
	sessionID string
}

// Hub fans upstream events into one ordered stream.
type Hub struct {
	mu   sync.RWMutex
	seq  uint64
	ring []protocol.Envelope    // bounded, oldest first
	size int                    // ring capacity
	open map[string]pending     // envelope ID -> awaiting reply
	subs map[chan struct{}]bool // long-poll wakeups

	clients map[string]*kilo.Client // upstream name -> client
}

// New returns a Hub retaining ringSize envelopes.
func New(ringSize int) *Hub {
	if ringSize <= 0 {
		ringSize = 200
	}
	return &Hub{
		size:    ringSize,
		ring:    make([]protocol.Envelope, 0, ringSize),
		open:    make(map[string]pending),
		subs:    make(map[chan struct{}]bool),
		clients: make(map[string]*kilo.Client),
	}
}

// AddUpstream registers a kilo instance to consume from.
func (h *Hub) AddUpstream(name string, c *kilo.Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[name] = c
}

// Run consumes every upstream until ctx is done.
//
// TODO: implement. For each client, call Events(ctx), decode with Translate,
// and Publish what survives. Reconnect with backoff — kilo restarts whenever
// VSCode reloads the window.
func (h *Hub) Run(ctx context.Context) error {
	return ErrNotImplemented
}

// Translate converts an upstream event into an envelope. The bool is false
// for the ~195 event types the watch does not care about.
//
// TODO: implement the mapping in docs/protocol.md:
//
//	permission.v2.asked -> perm, Title=action, Body=resources[0], choices
//	                       Approve/Always/Reject (Always only when Save is set)
//	question.v2.asked   -> ques, choices from QuestionV2Info
//	session.idle        -> idle, notify only
//	session.error       -> err,  notify only
//
// Truncate to protocol.MaxTitle / MaxBody here, so truncation is consistent
// for every consumer.
func (h *Hub) Translate(upstream string, ev kilo.Event) (protocol.Envelope, bool) {
	return protocol.Envelope{}, false
}

// Publish assigns a sequence number, appends to the ring, and wakes pollers.
//
// TODO: implement. Evict from the front once len(ring) == size, record
// reply-needing envelopes in h.open, and close every channel in h.subs.
func (h *Hub) Publish(env protocol.Envelope) {
}

// Poll blocks up to wait for envelopes newer than cursor, returning
// immediately if any are already queued.
//
// TODO: implement. Register a wakeup channel in h.subs, select on it against
// ctx.Done() and a timer. Return ErrCursorTooOld when cursor has been evicted.
func (h *Hub) Poll(ctx context.Context, cursor uint64, wait time.Duration) (protocol.PollResponse, error) {
	return protocol.PollResponse{}, ErrNotImplemented
}

// Reply routes an answer back to the Kilo instance that asked.
//
// TODO: implement. Look up h.open[req.EventID], pick ReplyPermission or
// ReplyQuestion by envelope type, delete on success. Return a distinguishable
// error for already-answered so httpapi can return 409.
func (h *Hub) Reply(ctx context.Context, req protocol.ReplyRequest) error {
	return ErrNotImplemented
}

// Prompt forwards dictated text to a session.
func (h *Hub) Prompt(ctx context.Context, req protocol.PromptRequest) error {
	return ErrNotImplemented
}

// Cursor returns the newest sequence number, for clients recovering from a
// gap.
func (h *Hub) Cursor() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.seq
}

// Upstreams reports how many kilo instances are configured.
func (h *Hub) Upstreams() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
