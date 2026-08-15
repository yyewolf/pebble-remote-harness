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
	"path"
	"sort"
	"strings"
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

// sess is the per-session state prh keeps for the phone's session list and
// conversation view. Built from events, not by calling Kilo, so prh needs no
// credentials.
type sess struct {
	id       string
	upstream string // which window owns it
	project  string
	title    string
	dir      string
	status   string // idle | busy | retry | unknown
	updated  int64  // unix ms

	// convSeq is a cursor space local to this session's conversation, separate
	// from the global ring's seq. The conversation endpoint filters on it, so
	// one session's history is served without scanning the global ring.
	//
	// A part that is updated is re-stamped with a fresh convSeq while keeping
	// its slice position, so a long-poller sees the change and a fresh reader
	// still gets the parts in the order they were created.
	convSeq  uint64
	conv     []protocol.Envelope // bounded, creation order, oldest first
	convSize int

	// pendingPrompt is the envelope ID of an unanswered perm/ques for this
	// session, so the session list can badge it. Only one is tracked: a
	// session has at most one outstanding prompt at a time.
	pendingPrompt     string
	pendingPromptType protocol.EventType

	convWakers map[chan struct{}]bool // conversation long-pollers
}

// touch advances the session's last-activity clock. Always monotonic: parts
// carry their own creation time, and an update to an *old* part must not drag
// the session backwards in a list sorted by recency.
func (s *sess) touch(ms int64) {
	if ms > s.updated {
		s.updated = ms
	}
}

