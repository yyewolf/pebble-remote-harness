// Package hub turns Kilo's event firehose into watch-sized envelopes and
// holds them in a replayable, acked queue.
//
// This is the component that justifies prh's existence: Kilo emits ~200 event
// types including per-token deltas, the watch needs five, and Bluetooth drops
// often enough that events must survive a disconnect.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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

// upstream is one plugin connection. Its lifetime is the connection's
// lifetime: when the window closes, the socket drops, and the upstream is
// removed. The plugin path (M1) registers upstreams here; the fallback SSE
// path uses clients below instead.
type upstream struct {
	id        string
	project   string
	directory string
	parentPID int

	// Per-upstream decision queue. The plugin long-polls this; only the
	// owning upstream's plugin can apply its own decisions, which is the
	// isolation that stops one window from approving another's prompt.
	decisions  []protocol.Decision
	decCursor  uint64
	decWakers  map[chan struct{}]bool
}

// Hub fans upstream events into one ordered stream.
type Hub struct {
	mu   sync.RWMutex
	seq  uint64
	ring []protocol.Envelope    // bounded, oldest first
	size int                    // ring capacity
	open      map[string]pending // envelope ID -> awaiting reply
	byRequest map[string]string // request_id -> envelope ID (for gone retraction)
	subs      map[chan struct{}]bool // long-poll wakeups

	clients map[string]*kilo.Client // upstream name -> client (fallback)

	upstreams map[string]*upstream // plugin upstreams
	upSeq     uint64
	decSeq    uint64
}

// New returns a Hub retaining ringSize envelopes.
func New(ringSize int) *Hub {
	if ringSize <= 0 {
		ringSize = 200
	}
	return &Hub{
		size:      ringSize,
		ring:      make([]protocol.Envelope, 0, ringSize),
		open:      make(map[string]pending),
		byRequest: make(map[string]string),
		subs:      make(map[chan struct{}]bool),
		clients:   make(map[string]*kilo.Client),
		upstreams: make(map[string]*upstream),
	}
}

// AddUpstream registers a kilo instance to consume from (fallback path).
func (h *Hub) AddUpstream(name string, c *kilo.Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[name] = c
}

// Run consumes every upstream until ctx is done.
//
// The fallback SSE path is not yet implemented. The plugin path (M1) does not
// use Run — events arrive via IngestPluginEvents. With no configured fallback
// upstreams this blocks until ctx is done, which is the correct idle state.
func (h *Hub) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Translate converts a raw SSE event into an envelope (fallback path).
//
// TODO: implement the mapping in docs/protocol.md for the fallback SSE path.
// The plugin path uses TranslatePluginEvent instead, which receives events
// already extracted by the plugin.
func (h *Hub) Translate(upstream string, ev kilo.Event) (protocol.Envelope, bool) {
	return protocol.Envelope{}, false
}

// RegisterUpstream creates a plugin upstream and returns its ID.
//
// Identity is (parentPID, directory); re-registering the same key replaces
// the entry, which is what a VSCode reload produces.
func (h *Hub) RegisterUpstream(project, directory string, parentPID int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.upSeq++
	id := fmt.Sprintf("up_%d", h.upSeq)
	h.upstreams[id] = &upstream{
		id:        id,
		project:   project,
		directory: directory,
		parentPID: parentPID,
		decWakers: make(map[chan struct{}]bool),
	}
	return id
}

// RemoveUpstream drops a plugin upstream. Called when the connection drops.
func (h *Hub) RemoveUpstream(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.upstreams, id)
}

// UpstreamProject returns the project label for an upstream, or "" if unknown.
func (h *Hub) UpstreamProject(id string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if up, ok := h.upstreams[id]; ok {
		return up.project
	}
	return ""
}

// UpstreamExists reports whether an upstream ID is registered.
func (h *Hub) UpstreamExists(id string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.upstreams[id]
	return ok
}

