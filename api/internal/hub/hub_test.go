package hub

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yyewolf/pebble-remote-harness/api/internal/protocol"
)

func TestTranslatePluginEvent_Permission(t *testing.T) {
	h := New(200)
	ev := protocol.PluginEvent{
		Kind:      "permission",
		RequestID: "per_1",
		SessionID: "ses_1",
		Action:    "bash",
		Resources: []string{"rm -rf build/"},
		Always:    []string{"rm *"},
	}
	env, needsReply := h.TranslatePluginEvent("infra", ev)
	if env.Type != protocol.EventPerm {
		t.Fatalf("type = %s, want perm", env.Type)
	}
	if !needsReply {
		t.Fatal("permission should need reply")
	}
	if env.Project != "infra" {
		t.Errorf("project = %q, want infra", env.Project)
	}
	if env.Title != "bash" {
		t.Errorf("title = %q, want bash", env.Title)
	}
	if env.Body != "rm -rf build/" {
		t.Errorf("body = %q, want rm -rf build/", env.Body)
	}
	if len(env.Choices) != 3 {
		t.Fatalf("choices = %v, want 3", env.Choices)
	}
	if env.Choices[1] != "Always: rm *" {
		t.Errorf("always choice = %q, want 'Always: rm *'", env.Choices[1])
	}
}

func TestTranslatePluginEvent_PermissionNoAlways(t *testing.T) {
	h := New(200)
	ev := protocol.PluginEvent{
		Kind:      "permission",
		RequestID: "per_1",
		Action:    "bash",
		Resources: []string{"ls -la"},
	}
	env, _ := h.TranslatePluginEvent("p", ev)
	if len(env.Choices) != 2 || env.Choices[0] != "Approve" || env.Choices[1] != "Reject" {
		t.Errorf("choices = %v, want [Approve Reject]", env.Choices)
	}
}

func TestTranslatePluginEvent_Idle(t *testing.T) {
	h := New(200)
	ev := protocol.PluginEvent{
		Kind:      "idle",
		SessionID: "ses_1",
	}
	env, needsReply := h.TranslatePluginEvent("proj", ev)
	if env.Type != protocol.EventIdle {
		t.Fatalf("type = %s, want idle", env.Type)
	}
	if needsReply {
		t.Fatal("idle should not need reply")
	}
}

func TestTranslatePluginEvent_Truncation(t *testing.T) {
	h := New(200)
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'x'
	}
	ev := protocol.PluginEvent{
		Kind:      "permission",
		Action:    string(long),
		Resources: []string{string(long)},
		Always:    []string{string(long)},
	}
	env, _ := h.TranslatePluginEvent("p", ev)
	if len(env.Title) > protocol.MaxTitle {
		t.Errorf("title len = %d, max %d", len(env.Title), protocol.MaxTitle)
	}
	if len(env.Body) > protocol.MaxBody {
		t.Errorf("body len = %d, max %d", len(env.Body), protocol.MaxBody)
	}
	if len(env.Choices[1]) > protocol.MaxChoice {
		t.Errorf("choice len = %d, max %d", len(env.Choices[1]), protocol.MaxChoice)
	}
}

func TestPublishAssignsSeq(t *testing.T) {
	h := New(200)
	env := protocol.Envelope{Type: protocol.EventPerm, Title: "bash"}
	h.Publish(env)

	if h.Cursor() != 1 {
		t.Fatalf("cursor = %d, want 1", h.Cursor())
	}
	resp, err := h.Poll(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.Events))
	}
	if resp.Events[0].Seq != 1 {
		t.Errorf("seq = %d, want 1", resp.Events[0].Seq)
	}
	if resp.Events[0].ID != "evt_1" {
		t.Errorf("id = %q, want evt_1", resp.Events[0].ID)
	}
}

func TestRingBufferEviction(t *testing.T) {
	h := New(3)
	for i := 0; i < 5; i++ {
		h.Publish(protocol.Envelope{Type: protocol.EventNote, Title: "n"})
	}
	// Only the last 3 should remain.
	if len(h.ring) != 3 {
		t.Fatalf("ring len = %d, want 3", len(h.ring))
	}
	if h.ring[0].Seq != 3 {
		t.Errorf("oldest seq = %d, want 3", h.ring[0].Seq)
	}

	// Cursor 0 is now too old.
	_, err := h.Poll(context.Background(), 0, 0)
	if err != ErrCursorTooOld {
		t.Fatalf("err = %v, want ErrCursorTooOld", err)
	}
}