// wake nudges every conversation long-poller. Must be called with mu held.
func (s *sess) wake() {
	for ch := range s.convWakers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// appendConv records an envelope in the session's conversation buffer.
//
// Entries are keyed by ID — the part ID for a message, the envelope ID for a
// prompt — and an entry that already exists is replaced in place, keeping its
// original position. The agent streams by re-sending the same part with more
// text, dozens to hundreds of times for one reply, so an append-only buffer
// would fill with copies of the part currently being written and evict the
// history behind it: open the view mid-reply and the conversation would be
// gone. Replacing keeps the buffer's capacity a count of *messages*, which is
// what it is documented to be.
//
// Must be called with mu held.
func (s *sess) appendConv(env protocol.Envelope) {
	s.convSeq++
	env.Seq = s.convSeq

	if env.ID != "" {
		for i := range s.conv {
			if s.conv[i].ID == env.ID {
				s.conv[i] = env
				return
			}
		}
	}

	s.conv = append(s.conv, env)
	if len(s.conv) > s.convSize {
		s.conv = s.conv[len(s.conv)-s.convSize:]
	}
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
	//
	// decCursor is the *global* decSeq of the newest queued decision, not a
	// count of this upstream's decisions. Decisions are filtered by the number
	// embedded in their global ID, so a per-upstream count would not compare
	// against them: with two windows open the counts fall behind the global
	// sequence, the plugin echoes back a cursor lower than its own decisions'
	// IDs, and every decision is redelivered on every poll — forever.
	decisions []protocol.Decision
	decCursor uint64
	decWakers map[chan struct{}]bool
}

// maxQueuedDecisions bounds one upstream's queue. Decisions are only removed
// by this trim: the plugin's cursor advancing past them is what retires them
// logically, but nothing was dropping them from memory.
const maxQueuedDecisions = 256

// Hub fans upstream events into one ordered stream.
type Hub struct {
	mu        sync.RWMutex
	seq       uint64
	ring      []protocol.Envelope    // bounded, oldest first
	size      int                    // ring capacity
	open      map[string]pending     // envelope ID -> awaiting reply
	byRequest map[string]string      // request_id -> envelope ID (for gone retraction)
	subs      map[chan struct{}]bool // long-poll wakeups

	clients map[string]*kilo.Client // upstream name -> client (fallback)

	upstreams map[string]*upstream // plugin upstreams
	upSeq     uint64
	decSeq    uint64

	// sessions is the per-session state for the phone's session list and
	// conversation view. Keyed by Kilo session ID. A session belongs to one
	// upstream; when that upstream drops, its sessions go too.
	sessions    map[string]*sess
	convSize    int // max messages kept per session
	maxSessions int // registry cap, oldest-by-activity evicted first
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
		sessions:  make(map[string]*sess),
		convSize:  50, // keep the last 50 messages per session
		// A window left open for a week accumulates sessions, each holding up
		// to convSize envelopes. Nothing else evicts them — Kilo only emits
		// session.deleted when the user actually deletes a session — so the
		// registry is capped and the least recently active is dropped.
		maxSessions: 64,
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
// Its sessions go too: a session belongs to its window, and the window is
// gone.
func (h *Hub) RemoveUpstream(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sid, s := range h.sessions {
		if s.upstream == id {
			delete(h.sessions, sid)
		}
	}
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
// a reply can route back. idle, err, message and session.* are notify-only.
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

	case "message":
		// Conversation content. Does not cross Bluetooth; the companion
		// renders it into its conversation view. The watch never sees it.
		return protocol.Envelope{
			Type:    protocol.EventMsg,
			Project: truncate(project, protocol.MaxProject),
			Session: ev.SessionID,
			MsgRole: ev.MsgRole,
			// A tool part can carry a whole file. prh truncates so that every
			// consumer sees the same string — the same contract the watch
			// fields already have.
			MsgPartID: ev.MsgPartID,
			MsgText:   truncate(ev.MsgText, protocol.MaxMsgText),
			MsgKind:   ev.MsgKind,
			MsgTime:   ev.MsgTimeMs,
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
		switch {
		case ev.Kind == "replied":
			h.handleRepliedLocked(ev)
		case ev.Kind == "message":
			h.handleMessageLocked(upstreamID, project, ev)
		case ev.Kind == "session.created":
			h.handleSessionCreatedLocked(upstreamID, project, ev)
		case ev.Kind == "session.updated":
			h.handleSessionUpdatedLocked(upstreamID, project, ev)
		case ev.Kind == "session.deleted":
			h.handleSessionDeletedLocked(ev)
		case ev.Kind == "session.status":
			h.handleSessionStatusLocked(upstreamID, project, ev)
		case ev.Kind == "idle":
			// session.idle doubles as a status update, so the session list
			// reflects idle even if Kilo never emits session.status.
			h.handleSessionStatusLocked(upstreamID, project, ev)
			env, _ := h.TranslatePluginEvent(project, ev)
			if env.Type != "" {
				h.publishLocked(env)
			}
		default:
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
				h.trackSessionPromptLocked(upstreamID, project, ev.SessionID, env)
			}
		}
	}
}

// handleMessageLocked records a conversation part in the session's buffer.
// Must be called with mu held.
//
// Deliberately *not* published to the global ring. The ring is the watch's
// poll stream: it is small, and the companion downloads all of it over mobile
// data before discarding anything the watch does not need. A streaming reply
// emits hundreds of part updates, which would (a) burn the phone's data on
// content the poll path throws away and (b) — the real damage — evict pending
// permission envelopes from a 200-entry ring before the watch ever polls them.
// Conversation lives on its own endpoint precisely so it cannot crowd out the
// prompts this project exists to deliver.
func (h *Hub) handleMessageLocked(upstreamID, project string, ev protocol.PluginEvent) {
	env, _ := h.TranslatePluginEvent(project, ev)
	if env.Type == "" || ev.SessionID == "" {
		return
	}

	s := h.getOrCreateSessionLocked(upstreamID, project, ev.SessionID)
	// The part ID *is* the conversation entry's identity: that is what makes a
	// streaming update replace its earlier text instead of stacking a partial
	// on top of it. A part with no ID cannot be deduplicated, so give it a
	// unique one rather than let it collide with every other anonymous part.
	env.ID = ev.MsgPartID
	if env.ID == "" {
		s.convSeq++
		env.ID = fmt.Sprintf("part_%d", s.convSeq)
	}
	s.appendConv(env)
	s.touch(ev.MsgTimeMs)
	s.wake()
}

// handleSessionCreatedLocked registers a new session from a session.created
// event. Must be called with mu held.
func (h *Hub) handleSessionCreatedLocked(upstreamID, project string, ev protocol.PluginEvent) {
	s := h.getOrCreateSessionLocked(upstreamID, project, ev.SessionID)
	s.title = truncate(ev.SessionTitle, protocol.MaxTitle)
	s.dir = ev.SessionDir
	if ev.SessionUpdatedMs > s.updated {
		s.updated = ev.SessionUpdatedMs
	}
}

// handleSessionUpdatedLocked updates an existing session's metadata. Must be
// called with mu held.
func (h *Hub) handleSessionUpdatedLocked(upstreamID, project string, ev protocol.PluginEvent) {
	s := h.getOrCreateSessionLocked(upstreamID, project, ev.SessionID)
	if ev.SessionTitle != "" {
		s.title = truncate(ev.SessionTitle, protocol.MaxTitle)
	}
	if ev.SessionDir != "" {
		s.dir = ev.SessionDir
	}
	if ev.SessionUpdatedMs > s.updated {
		s.updated = ev.SessionUpdatedMs
	}
}

// handleSessionDeletedLocked drops a session. Must be called with mu held.
//
// Wakes the conversation long-pollers rather than closing their channels: they
// are owned by the readers, and the next conversationOnce returns
// ErrUnknownSession, which turns into the 404 the phone already handles by
// closing the view.
func (h *Hub) handleSessionDeletedLocked(ev protocol.PluginEvent) {
	if s, ok := h.sessions[ev.SessionID]; ok {
		s.wake()
	}
	delete(h.sessions, ev.SessionID)
}

// handleSessionStatusLocked updates a session's status (idle/busy/retry).
// Must be called with mu held. Used for both session.status and session.idle.
//
// Receives the upstreamID and project so a session first seen through a
// status event (before session.created) still knows which window owns it —
// without that, Prompt() would find an empty upstream and fail with
// "session upstream gone".
func (h *Hub) handleSessionStatusLocked(upstreamID, project string, ev protocol.PluginEvent) {
	s, ok := h.sessions[ev.SessionID]
	if !ok {
		// A status event for an unknown session creates a minimal entry, so
		// the session list is not blind to a session that started emitting
		// before its session.created landed.
		s = h.getOrCreateSessionLocked(upstreamID, project, ev.SessionID)
	} else {
		// Keep upstream/project current even on a status update, in case
		// session.created never arrived or the upstream ID changed after a
		// window reload.
		if upstreamID != "" {
			s.upstream = upstreamID
		}
		if project != "" {
			s.project = project
		}
	}
	switch {
	case ev.Kind == "session.status" && ev.SessionStatus != "":
		s.status = ev.SessionStatus
	case ev.Kind == "idle":
		s.status = protocol.StatusIdle
	}
	s.touch(ev.SessionUpdatedMs)
	// A status change is what the list is showing; wake anything watching so a
	// busy session flipping to idle is not held until the poll times out.
	s.wake()
}

// trackSessionPromptLocked records that a session has a pending prompt and
// puts the prompt into the session's conversation, so the phone can badge the
// row *and* render approve/reject inline under the context that led to it.
// Must be called with mu held.
//
// The session is created if unknown. A permission can easily be the first
// thing prh hears about a session — the plugin loads mid-session, or prh
// restarted while the agent was working — and dropping the prompt then would
// hide exactly the row the user opened the app to find.
func (h *Hub) trackSessionPromptLocked(upstreamID, project, sessionID string, env protocol.Envelope) {
	if sessionID == "" {
		return
	}
	s := h.getOrCreateSessionLocked(upstreamID, project, sessionID)
	s.pendingPrompt = env.ID
	s.pendingPromptType = env.Type

	// The conversation carries the prompt too. Without this the phone's
	// approve/reject panel has nothing to render from: the conversation
	// endpoint is the only thing the view reads.
	s.appendConv(env)
	s.touch(nowMs())
	s.wake()
}

// getOrCreateSessionLocked returns the session, creating a minimal entry if
// it does not exist. Must be called with mu held.
func (h *Hub) getOrCreateSessionLocked(upstreamID, project, sessionID string) *sess {
	if s, ok := h.sessions[sessionID]; ok {
		// Keep upstream/project current: a reloaded window re-registers and
		// re-emits, and the upstream ID may have changed even if the session
		// is the same.
		if upstreamID != "" {
			s.upstream = upstreamID
		}
		if project != "" {
			s.project = project
		}
		return s
	}
	h.evictSessionsLocked()
	s := &sess{
		id:         sessionID,
		upstream:   upstreamID,
		project:    project,
		status:     protocol.StatusUnknown,
		convSize:   h.convSize,
		convWakers: make(map[chan struct{}]bool),
		updated:    nowMs(),
	}
	h.sessions[sessionID] = s
	return s
}

// evictSessionsLocked makes room for one new session, dropping the least
// recently active. A session with an unanswered prompt is never evicted: it is
// the one row the user most needs to find. Must be called with mu held.
func (h *Hub) evictSessionsLocked() {
	for len(h.sessions) >= h.maxSessions {
		var oldestID string
		var oldest int64
		for id, s := range h.sessions {
			if s.pendingPrompt != "" {
				continue
			}
			if oldestID == "" || s.updated < oldest {
				oldestID, oldest = id, s.updated
			}
		}
		if oldestID == "" {
			return // every session has a prompt pending; keep them all
		}
		delete(h.sessions, oldestID)
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

	h.clearSessionPromptLocked(p.sessionID, envID)

	h.publishLocked(protocol.Envelope{
		ID:      envID, // same ID so the watch knows which to dismiss
		Type:    protocol.EventGone,
		Project: p.env.Project,
		Session: p.env.Session,
	})
}

// clearSessionPromptLocked drops a session's pending-prompt badge and retracts
// the prompt from its conversation, so the phone's inline approve/reject panel
// disappears the moment the prompt is settled — wherever it was settled.
// Must be called with mu held.
func (h *Hub) clearSessionPromptLocked(sessionID, envID string) {
	s, ok := h.sessions[sessionID]
	if !ok {
		return
	}
	if s.pendingPrompt == envID {
		s.pendingPrompt = ""
		s.pendingPromptType = ""
	}

	// Replace the prompt in the conversation with a gone envelope under the
	// same ID. The phone keys its rendered views by ID, so this updates the
	// panel in place rather than leaving a dead set of buttons on screen.
	for i := range s.conv {
		if s.conv[i].ID != envID {
			continue
		}
		s.convSeq++
		s.conv[i] = protocol.Envelope{
			ID:      envID,
			Seq:     s.convSeq,
			Type:    protocol.EventGone,
			Project: s.conv[i].Project,
			Session: sessionID,
			Title:   s.conv[i].Title,
			Body:    s.conv[i].Body,
		}
		break
	}
	s.wake()
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

	// Re-check now that the waker is registered: an envelope published between
	// the read above and the subscribe woke nobody. For a permission prompt
	// that is the difference between the watch buzzing now and buzzing when the
	// poll times out.
	if resp, err := h.pollOnce(cursor); err != nil || len(resp.Events) > 0 {
		return resp, err
	}

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

	// Clear the badge here rather than waiting for Kilo's permission.replied
	// to come back around. That event is the only other thing that clears it,
	// and it never arrives if the upstream drops between the reply and the
	// ack — which would leave the session list advertising a prompt that has
	// already been answered.
	h.clearSessionPromptLocked(p.sessionID, req.EventID)

	up, ok := h.upstreams[p.upstream]
	if !ok {
		return ErrUpstreamGone
	}

	h.queueDecisionLocked(up, protocol.Decision{
		RequestID: p.requestID,
		SessionID: p.sessionID,
		Kind:      string(p.env.Type),
		Action:    req.Action,
		Choice:    req.Choice,
		Text:      req.Text,
		Expires:   p.env.Expires,
	})
	return nil
}

// queueDecisionLocked stamps a decision with the global sequence, queues it for
// one upstream, and wakes that upstream's long-poll. Must be called with mu
// held.
func (h *Hub) queueDecisionLocked(up *upstream, dec protocol.Decision) {
	h.decSeq++
	dec.ID = fmt.Sprintf("dec_%d", h.decSeq)
	dec.Nonce = randNonce()

	up.decisions = append(up.decisions, dec)
	if len(up.decisions) > maxQueuedDecisions {
		up.decisions = up.decisions[len(up.decisions)-maxQueuedDecisions:]
	}
	// The cursor the plugin echoes back must live in the same number space as
	// the IDs it is compared against. See the note on upstream.decCursor.
	up.decCursor = h.decSeq

	for ch := range up.decWakers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
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

	// Re-check now that the waker is registered — a decision queued in the gap
	// woke nobody, and an approval should not wait out the poll.
	if resp, err := h.pollDecisionsOnce(upstreamID, cursor); err != nil || len(resp.Decisions) > 0 {
		return resp, err
	}

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

// Prompt forwards dictated text to a session as a prompt decision.
//
// The plugin applies it via Kilo's prompt_async; prh never holds credentials,
// so it cannot send the text itself. Same inversion as permission replies.
//
// sessionID identifies which session to prompt. We find its upstream and
// queue a kind:"prompt" decision for that upstream's plugin.
func (h *Hub) Prompt(sessionID, text string) error {
	text = truncate(text, protocol.MaxPromptText)
	if text == "" {
		return ErrEmptyPrompt
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	s, ok := h.sessions[sessionID]
	if !ok {
		return ErrUnknownSession
	}
	up, ok := h.upstreams[s.upstream]
	if !ok {
		return ErrUpstreamGone
	}

	h.queueDecisionLocked(up, protocol.Decision{
		SessionID: sessionID,
		Kind:      "prompt",
		Action:    protocol.ActionText,
		Text:      text,
	})

	// The phone just gave the session work; reflect that immediately rather
	// than waiting for the first part to stream back, so the row does not sit
	// at the bottom of a recency-sorted list right after being used.
	s.touch(nowMs())
	s.status = protocol.StatusBusy
	return nil
}

// ErrUnknownSession means prh has no record of that session ID.
var ErrUnknownSession = errors.New("hub: unknown session")

// ErrUpstreamGone means the window that owned the session is no longer
// connected, so there is no plugin left to apply the decision. Distinct from
// ErrAlreadyAnswered: nothing was answered, there is simply nowhere to send it.
var ErrUpstreamGone = errors.New("hub: session upstream gone")

// ErrEmptyPrompt means the text was empty (or whitespace that truncated away).
var ErrEmptyPrompt = errors.New("hub: empty prompt text")

// Sessions returns a summary of every known session for GET /v1/sessions.
// Built from the session registry, which is built from events — no Kilo API
// call.
func (h *Hub) Sessions() []protocol.SessionSummary {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := make([]protocol.SessionSummary, 0, len(h.sessions))
	for _, s := range h.sessions {
		summary := protocol.SessionSummary{
			ID:      s.id,
			Project: s.project,
			Title:   s.title,
			Dir:     dirBase(s.dir),
			Status:  s.status,
			Updated: s.updated,
		}
		if s.pendingPrompt != "" {
			summary.HasPrompt = true
			summary.PromptID = s.pendingPrompt
			summary.PromptType = string(s.pendingPromptType)
		}
		out = append(out, summary)
	}

	// Map iteration is randomised, so an unsorted list reshuffles on every
	// refresh. Newest first, with a pending prompt always on top: the row that
	// needs an answer is the reason the list is open.
	sort.Slice(out, func(i, j int) bool {
		if out[i].HasPrompt != out[j].HasPrompt {
			return out[i].HasPrompt
		}
		if out[i].Updated != out[j].Updated {
			return out[i].Updated > out[j].Updated
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Conversation returns the conversation history for a session after the given
// per-session cursor, blocking up to wait for new messages. Used by
// GET /v1/sessions/{id}/conversation.
func (h *Hub) Conversation(ctx context.Context, sessionID string, cursor uint64, wait time.Duration) (protocol.ConversationResponse, error) {
	if resp, err := h.conversationOnce(sessionID, cursor); err != nil || len(resp.Events) > 0 || resp.HasMore {
		return resp, err
	}

	if wait <= 0 {
		return h.conversationOnce(sessionID, cursor)
	}

	// Look up and subscribe under one lock. Split across two, the session can
	// be deleted in between and the waker lands on an orphaned struct that
	// nothing will ever signal — the poller then blocks for the full wait
	// instead of returning "unknown session" straight away.
	wake := make(chan struct{}, 1)
	h.mu.Lock()
	s, ok := h.sessions[sessionID]
	if ok {
		s.convWakers[wake] = true
	}
	h.mu.Unlock()
	if !ok {
		return protocol.ConversationResponse{}, ErrUnknownSession
	}
	defer func() {
		h.mu.Lock()
		delete(s.convWakers, wake)
		h.mu.Unlock()
	}()

	// Re-check now that the waker is registered. A message that landed between
	// the read above and the subscribe signalled nobody, and without this the
	// caller would sit out the full wait before noticing it — the agent's reply
	// appearing on the phone up to a minute late, for no reason.
	if resp, err := h.conversationOnce(sessionID, cursor); err != nil || len(resp.Events) > 0 || resp.HasMore {
		return resp, err
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return protocol.ConversationResponse{}, ctx.Err()
		case <-timer.C:
			return h.conversationOnce(sessionID, cursor)
		case <-wake:
			if resp, err := h.conversationOnce(sessionID, cursor); err != nil || len(resp.Events) > 0 || resp.HasMore {
				return resp, err
			}
		}
	}
}

// conversationOnce returns the session's conversation after cursor. Does not
// block.
func (h *Hub) conversationOnce(sessionID string, cursor uint64) (protocol.ConversationResponse, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	s, ok := h.sessions[sessionID]
	if !ok {
		return protocol.ConversationResponse{}, ErrUnknownSession
	}

	resp := protocol.ConversationResponse{Cursor: s.convSeq}
	for _, env := range s.conv {
		if env.Seq > cursor {
			resp.Events = append(resp.Events, env)
		}
	}
	return resp, nil
}

// nowMs is the current time in unix milliseconds, the unit every session and
// message timestamp on this path uses.
func nowMs() int64 { return time.Now().UnixMilli() }

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

// dirBase returns the basename of a directory path. Used to label sessions on
// the phone the same way upstreams are labelled on the watch.
//
// path.Base rather than filepath.Base: these paths arrive over the wire from
// the plugin, so they must be parsed the same way regardless of the OS prh
// happens to run on. Backslashes are folded first so a Windows path still
// yields its last component.
func dirBase(dir string) string {
	if dir == "" {
		return ""
	}
	b := path.Base(strings.ReplaceAll(dir, `\`, "/"))
	if b == "." || b == "/" {
		return ""
	}
	return truncate(b, protocol.MaxTitle)
}
