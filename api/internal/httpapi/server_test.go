package httpapi

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"

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

// signer holds what a paired companion holds: a key ID and the key itself.
// nil means an unauthenticated request.
type signer struct {
	keyID string
	key   []byte

	// nonce is fixed when a test wants to prove a replay is refused.
	nonce string
	// skew shifts the signed date to exercise the leeway check.
	skew time.Duration
}

// registerAndLogin does what the companion does on first run: pair with the
// passphrase, then trade the device secret for a session key.
func registerAndLogin(t *testing.T, srv *Server) *signer {
	t.Helper()
	dev, err := srv.devices.Register("dev", "android")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	sess, _, err := srv.devices.NewSession(dev.ID)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return &signer{keyID: sess.KeyID, key: sess.Key}
}

// deviceSigner signs with the device secret, which is what POST /v1/login
// requires.
func deviceSigner(t *testing.T, srv *Server, deviceID string) *signer {
	t.Helper()
	key, err := srv.devices.DeviceKey(deviceID)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	return &signer{keyID: deviceID, key: key}
}

// openPairing arms an enrolment window and returns the key, which is what the
// QR would carry.
func openPairing(t *testing.T, srv *Server) []byte {
	t.Helper()
	key, err := auth.NewPairingKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.devices.OpenPairing(key, time.Minute); err != nil {
		t.Fatal(err)
	}
	return key
}

// openSealedSecret is the companion's half of the pairing wrap, written from
// the recipe in docs/protocol.md rather than by calling the sealing code, so
// that a drift between doc and implementation fails here.
func openSealedSecret(t *testing.T, pairingKey []byte, w *auth.WrappedSecret, deviceID string) []byte {
	t.Helper()

	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("base64: %v", err)
		}
		return b
	}

	wrapKey := make([]byte, 32)
	kdf := hkdf.New(sha256.New, pairingKey, dec(w.WrapSalt), []byte("prh-pairing-wrap-v1"))
	if _, err := io.ReadFull(kdf, wrapKey); err != nil {
		t.Fatalf("hkdf: %v", err)
	}
	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	out, err := gcm.Open(nil, dec(w.WrapNonce), dec(w.WrapSecret), []byte(deviceID))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return out
}

func randNonce(t *testing.T) string {
	t.Helper()
	raw := make([]byte, protocol.NonceMinLen)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func doJSON(t *testing.T, srv *Server, method, path string, body any, s *signer) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}

	var r io.Reader
	if buf != nil {
		r = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")

	if s != nil {
		nonce := s.nonce
		if nonce == "" {
			nonce = randNonce(t)
		}
		date := strconv.FormatInt(time.Now().Add(s.skew).Unix(), 10)
		req.Header.Set(protocol.HeaderKeyID, s.keyID)
		req.Header.Set(protocol.HeaderDate, date)
		req.Header.Set(protocol.HeaderNonce, nonce)
		// RequestURI on the server side is path+query, which is what the
		// middleware canonicalises; mirror it exactly or every test 401s.
		req.Header.Set(protocol.HeaderSig, auth.Sign(s.key,
			auth.Canonical(method, path, date, nonce, buf)))
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
	w := doJSON(t, srv, "GET", "/v1/health", nil, nil)
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
	openPairing(t, srv)

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password:   "secret",
		DeviceName: "Pixel",
		Platform:   "android",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp protocol.RegisterResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DeviceSecret == "" {
		t.Error("empty device secret")
	}
	if resp.DeviceID == "" {
		t.Error("empty device ID")
	}
}

func TestHandleRegisterBadPassword(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	openPairing(t, srv)

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password: "wrong",
	}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandleRegisterUnpaired(t *testing.T) {
	srv := newTestServer(t, "")
	openPairing(t, srv)

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password: "anything",
	}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandlePollUnsigned(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doJSON(t, srv, "GET", "/v1/poll?cursor=0", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandlePollUnknownKey(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doJSON(t, srv, "GET", "/v1/poll?cursor=0", nil, &signer{keyID: "key_bogus", key: []byte("nope")})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandlePollEmpty(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	sign := registerAndLogin(t, srv)

	w := doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign)
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
	sign := registerAndLogin(t, srv)
	w = doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign)
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
	sign := registerAndLogin(t, srv)

	w := doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign)
	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", w.Code)
	}
}