func TestPollReturnsNewerThanCursor(t *testing.T) {
	h := New(10)
	h.Publish(protocol.Envelope{Type: protocol.EventNote})
	h.Publish(protocol.Envelope{Type: protocol.EventNote})

	resp, err := h.Poll(context.Background(), 1, 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.Events))
	}
	if resp.Events[0].Seq != 2 {
		t.Errorf("seq = %d, want 2", resp.Events[0].Seq)
	}
	if resp.Cursor != 2 {
		t.Errorf("cursor = %d, want 2", resp.Cursor)
	}
}

func TestPollLongPollWakes(t *testing.T) {
	h := New(10)
	done := make(chan struct{})
	go func() {
		resp, err := h.Poll(context.Background(), 0, 5*time.Second)
		if err != nil {
			t.Errorf("poll: %v", err)
		}
		if len(resp.Events) != 1 {
			t.Errorf("events = %d, want 1", len(resp.Events))
		}
		close(done)
	}()

	// Give the poller time to register.
	time.Sleep(50 * time.Millisecond)
	h.Publish(protocol.Envelope{Type: protocol.EventNote})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll did not wake")
	}
}

func TestPollContextCancel(t *testing.T) {
	h := New(10)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		cancel()
	}()
	_, err := h.Poll(ctx, 0, 5*time.Second)
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestIngestPluginEvents_Permission(t *testing.T) {
	h := New(50)
	h.RegisterUpstream("infra", "/home/me/infra", 0)

	h.IngestPluginEvents("up_1", "infra", []protocol.PluginEvent{
		{
			Kind:      "permission",
			RequestID: "per_1",
			SessionID: "ses_1",
			Action:    "bash",
			Resources: []string{"ls -la"},
			Always:    []string{"ls *"},
		},
	})

	resp, err := h.Poll(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.Events))
	}
	env := resp.Events[0]
	if env.Type != protocol.EventPerm {
		t.Errorf("type = %s, want perm", env.Type)
	}
	if env.Title != "bash" {
		t.Errorf("title = %q", env.Title)
	}
	if len(env.Choices) != 3 {
		t.Errorf("choices = %v", env.Choices)
	}
}

func TestIngestPluginEvents_Replied(t *testing.T) {
	h := New(50)
	h.RegisterUpstream("infra", "/home/me/infra", 0)

	h.IngestPluginEvents("up_1", "infra", []protocol.PluginEvent{
		{
			Kind:      "permission",
			RequestID: "per_1",
			SessionID: "ses_1",
			Action:    "bash",
			Resources: []string{"ls"},
		},
	})

	h.IngestPluginEvents("up_1", "infra", []protocol.PluginEvent{
		{
			Kind:      "replied",
			RequestID: "per_1",
			SessionID: "ses_1",
		},
	})

	resp, _ := h.Poll(context.Background(), 0, 0)
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2 (perm + gone)", len(resp.Events))
	}
	if resp.Events[1].Type != protocol.EventGone {
		t.Errorf("second event type = %s, want gone", resp.Events[1].Type)
	}
	if resp.Events[1].ID != resp.Events[0].ID {
		t.Errorf("gone ID = %q, want %q", resp.Events[1].ID, resp.Events[0].ID)
	}
}

func TestIngestPluginEvents_Idle(t *testing.T) {
	h := New(50)

	h.IngestPluginEvents("up_1", "infra", []protocol.PluginEvent{
		{Kind: "idle", SessionID: "ses_1"},
	})

	resp, _ := h.Poll(context.Background(), 0, 0)
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.Events))
	}
	if resp.Events[0].Type != protocol.EventIdle {
		t.Errorf("type = %s, want idle", resp.Events[0].Type)
	}
}

func TestRegisterUpstream(t *testing.T) {
	h := New(10)
	id := h.RegisterUpstream("infra", "/home/me/infra", 12345)
	if id == "" {
		t.Fatal("empty upstream ID")
	}
	if !h.UpstreamExists(id) {
		t.Fatal("upstream not found after register")
	}
	if h.UpstreamProject(id) != "infra" {
		t.Errorf("project = %q, want infra", h.UpstreamProject(id))
	}
	h.RemoveUpstream(id)
	if h.UpstreamExists(id) {
		t.Fatal("upstream still found after remove")
	}
}

