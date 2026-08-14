package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yyewolf/pebble-remote-harness/api/internal/auth"
	"github.com/yyewolf/pebble-remote-harness/api/internal/config"
	"github.com/yyewolf/pebble-remote-harness/api/internal/hub"
	"github.com/yyewolf/pebble-remote-harness/api/internal/protocol"
)

func newTestServer(t *testing.T, passwordHash string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.PasswordHash = passwordHash
	cfg.MaxPollWaitSec = 2
	h := hub.New(50)
	devices := auth.NewRegistry(passwordHash)
	return New(cfg, h, devices, slog.New(slog.DiscardHandler))
}

func doJSON(t *testing.T, srv *Server, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func doPluginJSON(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.PluginHandler().ServeHTTP(w, req)
	return w
}

func TestHandleHealth(t *testing.T) {
	srv := newTestServer(t, "")
	w := doJSON(t, srv, "GET", "/v1/health", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var h protocol.Health
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Version != protocol.Version {
		t.Errorf("version = %q", h.Version)
	}
	if h.Paired {
		t.Error("should not be paired with empty password")
	}
}

func TestHandleRegister(t *testing.T) {
	srv := newTestServer(t, "plain:secret")

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password:   "secret",
		DeviceName: "Pixel",
		Platform:   "android",
	}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp protocol.RegisterResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" {
		t.Error("empty token")
	}
	if resp.DeviceID == "" {
		t.Error("empty device ID")
	}
}

