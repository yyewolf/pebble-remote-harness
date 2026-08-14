package hub

import (
	"context"
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