func TestReplyQueuesDecision(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/home/me/infra", 0)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{
			Kind:      "permission",
			RequestID: "per_1",
			SessionID: "ses_1",
			Action:    "bash",
			Resources: []string{"ls"},
		},
	})

	// Get the envelope ID from the poll.
	pollResp, _ := h.Poll(context.Background(), 0, 0)
	envID := pollResp.Events[0].ID

	err := h.Reply(protocol.ReplyRequest{EventID: envID, Action: protocol.ActionOnce})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}

	// The plugin should see the decision.
	decResp, err := h.PollDecisions(context.Background(), upID, 0, 0)
	if err != nil {
		t.Fatalf("poll decisions: %v", err)
	}
	if len(decResp.Decisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(decResp.Decisions))
	}
	dec := decResp.Decisions[0]
	if dec.RequestID != "per_1" {
		t.Errorf("request_id = %q, want per_1", dec.RequestID)
	}
	if dec.Action != protocol.ActionOnce {
		t.Errorf("action = %q, want once", dec.Action)
	}
	if dec.Nonce == "" {
		t.Error("empty nonce")
	}
}

func TestReplyAlreadyAnswered(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/home/me/infra", 0)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{
			Kind:      "permission",
			RequestID: "per_1",
			SessionID: "ses_1",
			Action:    "bash",
			Resources: []string{"ls"},
		},
	})

	pollResp, _ := h.Poll(context.Background(), 0, 0)
	envID := pollResp.Events[0].ID

	if err := h.Reply(protocol.ReplyRequest{EventID: envID, Action: protocol.ActionOnce}); err != nil {
		t.Fatalf("first reply: %v", err)
	}

	// Second reply must fail — idempotent, not double-approve.
	err := h.Reply(protocol.ReplyRequest{EventID: envID, Action: protocol.ActionAlways})
	if err != ErrAlreadyAnswered {
		t.Fatalf("second reply: err = %v, want ErrAlreadyAnswered", err)
	}

	// Only one decision should be queued.
	decResp, _ := h.PollDecisions(context.Background(), upID, 0, 0)
	if len(decResp.Decisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(decResp.Decisions))
	}
	if decResp.Decisions[0].Action != protocol.ActionOnce {
		t.Errorf("action = %q, want once (first)", decResp.Decisions[0].Action)
	}
}

func TestReplyUnknownEvent(t *testing.T) {
	h := New(50)
	err := h.Reply(protocol.ReplyRequest{EventID: "evt_bogus", Action: protocol.ActionOnce})
	if err != ErrAlreadyAnswered {
		t.Fatalf("err = %v, want ErrAlreadyAnswered", err)
	}
}