func TestHandlePollLongPollWakes(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	sign := registerAndLogin(t, srv)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=3", nil, sign)
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
	sign := registerAndLogin(t, srv)

	const path = "/v1/poll?cursor=0&wait=5"
	req := httptest.NewRequest("GET", path, nil)
	nonce := randNonce(t)
	date := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set(protocol.HeaderKeyID, sign.keyID)
	req.Header.Set(protocol.HeaderDate, date)
	req.Header.Set(protocol.HeaderNonce, nonce)
	req.Header.Set(protocol.HeaderSig, auth.Sign(sign.key, auth.Canonical("GET", path, date, nonce, nil)))

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
	sign := registerAndLogin(t, srv)
	w = doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign)
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
	}, sign)
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

	sign := registerAndLogin(t, srv)
	w = doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign)
	var pollResp protocol.PollResponse
	json.Unmarshal(w.Body.Bytes(), &pollResp)
	envID := pollResp.Events[0].ID

	// First reply succeeds.
	w = doJSON(t, srv, "POST", "/v1/reply", protocol.ReplyRequest{
		EventID: envID,
		Action:  protocol.ActionOnce,
	}, sign)
	if w.Code != http.StatusOK {
		t.Fatalf("first reply = %d", w.Code)
	}

	// Second reply must be 409.
	w = doJSON(t, srv, "POST", "/v1/reply", protocol.ReplyRequest{
		EventID: envID,
		Action:  protocol.ActionAlways,
	}, sign)
	if w.Code != http.StatusConflict {
		t.Fatalf("second reply = %d, want 409", w.Code)
	}
}

func TestReplyUnknownEvent(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	sign := registerAndLogin(t, srv)

	w := doJSON(t, srv, "POST", "/v1/reply", protocol.ReplyRequest{
		EventID: "evt_bogus",
		Action:  protocol.ActionOnce,
	}, sign)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestReplyNoToken(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doJSON(t, srv, "POST", "/v1/reply", protocol.ReplyRequest{
		EventID: "evt_1",
		Action:  protocol.ActionOnce,
	}, nil)
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

func TestRegisterRateLimited(t *testing.T) {
	srv := newTestServer(t, "plain:secret")

	// Use a low-threshold limiter for the test.
	srv.rateLimiter = newRateLimiter(3, 5*time.Minute)
	openPairing(t, srv)

	for i := 0; i < 3; i++ {
		w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
			Password: "wrong",
		}, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, w.Code)
		}
	}

	// 4th attempt should be locked out.
	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password: "secret",
	}, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
}

func TestRegisterRateLimitResetOnSuccess(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	srv.rateLimiter = newRateLimiter(3, 5*time.Minute)
	openPairing(t, srv)

	// Two failures — under threshold.
	for i := 0; i < 2; i++ {
		doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
			Password: "wrong",
		}, nil)
	}

	// Correct password succeeds and resets.
	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password:   "secret",
		DeviceName: "dev",
		Platform:   "android",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Enrolling closed the window; reopen it to keep probing the limiter.
	openPairing(t, srv)

	// Counter is reset: two more failures should not lock out.
	for i := 0; i < 2; i++ {
		w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
			Password: "wrong",
		}, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset attempt %d: status = %d, want 401", i, w.Code)
		}
	}
}

// --- request signing ------------------------------------------------------

// The property that replaced bearer tokens: capturing a request must not let
// you send it again. Without this, one sniffed poll on an open network is a
// permanent licence to approve shell commands.
func TestSignedRequestCannotBeReplayed(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	sign := registerAndLogin(t, srv)
	sign.nonce = randNonce(t) // pin it, so the second call is byte-identical

	if w := doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign); w.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, body %s", w.Code, w.Body.String())
	}
	if w := doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign); w.Code != http.StatusUnauthorized {
		t.Fatalf("replay: status = %d, want 401", w.Code)
	}
}