func TestHandleRegisterBadPassword(t *testing.T) {
	srv := newTestServer(t, "plain:secret")

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password: "wrong",
	}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandleRegisterUnpaired(t *testing.T) {
	srv := newTestServer(t, "")

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password: "anything",
	}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandlePollNoToken(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doJSON(t, srv, "GET", "/v1/poll?cursor=0", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandlePollBadToken(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doJSON(t, srv, "GET", "/v1/poll?cursor=0", nil, "prh_bogus")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandlePollEmpty(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	_, token, _ := srv.devices.Register("dev", "android")

	w := doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp protocol.PollResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 0 {
		t.Errorf("events = %d, want 0", len(resp.Events))
	}
}

func TestHandlePollEvents(t *testing.T) {
	srv := newTestServer(t, "plain:secret")

	// Register upstream and send a permission event.
	w := doPluginJSON(t, srv, "POST", "/plugin/v1/hello", protocol.PluginHello{
		Protocol:  protocol.Version,
		Directory: "/home/me/infra",
		ParentPID: 0,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("hello status = %d, body %s", w.Code, w.Body.String())
	}
	var helloResp protocol.PluginHelloResponse
	json.Unmarshal(w.Body.Bytes(), &helloResp)

	w = doPluginJSON(t, srv, "POST", "/plugin/v1/events", protocol.PluginEvents{
		UpstreamID: helloResp.UpstreamID,
		Events: []protocol.PluginEvent{
			{
				Kind:      "permission",
				RequestID: "per_1",
				SessionID: "ses_1",
				Action:    "bash",
				Resources: []string{"rm -rf build/"},
				Always:    []string{"rm *"},
			},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("events status = %d, body %s", w.Code, w.Body.String())
	}

	// Register a device and poll.
	_, token, _ := srv.devices.Register("dev", "android")
	w = doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("poll status = %d", w.Code)
	}
	var resp protocol.PollResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.Events))
	}
	env := resp.Events[0]
	if env.Type != protocol.EventPerm {
		t.Errorf("type = %s, want perm", env.Type)
	}
	if env.Project != "infra" {
		t.Errorf("project = %q, want infra", env.Project)
	}
	if env.Title != "bash" {
		t.Errorf("title = %q", env.Title)
	}
	if env.Body != "rm -rf build/" {
		t.Errorf("body = %q", env.Body)
	}
	if len(env.Choices) != 3 || env.Choices[1] != "Always: rm *" {
		t.Errorf("choices = %v", env.Choices)
	}
}

func TestHandlePluginHelloProtocolMismatch(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doPluginJSON(t, srv, "POST", "/plugin/v1/hello", protocol.PluginHello{
		Protocol: "v2",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestHandlePluginEventsBadUpstream(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doPluginJSON(t, srv, "POST", "/plugin/v1/events", protocol.PluginEvents{
		UpstreamID: "up_bogus",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandlePollGoneCursor(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	// Fill the ring beyond capacity.
	for i := 0; i < 60; i++ {
		srv.hub.Publish(protocol.Envelope{Type: protocol.EventNote})
	}
	_, token, _ := srv.devices.Register("dev", "android")

	w := doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, token)
	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", w.Code)
	}
}

func TestHandlePollLongPollWakes(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	_, token, _ := srv.devices.Register("dev", "android")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=3", nil, token)
	}()

	time.Sleep(100 * time.Millisecond)
	srv.hub.Publish(protocol.Envelope{Type: protocol.EventNote})

	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		var resp protocol.PollResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Events) != 1 {
			t.Errorf("events = %d, want 1", len(resp.Events))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long-poll did not return")
	}
}

func TestHandlePollContextCancel(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	_, token, _ := srv.devices.Register("dev", "android")

	req := httptest.NewRequest("GET", "/v1/poll?cursor=0&wait=5", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp protocol.PollResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Events) != 0 {
		t.Errorf("events = %d, want 0", len(resp.Events))
	}
}

// --- M2: reply → decisions → ack ---

func TestReplyAndPluginDecisions(t *testing.T) {
	srv := newTestServer(t, "plain:secret")

	// Register upstream and send a permission event.
	w := doPluginJSON(t, srv, "POST", "/plugin/v1/hello", protocol.PluginHello{
		Protocol:  protocol.Version,
		Directory: "/home/me/infra",
	})
	var helloResp protocol.PluginHelloResponse
	json.Unmarshal(w.Body.Bytes(), &helloResp)

	doPluginJSON(t, srv, "POST", "/plugin/v1/events", protocol.PluginEvents{
		UpstreamID: helloResp.UpstreamID,
		Events: []protocol.PluginEvent{{
			Kind:      "permission",
			RequestID: "per_1",
			SessionID: "ses_1",
			Action:    "bash",
			Resources: []string{"rm -rf build/"},
		}},
	})

	// Register a device and poll to get the event ID.
	_, token, _ := srv.devices.Register("dev", "android")
	w = doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, token)
	var pollResp protocol.PollResponse
	json.Unmarshal(w.Body.Bytes(), &pollResp)
	if len(pollResp.Events) != 1 {
		t.Fatalf("poll events = %d, want 1", len(pollResp.Events))
	}
	envID := pollResp.Events[0].ID

	// Reply: approve once.
	w = doJSON(t, srv, "POST", "/v1/reply", protocol.ReplyRequest{
		EventID: envID,
		Action:  protocol.ActionOnce,
	}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("reply status = %d, body %s", w.Code, w.Body.String())
	}

	// Plugin long-polls decisions.
	w = doPluginJSON(t, srv, "GET", "/plugin/v1/decisions?upstream_id="+helloResp.UpstreamID+"&cursor=0&wait=0", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("decisions status = %d, body %s", w.Code, w.Body.String())
	}
	var decResp protocol.DecisionsResponse
	json.Unmarshal(w.Body.Bytes(), &decResp)
	if len(decResp.Decisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(decResp.Decisions))
	}
	dec := decResp.Decisions[0]
	if dec.RequestID != "per_1" {
		t.Errorf("request_id = %q", dec.RequestID)
	}
	if dec.Action != protocol.ActionOnce {
		t.Errorf("action = %q, want once", dec.Action)
	}
	if dec.Nonce == "" {
		t.Error("empty nonce")
	}

	// Ack.
	w = doPluginJSON(t, srv, "POST", "/plugin/v1/ack", protocol.DecisionAck{
		UpstreamID: helloResp.UpstreamID,
		ID:         dec.ID,
		Status:     "applied",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("ack status = %d", w.Code)
	}
}

func TestReplyAlreadyAnswered(t *testing.T) {
	srv := newTestServer(t, "plain:secret")

	w := doPluginJSON(t, srv, "POST", "/plugin/v1/hello", protocol.PluginHello{
		Protocol:  protocol.Version,
		Directory: "/home/me/infra",
	})
	var helloResp protocol.PluginHelloResponse
	json.Unmarshal(w.Body.Bytes(), &helloResp)

	doPluginJSON(t, srv, "POST", "/plugin/v1/events", protocol.PluginEvents{
		UpstreamID: helloResp.UpstreamID,
		Events: []protocol.PluginEvent{{
			Kind:      "permission",
			RequestID: "per_1",
			SessionID: "ses_1",
			Action:    "bash",
			Resources: []string{"ls"},
		}},
	})

	_, token, _ := srv.devices.Register("dev", "android")
	w = doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, token)
	var pollResp protocol.PollResponse
	json.Unmarshal(w.Body.Bytes(), &pollResp)
	envID := pollResp.Events[0].ID

	// First reply succeeds.
	w = doJSON(t, srv, "POST", "/v1/reply", protocol.ReplyRequest{
		EventID: envID,
		Action:  protocol.ActionOnce,
	}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("first reply = %d", w.Code)
	}

	// Second reply must be 409.
	w = doJSON(t, srv, "POST", "/v1/reply", protocol.ReplyRequest{
		EventID: envID,
		Action:  protocol.ActionAlways,
	}, token)
	if w.Code != http.StatusConflict {
		t.Fatalf("second reply = %d, want 409", w.Code)
	}
}

func TestReplyUnknownEvent(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	_, token, _ := srv.devices.Register("dev", "android")

	w := doJSON(t, srv, "POST", "/v1/reply", protocol.ReplyRequest{
		EventID: "evt_bogus",
		Action:  protocol.ActionOnce,
	}, token)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestReplyNoToken(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doJSON(t, srv, "POST", "/v1/reply", protocol.ReplyRequest{
		EventID: "evt_1",
		Action:  protocol.ActionOnce,
	}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestPluginDecisionsBadUpstream(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doPluginJSON(t, srv, "GET", "/plugin/v1/decisions?upstream_id=up_bogus&cursor=0&wait=0", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