func TestPollDecisionsLongPollWakes(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/home/me/infra", 0)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{
			Kind:      "permission",
			RequestID: "per_1",
			SessionID: "ses_1",
			Action:    "bash",
			Resources: []string{"ls"},
		},
	})
	pollResp, _ := h.Poll(context.Background(), 0, 0)
	envID := pollResp.Events[0].ID

	done := make(chan protocol.DecisionsResponse, 1)
	go func() {
		resp, err := h.PollDecisions(context.Background(), upID, 0, 5*time.Second)
		if err != nil {
			t.Errorf("poll decisions: %v", err)
		}
		done <- resp
	}()

	time.Sleep(50 * time.Millisecond)
	h.Reply(protocol.ReplyRequest{EventID: envID, Action: protocol.ActionReject})

	select {
	case resp := <-done:
		if len(resp.Decisions) != 1 {
			t.Fatalf("decisions = %d, want 1", len(resp.Decisions))
		}
		if resp.Decisions[0].Action != protocol.ActionReject {
			t.Errorf("action = %q, want reject", resp.Decisions[0].Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decision long-poll did not wake")
	}
}

func TestPollDecisionsContextCancel(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/home/me/infra", 0)

	ctx, cancel := context.WithCancel(context.Background())
	go cancel()

	_, err := h.PollDecisions(ctx, upID, 0, 5*time.Second)
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A prh restart resets the in-memory sequence to zero while the companion
// keeps the cursor it persisted. Without this, the phone polls with a cursor
// nothing will ever exceed, gets 200 and an empty list, and silently misses
// every event until the sequence climbs back past it. Seen on hardware.
func TestPollRejectsACursorFromAPreviousEpoch(t *testing.T) {
	h := New(50)
	h.Publish(protocol.Envelope{Type: protocol.EventPerm})

	// The client's cursor is far ahead of anything this daemon has issued.
	_, err := h.Poll(context.Background(), 10, 0)
	if !errors.Is(err, ErrCursorTooOld) {
		t.Fatalf("err = %v, want ErrCursorTooOld so the client resets", err)
	}

	// After resetting, it sees the backlog.
	resp, err := h.Poll(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("poll after reset: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events after reset = %d, want 1", len(resp.Events))
	}
}

// The boundary must stay usable: a cursor exactly at the newest event is a
// caller that is simply up to date, not one from another epoch.
func TestPollAcceptsACursorAtTheHead(t *testing.T) {
	h := New(50)
	h.Publish(protocol.Envelope{Type: protocol.EventPerm})

	resp, err := h.Poll(context.Background(), h.Cursor(), 0)
	if err != nil {
		t.Fatalf("cursor at head rejected: %v", err)
	}
	if len(resp.Events) != 0 {
		t.Fatalf("events = %d, want 0", len(resp.Events))
	}
}

// -- conversation + sessions + prompt ---------------------------------------

func TestMessageEnvelopesGoToConversationBuffer(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/home/you/work/infra", 1)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "message", SessionID: "ses_1", MsgRole: "assistant", MsgPartID: "p_1", MsgText: "hello", MsgKind: "text", MsgTimeMs: 1000},
		{Kind: "message", SessionID: "ses_1", MsgRole: "assistant", MsgPartID: "p_1", MsgText: "hello world", MsgKind: "text", MsgTimeMs: 1100},
		{Kind: "message", SessionID: "ses_1", MsgRole: "assistant", MsgPartID: "p_2", MsgText: "second", MsgKind: "text", MsgTimeMs: 1200},
	})

	// Two parts, not three: the second p_1 is the *same* part with more text.
	// The agent streams by re-sending a growing part, so an append-only buffer
	// would fill with partials of whatever is being written now and push the
	// real history out of a 50-entry window.
	resp, err := h.Conversation(context.Background(), "ses_1", 0, 0)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(resp.Events))
	}
	// Replaced in place: p_1 keeps its position ahead of p_2, with the latest
	// text.
	if resp.Events[0].MsgPartID != "p_1" || resp.Events[0].MsgText != "hello world" {
		t.Errorf("first entry = %q/%q, want p_1/'hello world'",
			resp.Events[0].MsgPartID, resp.Events[0].MsgText)
	}
	if resp.Events[1].MsgPartID != "p_2" {
		t.Errorf("second entry = %q, want p_2", resp.Events[1].MsgPartID)
	}
	// The cursor still counts every update, so a long-poller parked on the
	// old cursor is woken by an edit to a part it has already seen.
	if resp.Cursor != 3 {
		t.Errorf("cursor = %d, want 3", resp.Cursor)
	}
}

// A streaming reply must not be able to push the conversation out of the
// buffer. This is the failure the part-ID keying exists to prevent: open the
// view while the agent is mid-reply and the history should still be there.
func TestStreamingPartDoesNotEvictHistory(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/home/you/work/infra", 1)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "message", SessionID: "ses_1", MsgRole: "user", MsgPartID: "p_ask", MsgText: "fix the build", MsgKind: "text"},
	})

	// One part, updated far more times than the buffer could hold.
	for i := 0; i < 500; i++ {
		h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
			{Kind: "message", SessionID: "ses_1", MsgRole: "assistant", MsgPartID: "p_reply", MsgText: strings.Repeat("x", i+1), MsgKind: "text"},
		})
	}

	resp, err := h.Conversation(context.Background(), "ses_1", 0, 0)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2 (the question and the reply)", len(resp.Events))
	}
	if resp.Events[0].MsgPartID != "p_ask" {
		t.Errorf("the question was evicted by its own answer: first entry = %q", resp.Events[0].MsgPartID)
	}
}