// A stale capture is refused even with a fresh nonce, which bounds how long a
// recording stays dangerous to the leeway window.
func TestSignedRequestOutsideLeewayIsRejected(t *testing.T) {
	srv := newTestServer(t, "plain:secret")

	for _, tc := range []struct {
		name string
		skew time.Duration
	}{
		{"stale", -(protocol.ClockLeewaySec + 5) * time.Second},
		{"future", (protocol.ClockLeewaySec + 5) * time.Second},
	} {
		sign := registerAndLogin(t, srv)
		sign.skew = tc.skew
		w := doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, w.Code)
		}
		// The client needs our clock to correct itself, or a drifting phone
		// fails forever with what looks like a credential error.
		if w.Header().Get(protocol.HeaderServerTime) == "" {
			t.Errorf("%s: no %s header on a skew rejection", tc.name, protocol.HeaderServerTime)
		}
	}
}

// Signing the method and path is what stops a captured poll from being
// rewritten into an approval.
func TestSignatureCannotBeMovedToAnotherRoute(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	sign := registerAndLogin(t, srv)

	nonce := randNonce(t)
	date := strconv.FormatInt(time.Now().Unix(), 10)
	// A legitimate signature over a harmless GET.
	sig := auth.Sign(sign.key, auth.Canonical("GET", "/v1/poll?cursor=0&wait=0", date, nonce, nil))

	body, _ := json.Marshal(protocol.ReplyRequest{EventID: "evt_1", Action: protocol.ActionOnce})
	req := httptest.NewRequest("POST", "/v1/reply", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.HeaderKeyID, sign.keyID)
	req.Header.Set(protocol.HeaderDate, date)
	req.Header.Set(protocol.HeaderNonce, nonce)
	req.Header.Set(protocol.HeaderSig, sig)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a poll signature approved a command", w.Code)
	}
}

// The body is signed, so an approval cannot be edited in flight.
func TestTamperedBodyIsRejected(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	sign := registerAndLogin(t, srv)

	nonce := randNonce(t)
	date := strconv.FormatInt(time.Now().Unix(), 10)
	signed, _ := json.Marshal(protocol.ReplyRequest{EventID: "evt_1", Action: protocol.ActionReject})
	sig := auth.Sign(sign.key, auth.Canonical("POST", "/v1/reply", date, nonce, signed))

	// Same signature, but the decision has been flipped to an approval.
	tampered, _ := json.Marshal(protocol.ReplyRequest{EventID: "evt_1", Action: protocol.ActionAlways})
	req := httptest.NewRequest("POST", "/v1/reply", bytes.NewReader(tampered))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.HeaderKeyID, sign.keyID)
	req.Header.Set(protocol.HeaderDate, date)
	req.Header.Set(protocol.HeaderNonce, nonce)
	req.Header.Set(protocol.HeaderSig, sig)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a reject was turned into an always", w.Code)
	}
}

// --- login and heartbeat --------------------------------------------------

func TestLoginIssuesAWrappedSession(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	dev, err := srv.devices.Register("Pixel", "android")
	if err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, srv, "POST", "/v1/login", protocol.LoginRequest{DeviceID: dev.ID},
		deviceSigner(t, srv, dev.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp protocol.LoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"key_id":      resp.KeyID,
		"wrap_salt":   resp.WrapSalt,
		"wrap_nonce":  resp.WrapNonce,
		"wrapped_key": resp.WrappedKey,
	} {
		if v == "" {
			t.Errorf("empty %s", name)
		}
	}
	if resp.ExpiresAt <= time.Now().Unix() {
		t.Error("session already expired")
	}
}