// TranslatePluginEvent converts a plugin event to a watch-sized envelope.
//
// Field names follow Kilo's v1 permission.asked, which is what actually fires
// — not the v2 schema. See docs/kilo-integration.md.
//
// The bool is needsReply: perm and ques envelopes track a pending request so
// a reply can route back. idle and err are notify-only.
func (h *Hub) TranslatePluginEvent(project string, ev protocol.PluginEvent) (protocol.Envelope, bool) {
	switch ev.Kind {
	case "permission":
		body := ""
		if len(ev.Resources) > 0 {
			body = ev.Resources[0]
		}
		choices := []string{"Approve", "Reject"}
		if len(ev.Always) > 0 {
			// "Always: ls *" — the pattern, not the command. Approving
			// "ls -la" with always grants "ls *"; show it or the user
			// consents to more than they read.
			choices = []string{"Approve", "Always: " + truncate(ev.Always[0], protocol.MaxChoice), "Reject"}
		}
		return protocol.Envelope{
			Type:    protocol.EventPerm,
			Project: truncate(project, protocol.MaxProject),
			Session: ev.SessionID,
			Title:   truncate(ev.Action, protocol.MaxTitle),
			Body:    truncate(body, protocol.MaxBody),
			Choices: truncateChoices(choices),
		}, true

	case "idle":
		return protocol.Envelope{
			Type:    protocol.EventIdle,
			Project: truncate(project, protocol.MaxProject),
			Session: ev.SessionID,
			Title:   "idle",
		}, false

	case "error":
		return protocol.Envelope{
			Type:    protocol.EventErr,
			Project: truncate(project, protocol.MaxProject),
			Session: ev.SessionID,
			Title:   "error",
			Body:    truncate(ev.Description, protocol.MaxBody),
		}, false

	default:
		return protocol.Envelope{}, false
	}
}

// IngestPluginEvents translates and publishes a batch of plugin events.
//
// Called from the plugin uplink handler. Must never block the agent: the
// handler enforces a deadline, and the hub work here is all in-memory.
func (h *Hub) IngestPluginEvents(upstreamID, project string, events []protocol.PluginEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, ev := range events {
		if ev.Kind == "replied" {
			h.handleRepliedLocked(ev)
			continue
		}

		env, needsReply := h.TranslatePluginEvent(project, ev)
		if env.Type == "" {
			continue // unrecognized kind, drop
		}

		env = h.publishLocked(env)
		if needsReply {
			h.open[env.ID] = pending{
				env:       env,
				upstream:  upstreamID,
				requestID: ev.RequestID,
				sessionID: ev.SessionID,
			}
			h.byRequest[ev.RequestID] = env.ID
		}
	}
}

// handleRepliedLocked retracts a prompt answered elsewhere — including in the
// VSCode UI. Publishes a gone envelope so the watch stops asking. Must be
// called with mu held.
func (h *Hub) handleRepliedLocked(ev protocol.PluginEvent) {
	envID, ok := h.byRequest[ev.RequestID]
	if !ok {
		return // already answered or never tracked
	}
	p, ok := h.open[envID]
	if !ok {
		delete(h.byRequest, ev.RequestID)
		return
	}

	delete(h.open, envID)
	delete(h.byRequest, ev.RequestID)

	h.publishLocked(protocol.Envelope{
		ID:      envID, // same ID so the watch knows which to dismiss
		Type:    protocol.EventGone,
		Project: p.env.Project,
		Session: p.env.Session,
	})
}

// publishLocked assigns seq and ID, appends to the ring, evicts the oldest if
// over capacity, and wakes all pollers. Must be called with mu held. Returns
// the envelope with seq and ID assigned.
func (h *Hub) publishLocked(env protocol.Envelope) protocol.Envelope {
	h.seq++
	env.Seq = h.seq
	if env.ID == "" {
		env.ID = fmt.Sprintf("evt_%d", h.seq)
	}

	h.ring = append(h.ring, env)
	if len(h.ring) > h.size {
		evicted := h.ring[0]
		h.ring = h.ring[1:]
		// Clean up pending tracking for the evicted envelope. If it was
		// already answered (removed by handleRepliedLocked), this is a
		// no-op.
		if p, ok := h.open[evicted.ID]; ok {
			delete(h.open, evicted.ID)
			delete(h.byRequest, p.requestID)
		}
	}

	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // already woken, don't block
		}
	}
	return env
}