// Conversation parts are phone-only. Publishing them to the global ring would
// spend the phone's mobile data on content the poll path discards, and — worse
// — evict pending permission envelopes from a small ring before the watch ever
// polls them.
func TestMessagesStayOutOfTheWatchRing(t *testing.T) {
	h := New(8)
	upID := h.RegisterUpstream("infra", "/home/you/work/infra", 1)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "permission", RequestID: "per_1", SessionID: "ses_1", Action: "bash", Resources: []string{"rm -rf /"}},
	})

	for i := 0; i < 50; i++ {
		h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
			{Kind: "message", SessionID: "ses_1", MsgPartID: "p_" + strconv.Itoa(i), MsgText: "chatter", MsgKind: "text"},
		})
	}

	resp, err := h.Poll(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("ring holds %d events, want 1 — conversation leaked into the watch stream", len(resp.Events))
	}
	if resp.Events[0].Type != protocol.EventPerm {
		t.Fatalf("the permission was evicted by conversation traffic: type = %q", resp.Events[0].Type)
	}
}

// The prompt must reach the conversation, or the phone's inline approve/reject
// panel has nothing to render: the conversation endpoint is the only thing
// that view reads.
func TestPendingPromptReachesTheConversation(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/home/you/work/infra", 1)

	// No session.created first: the permission is the very first thing prh
	// hears about this session, which is what happens when the plugin loads
	// mid-session or prh restarts while the agent is working.
	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "permission", RequestID: "per_1", SessionID: "ses_1", Action: "bash", Resources: []string{"rm ../fds"}},
	})

	sessions := h.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 — a session with a pending prompt is missing from the list", len(sessions))
	}
	if !sessions[0].HasPrompt || sessions[0].PromptType != string(protocol.EventPerm) {
		t.Errorf("summary = %+v, want a pending perm prompt", sessions[0])
	}

	resp, err := h.Conversation(context.Background(), "ses_1", 0, 0)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	if len(resp.Events) != 1 || resp.Events[0].Type != protocol.EventPerm {
		t.Fatalf("conversation = %+v, want the pending permission", resp.Events)
	}

	// Answering it clears the badge and retracts the prompt from the
	// conversation, so the panel does not linger with dead buttons.
	if err := h.Reply(protocol.ReplyRequest{EventID: resp.Events[0].ID, Action: protocol.ActionOnce}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if sessions := h.Sessions(); sessions[0].HasPrompt {
		t.Error("badge still set after the prompt was answered")
	}
	resp, err = h.Conversation(context.Background(), "ses_1", 0, 0)
	if err != nil {
		t.Fatalf("conversation after reply: %v", err)
	}
	if len(resp.Events) != 1 || resp.Events[0].Type != protocol.EventGone {
		t.Fatalf("conversation = %+v, want the prompt retracted as gone", resp.Events)
	}
}

// Two windows share one global decision sequence. The cursor the plugin echoes
// back has to live in that same space, or it lands below its own decisions'
// IDs and every poll redelivers work that was already applied.
func TestDecisionCursorSurvivesASecondWindow(t *testing.T) {
	h := New(50)
	upA := h.RegisterUpstream("a", "/home/you/a", 1)
	upB := h.RegisterUpstream("b", "/home/you/b", 2)

	// Three decisions on A push the global sequence ahead of B's own count.
	for i := 0; i < 3; i++ {
		h.IngestPluginEvents(upA, "a", []protocol.PluginEvent{
			{Kind: "permission", RequestID: "per_a" + strconv.Itoa(i), SessionID: "ses_a", Action: "bash", Resources: []string{"ls"}},
		})
		resp, _ := h.Poll(context.Background(), uint64(i), 0)
		if err := h.Reply(protocol.ReplyRequest{EventID: resp.Events[0].ID, Action: protocol.ActionOnce}); err != nil {
			t.Fatalf("reply on A: %v", err)
		}
	}

	h.IngestPluginEvents(upB, "b", []protocol.PluginEvent{
		{Kind: "message", SessionID: "ses_b", MsgPartID: "p_1", MsgText: "hi", MsgKind: "text"},
	})
	if err := h.Prompt("ses_b", "carry on"); err != nil {
		t.Fatalf("prompt on B: %v", err)
	}

	resp, err := h.PollDecisions(context.Background(), upB, 0, 0)
	if err != nil {
		t.Fatalf("poll decisions: %v", err)
	}
	if len(resp.Decisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(resp.Decisions))
	}

	// Poll again with the cursor just handed out. A correct cursor drains the
	// queue; a per-upstream count would resend the same prompt forever.
	again, err := h.PollDecisions(context.Background(), upB, resp.Cursor, 0)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if len(again.Decisions) != 0 {
		t.Fatalf("decisions redelivered after acking the cursor: %+v", again.Decisions)
	}
}