func TestLoginRequiresTheDeviceSecret(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	dev, _ := srv.devices.Register("Pixel", "android")

	// Right device ID, wrong key.
	w := doJSON(t, srv, "POST", "/v1/login", protocol.LoginRequest{DeviceID: dev.ID},
		&signer{keyID: dev.ID, key: []byte("not the secret")})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// A session key is only good for the device that proved it owns the secret.
func TestLoginBodyMustMatchTheSigningKey(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	mine, _ := srv.devices.Register("mine", "android")
	theirs, _ := srv.devices.Register("theirs", "android")

	w := doJSON(t, srv, "POST", "/v1/login", protocol.LoginRequest{DeviceID: theirs.ID},
		deviceSigner(t, srv, mine.ID))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestLoginUnknownDevice(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doJSON(t, srv, "POST", "/v1/login", protocol.LoginRequest{DeviceID: "dev_nope"},
		&signer{keyID: "dev_nope", key: []byte("whatever")})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHeartbeatConfirmsALiveSession(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	sign := registerAndLogin(t, srv)

	w := doJSON(t, srv, "POST", "/v1/heartbeat", nil, sign)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp protocol.HeartbeatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Error("ok = false")
	}
	if resp.ExpiresAt == 0 {
		t.Error("no session expiry reported")
	}
	if resp.ServerTime == 0 {
		t.Error("no server time reported; clients cannot measure skew")
	}
}

// The restart case the heartbeat exists for: sessions are memory-only, so a
// fresh registry rejects the old key and the companion knows to log in again.
func TestHeartbeatFailsAfterSessionsAreLost(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	sign := registerAndLogin(t, srv)

	if w := doJSON(t, srv, "POST", "/v1/heartbeat", nil, sign); w.Code != http.StatusOK {
		t.Fatalf("before restart: status = %d", w.Code)
	}

	// What a restart looks like from the client's side: same devices, no
	// sessions.
	srv.devices = auth.NewRegistry("plain:secret")

	if w := doJSON(t, srv, "POST", "/v1/heartbeat", nil, sign); w.Code != http.StatusUnauthorized {
		t.Fatalf("after restart: status = %d, want 401", w.Code)
	}
}

// Revocation has to bite immediately, not when the session key expires.
func TestRevokedDeviceLosesAccessAtOnce(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	dev, _ := srv.devices.Register("Pixel", "android")
	sess, _, _ := srv.devices.NewSession(dev.ID)
	sign := &signer{keyID: sess.KeyID, key: sess.Key}

	if w := doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign); w.Code != http.StatusOK {
		t.Fatalf("before revoke: status = %d", w.Code)
	}
	if err := srv.devices.Revoke(dev.ID); err != nil {
		t.Fatal(err)
	}
	if w := doJSON(t, srv, "GET", "/v1/poll?cursor=0&wait=0", nil, sign); w.Code != http.StatusUnauthorized {
		t.Fatalf("after revoke: status = %d, want 401", w.Code)
	}
}

// --- pairing window -------------------------------------------------------

// The gate that turns a leaked passphrase from a permanent invitation into
// something usable only during a window the user deliberately opened.
func TestRegisterRefusedWhenPairingIsShut(t *testing.T) {
	srv := newTestServer(t, "plain:secret")

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password: "secret", DeviceName: "Pixel", Platform: "android",
	}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 with the window shut", w.Code)
	}
	if srv.devices.Count() != 0 {
		t.Fatal("a device enrolled with pairing shut")
	}
}

func TestRegisterRefusedAfterTheWindowExpires(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	key, _ := auth.NewPairingKey()
	if err := srv.devices.OpenPairing(key, -time.Second); err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password: "secret", DeviceName: "Pixel", Platform: "android",
	}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// The sealed path: sign with the key from the QR, get the device secret back
// encrypted. Nothing usable crosses the wire.
func TestSealedRegistration(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	key := openPairing(t, srv)

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		DeviceName: "Pixel 8", Platform: "android",
	}, &signer{keyID: protocol.PairingKeyID, key: key})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}

	var resp protocol.RegisterResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DeviceSecret != "" {
		t.Fatal("sealed registration returned the secret in the clear")
	}
	if resp.WrapSalt == "" || resp.WrapNonce == "" || resp.WrapSecret == "" {
		t.Fatal("sealed registration returned no wrapped secret")
	}

	// And it is the secret prh actually stored.
	secret := openSealedSecret(t, key, &auth.WrappedSecret{
		WrapSalt: resp.WrapSalt, WrapNonce: resp.WrapNonce, WrapSecret: resp.WrapSecret,
	}, resp.DeviceID)
	stored, err := srv.devices.DeviceKey(resp.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if string(secret) != string(stored) {
		t.Fatal("unsealed secret does not match the one prh stored")
	}
}