// Publish assigns a sequence number, appends to the ring, and wakes pollers.
func (h *Hub) Publish(env protocol.Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.publishLocked(env)
}

// Poll blocks up to wait for envelopes newer than cursor, returning
// immediately if any are already queued.
func (h *Hub) Poll(ctx context.Context, cursor uint64, wait time.Duration) (protocol.PollResponse, error) {
	if resp, err := h.pollOnce(cursor); err != nil || len(resp.Events) > 0 {
		return resp, err
	}

	if wait <= 0 {
		return h.pollOnce(cursor)
	}

	wake := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[wake] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, wake)
		h.mu.Unlock()
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return protocol.PollResponse{}, ctx.Err()
		case <-timer.C:
			return h.pollOnce(cursor)
		case <-wake:
			if resp, err := h.pollOnce(cursor); err != nil || len(resp.Events) > 0 {
				return resp, err
			}
		}
	}
}

// pollOnce returns events newer than cursor, or ErrCursorTooOld. Does not block.
func (h *Hub) pollOnce(cursor uint64) (protocol.PollResponse, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.ring) > 0 && cursor+1 < h.ring[0].Seq {
		return protocol.PollResponse{}, ErrCursorTooOld
	}

	// A cursor *ahead* of our sequence belongs to a previous daemon epoch. The
	// ring is in-memory, so a prh restart resets the sequence to zero while the
	// companion keeps the cursor it persisted — and every event published
	// before the sequence climbs back past that number would be silently
	// dropped, with the poll returning 200 and an empty list. Observed on
	// hardware: a phone paired before a restart sat at cursor 10, missed the
	// next event entirely, and only recovered once the sequence overtook it.
	//
	// Treat it the same as a cursor that fell off the ring: the client's
	// reaction to 410 — reset and accept the gap — is exactly right here too.
	if cursor > h.seq {
		return protocol.PollResponse{}, ErrCursorTooOld
	}

	events := make([]protocol.Envelope, 0)
	for i := range h.ring {
		if h.ring[i].Seq > cursor {
			events = append(events, h.ring[i])
		}
	}

	return protocol.PollResponse{Cursor: h.seq, Events: events}, nil
}

// ErrAlreadyAnswered means the envelope was already replied to or expired.
// The companion retries on network failure; this lets httpapi return 409
// so a retry does not double-approve.
var ErrAlreadyAnswered = errors.New("hub: envelope already answered or expired")

// Reply routes an answer back to the upstream that asked, by queueing a
// decision for the plugin to apply. The plugin is the thing that can
// actually call Kilo's API — prh never holds Kilo credentials.
func (h *Hub) Reply(req protocol.ReplyRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	p, ok := h.open[req.EventID]
	if !ok {
		return ErrAlreadyAnswered
	}

	// Delete now so a concurrent or retried reply gets 409, not a second
	// decision. The plugin's own nonce check is the second line of defence.
	delete(h.open, req.EventID)
	delete(h.byRequest, p.requestID)

	h.decSeq++
	dec := protocol.Decision{
		ID:        fmt.Sprintf("dec_%d", h.decSeq),
		RequestID: p.requestID,
		SessionID: p.sessionID,
		Kind:      string(p.env.Type),
		Action:    req.Action,
		Choice:    req.Choice,
		Text:      req.Text,
		Nonce:     randNonce(),
		Expires:   p.env.Expires,
	}

	up, ok := h.upstreams[p.upstream]
	if !ok {
		return ErrAlreadyAnswered // upstream gone
	}

	up.decisions = append(up.decisions, dec)
	up.decCursor++
	for ch := range up.decWakers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}

	return nil
}