func TestConversationRespectsCursor(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/home/you/work/infra", 1)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "message", SessionID: "ses_1", MsgPartID: "p_1", MsgText: "a", MsgKind: "text"},
		{Kind: "message", SessionID: "ses_1", MsgPartID: "p_2", MsgText: "b", MsgKind: "text"},
	})

	resp, err := h.Conversation(context.Background(), "ses_1", 1, 0)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1 (only after cursor 1)", len(resp.Events))
	}
	if resp.Events[0].MsgText != "b" {
		t.Errorf("event text = %q, want b", resp.Events[0].MsgText)
	}
}

func TestSessionRegistryFromEvents(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/home/you/work/infra", 1)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "session.created", SessionID: "ses_1", SessionTitle: "Fix bug", SessionDir: "/work/infra", SessionUpdatedMs: 5000},
		{Kind: "session.status", SessionID: "ses_1", SessionStatus: "busy"},
	})

	sessions := h.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	s := sessions[0]
	if s.ID != "ses_1" {
		t.Errorf("id = %q, want ses_1", s.ID)
	}
	if s.Title != "Fix bug" {
		t.Errorf("title = %q, want 'Fix bug'", s.Title)
	}
	if s.Status != "busy" {
		t.Errorf("status = %q, want busy", s.Status)
	}
	if s.Dir != "infra" {
		t.Errorf("dir = %q, want infra", s.Dir)
	}
}

func TestSessionListBadgesPendingPrompt(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/work/infra", 1)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "session.created", SessionID: "ses_1"},
		{Kind: "permission", RequestID: "per_1", SessionID: "ses_1", Action: "bash", Resources: []string{"rm -rf build/"}},
	})

	sessions := h.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if !sessions[0].HasPrompt {
		t.Fatal("session should badge as having a pending prompt")
	}
	if sessions[0].PromptType != "perm" {
		t.Errorf("prompt type = %q, want perm", sessions[0].PromptType)
	}
}

func TestRepliedClearsSessionPromptBadge(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/work/infra", 1)

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "session.created", SessionID: "ses_1"},
		{Kind: "permission", RequestID: "per_1", SessionID: "ses_1", Action: "bash", Resources: []string{"rm -rf build/"}},
	})
	if !h.Sessions()[0].HasPrompt {
		t.Fatal("expected badge before reply")
	}

	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "replied", RequestID: "per_1", SessionID: "ses_1"},
	})
	if h.Sessions()[0].HasPrompt {
		t.Fatal("badge should clear after reply")
	}
}

func TestPromptQueuesDecisionForOwningUpstream(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/work/infra", 1)
	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "session.created", SessionID: "ses_1"},
	})

	if err := h.Prompt("ses_1", "run the tests"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	resp, err := h.PollDecisions(context.Background(), upID, 0, 0)
	if err != nil {
		t.Fatalf("poll decisions: %v", err)
	}
	if len(resp.Decisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(resp.Decisions))
	}
	dec := resp.Decisions[0]
	if dec.Kind != "prompt" {
		t.Errorf("kind = %q, want prompt", dec.Kind)
	}
	if dec.Text != "run the tests" {
		t.Errorf("text = %q, want 'run the tests'", dec.Text)
	}
	if dec.SessionID != "ses_1" {
		t.Errorf("session = %q, want ses_1", dec.SessionID)
	}
}

func TestPromptUnknownSession(t *testing.T) {
	h := New(50)
	if err := h.Prompt("nope", "text"); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("err = %v, want ErrUnknownSession", err)
	}
}

func TestRemoveUpstreamDropsItsSessions(t *testing.T) {
	h := New(50)
	upID := h.RegisterUpstream("infra", "/work/infra", 1)
	h.IngestPluginEvents(upID, "infra", []protocol.PluginEvent{
		{Kind: "session.created", SessionID: "ses_1"},
	})

	h.RemoveUpstream(upID)

	if _, err := h.Conversation(context.Background(), "ses_1", 0, 0); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("err = %v, want ErrUnknownSession after upstream removed", err)
	}
}

func TestMsgEnvelopesCrossBluetoothFalse(t *testing.T) {
	if protocol.EventMsg.CrossesBluetooth() {
		t.Fatal("msg envelopes must not cross Bluetooth")
	}
	if !protocol.EventPerm.CrossesBluetooth() {
		t.Fatal("perm envelopes must cross Bluetooth")
	}
}