func TestSealedRegistrationRejectsAWrongPairingKey(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	openPairing(t, srv)

	wrong, _ := auth.NewPairingKey()
	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		DeviceName: "Pixel", Platform: "android",
	}, &signer{keyID: protocol.PairingKeyID, key: wrong})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if srv.devices.Count() != 0 {
		t.Fatal("a device enrolled with the wrong pairing key")
	}
}

// Signing proves more than the passphrase does; accepting both would make one
// of them decorative.
func TestSealedRegistrationRejectsAPassword(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	key := openPairing(t, srv)

	w := doJSON(t, srv, "POST", "/v1/register", protocol.RegisterRequest{
		Password: "secret", DeviceName: "Pixel", Platform: "android",
	}, &signer{keyID: protocol.PairingKeyID, key: key})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// One window, one device — in both modes.
func TestEnrollingClosesTheWindow(t *testing.T) {
	for _, mode := range []string{"sealed", "passphrase"} {
		t.Run(mode, func(t *testing.T) {
			srv := newTestServer(t, "plain:secret")
			key := openPairing(t, srv)

			req := protocol.RegisterRequest{DeviceName: "first", Platform: "android"}
			var s *signer
			if mode == "sealed" {
				s = &signer{keyID: protocol.PairingKeyID, key: key}
			} else {
				req.Password = "secret"
			}

			if w := doJSON(t, srv, "POST", "/v1/register", req, s); w.Code != http.StatusOK {
				t.Fatalf("first enrolment: status = %d, body %s", w.Code, w.Body.String())
			}
			if srv.devices.PairingOpen() {
				t.Fatal("window stayed open after a device enrolled")
			}

			// A second phone needs a second deliberate act.
			req.DeviceName = "second"
			if w := doJSON(t, srv, "POST", "/v1/register", req, s); w.Code != http.StatusForbidden {
				t.Fatalf("second enrolment: status = %d, want 403", w.Code)
			}
			if n := srv.devices.Count(); n != 1 {
				t.Fatalf("devices = %d, want 1", n)
			}
		})
	}
}

// --- admin plane ----------------------------------------------------------

// Arming enrolment must not be reachable by the people enrolment defends
// against.
func TestPairingAdminRoutesAreNotOnTheNetwork(t *testing.T) {
	srv := newTestServer(t, "plain:secret")

	for _, method := range []string{"POST", "DELETE", "GET"} {
		req := httptest.NewRequest(method, "/admin/v1/pairing", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s /admin/v1/pairing on the TCP mux = %d, want 404", method, w.Code)
		}
	}
}

func TestAdminPairingOpenCloseStatus(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	key, _ := auth.NewPairingKey()

	w := doPluginJSON(t, srv, "POST", "/admin/v1/pairing", protocol.OpenPairingRequest{
		PairingKey: base64.RawURLEncoding.EncodeToString(key),
		TTLSec:     60,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("open: status = %d, body %s", w.Code, w.Body.String())
	}
	if !srv.devices.PairingOpen() {
		t.Fatal("window did not open")
	}

	w = doPluginJSON(t, srv, "GET", "/admin/v1/pairing", nil)
	var st protocol.PairingStatus
	json.Unmarshal(w.Body.Bytes(), &st)
	if !st.Open || st.ExpiresAt == 0 {
		t.Fatalf("status = %+v, want open with an expiry", st)
	}

	w = doPluginJSON(t, srv, "DELETE", "/admin/v1/pairing", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("close: status = %d", w.Code)
	}
	if srv.devices.PairingOpen() {
		t.Fatal("window did not close")
	}
}

// A weak pairing key would undo the whole scheme, so it is refused at the door.
func TestAdminPairingRejectsAShortKey(t *testing.T) {
	srv := newTestServer(t, "plain:secret")
	w := doPluginJSON(t, srv, "POST", "/admin/v1/pairing", protocol.OpenPairingRequest{
		PairingKey: base64.RawURLEncoding.EncodeToString(make([]byte, 8)),
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