// PollDecisions is the downlink long-poll the plugin holds open. Only the
// owning upstream's decisions are returned — one window's plugin must never
// be able to apply another window's approval.
func (h *Hub) PollDecisions(ctx context.Context, upstreamID string, cursor uint64, wait time.Duration) (protocol.DecisionsResponse, error) {
	if resp, err := h.pollDecisionsOnce(upstreamID, cursor); err != nil || len(resp.Decisions) > 0 {
		return resp, err
	}

	if wait <= 0 {
		return h.pollDecisionsOnce(upstreamID, cursor)
	}

	h.mu.Lock()
	up, ok := h.upstreams[upstreamID]
	h.mu.Unlock()
	if !ok {
		return protocol.DecisionsResponse{}, ErrAlreadyAnswered
	}

	wake := make(chan struct{}, 1)
	h.mu.Lock()
	up.decWakers[wake] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(up.decWakers, wake)
		h.mu.Unlock()
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return protocol.DecisionsResponse{}, ctx.Err()
		case <-timer.C:
			return h.pollDecisionsOnce(upstreamID, cursor)
		case <-wake:
			if resp, err := h.pollDecisionsOnce(upstreamID, cursor); err != nil || len(resp.Decisions) > 0 {
				return resp, err
			}
		}
	}
}

func (h *Hub) pollDecisionsOnce(upstreamID string, cursor uint64) (protocol.DecisionsResponse, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	up, ok := h.upstreams[upstreamID]
	if !ok {
		return protocol.DecisionsResponse{}, ErrAlreadyAnswered
	}

	resp := protocol.DecisionsResponse{Cursor: up.decCursor}
	for _, dec := range up.decisions {
		if decCursorOf(dec) > cursor {
			resp.Decisions = append(resp.Decisions, dec)
		}
	}
	return resp, nil
}

// decCursorOf extracts the monotonic position of a decision. We store the
// cursor alongside each decision by tracking it in the queue index.
func decCursorOf(dec protocol.Decision) uint64 {
	// Decisions are numbered by h.decSeq; the ID is dec_N. Parse it.
	var n uint64
	if len(dec.ID) > 4 && dec.ID[:4] == "dec_" {
		fmt.Sscanf(dec.ID[4:], "%d", &n)
	}
	return n
}

// AckDecision records that the plugin applied (or refused) a decision, so prh
// can stop retrying. For now this is a no-op store — the plugin's long-poll
// cursor advances past it, so it will not be re-sent.
func (h *Hub) AckDecision(upstreamID, decID, status string) {
	// Nothing to do: the cursor advances and the decision is not re-sent.
	// In future this could track delivery status for the watch's "sent"
	// indicator.
}

// Prompt forwards dictated text to a session.
//
// TODO: implement in a later milestone. Queue a kind: "prompt" decision for
// the plugin.
func (h *Hub) Prompt(ctx context.Context, req protocol.PromptRequest) error {
	return ErrNotImplemented
}

// randNonce returns 8 random hex bytes, for decision deduplication.
func randNonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Cursor returns the newest sequence number, for clients recovering from a
// gap.
func (h *Hub) Cursor() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.seq
}

// Upstreams reports how many kilo instances are connected.
func (h *Hub) Upstreams() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients) + len(h.upstreams)
}

// truncate clips a string to max bytes. prh truncates so the companion never
// has to, keeping truncation consistent across every consumer.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// truncateChoices clips the choice list to MaxChoices entries and each entry
// to MaxChoice bytes.
func truncateChoices(choices []string) []string {
	if len(choices) > protocol.MaxChoices {
		choices = choices[:protocol.MaxChoices]
	}
	out := make([]string, len(choices))
	for i, c := range choices {
		out[i] = truncate(c, protocol.MaxChoice)
	}
	return out
}
