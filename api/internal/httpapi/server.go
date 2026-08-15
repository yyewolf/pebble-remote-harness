// Package httpapi serves the v1 API consumed by the Android companion.
//
// Routes and payloads are specified in docs/protocol.md.
package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yyewolf/pebble-remote-harness/api/internal/auth"
	"github.com/yyewolf/pebble-remote-harness/api/internal/config"
	"github.com/yyewolf/pebble-remote-harness/api/internal/hub"
	"github.com/yyewolf/pebble-remote-harness/api/internal/protocol"
)

// Server wires the HTTP surface onto the hub and device registry.
type Server struct {
	cfg     config.Config
	hub     *hub.Hub
	devices *auth.Registry
	log     *slog.Logger
	started time.Time

	// rateLimiter tracks failed registration attempts per source IP. The
	// pairing password is the only secret an attacker can guess at, so this
	// is the only endpoint that needs it.
	rateLimiter *rateLimiter

	// tlsPin is published on /v1/health so the extension can build a pairing
	// QR without parsing the certificate itself. Not a secret: every TLS
	// client is handed the certificate during the handshake.
	tlsPin string
}

// SetTLSPin records the pin for /v1/health.
func (s *Server) SetTLSPin(pin string) { s.tlsPin = pin }

func New(cfg config.Config, h *hub.Hub, devices *auth.Registry, log *slog.Logger) *Server {
	return &Server{
		cfg:         cfg,
		hub:         h,
		devices:     devices,
		log:         log,
		started:     time.Now(),
		rateLimiter: newRateLimiter(5, 5*time.Minute),
	}
}

// Handler returns the routed handler. Go 1.22+ method patterns keep this flat.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Only health and register are unauthenticated, and register is the only
	// one that accepts a secret. Everything else is signed.
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/register", s.handleRegister)
	mux.HandleFunc("POST /v1/login", s.handleLogin)
	mux.HandleFunc("POST /v1/heartbeat", s.signed(s.handleHeartbeat))
	mux.HandleFunc("GET /v1/poll", s.signed(s.handlePoll))
	mux.HandleFunc("POST /v1/reply", s.signed(s.handleReply))
	mux.HandleFunc("POST /v1/prompt", s.signed(s.handlePrompt))

	return mux
}

// PluginHandler is the Hop 0 surface, served **only** on the unix socket.
//
// It is a separate handler from Handler() on purpose: nothing here may ever
// be reachable from the network. Wiring these routes into the TCP mux would
// expose upstream registration and the decision queue to the LAN.
func (s *Server) PluginHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /plugin/v1/hello", s.handlePluginHello)
	mux.HandleFunc("POST /plugin/v1/events", s.handlePluginEvents)
	mux.HandleFunc("GET /plugin/v1/decisions", s.handlePluginDecisions)
	mux.HandleFunc("POST /plugin/v1/ack", s.handlePluginAck)

	// Admin plane. On the socket for the same reason the plugin routes are:
	// arming enrolment must not be reachable from the network. Anyone who can
	// open the socket is already this UID and has better options than pairing
	// a phone.
	mux.HandleFunc("POST /admin/v1/pairing", s.handleOpenPairing)
	mux.HandleFunc("DELETE /admin/v1/pairing", s.handleClosePairing)
	mux.HandleFunc("GET /admin/v1/pairing", s.handlePairingStatus)

	return mux
}

// handleOpenPairing arms an enrolment window with a key the caller supplies.
//
// The key comes from the caller rather than being minted here because whoever
// opens the window is also the one who has to display it: the extension puts
// it in the QR the phone scans. Generating it here would mean handing it back
// over the socket, which is the same secret in one more place for no gain.
func (s *Server) handleOpenPairing(w http.ResponseWriter, r *http.Request) {
	var req protocol.OpenPairingRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	key, err := base64.RawURLEncoding.DecodeString(req.PairingKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "pairing_key is not base64url")
		return
	}

	ttl := time.Duration(req.TTLSec) * time.Second
	if req.TTLSec <= 0 {
		ttl = protocol.DefaultPairingTTLSec * time.Second
	}

	if err := s.devices.OpenPairing(key, ttl); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("pairing window opened", "ttl_sec", int(ttl.Seconds()))

	writeJSON(w, http.StatusOK, protocol.PairingStatus{
		Open:      true,
		ExpiresAt: s.devices.PairingExpiry().Unix(),
	})
}

// handleClosePairing disarms the window early, e.g. when the user closes the
// pairing panel.
func (s *Server) handleClosePairing(w http.ResponseWriter, r *http.Request) {
	s.devices.ClosePairing()
	s.log.Info("pairing window closed")
	writeJSON(w, http.StatusOK, protocol.PairingStatus{Open: false})
}

func (s *Server) handlePairingStatus(w http.ResponseWriter, r *http.Request) {
	status := protocol.PairingStatus{Open: s.devices.PairingOpen()}
	if status.Open {
		status.ExpiresAt = s.devices.PairingExpiry().Unix()
	}
	writeJSON(w, http.StatusOK, status)
}

// handlePluginHello registers one kilo server, keyed by (parent_pid,
// directory).
//
//   - reject a protocol mismatch with 409 rather than guessing
//   - re-registering the same key replaces the entry; that is a window reload
//   - bind the upstream to this connection so that a dropped socket, which is
//     what a closing window produces, drops the upstream
//   - derive the project label from filepath.Base(Directory)
func (s *Server) handlePluginHello(w http.ResponseWriter, r *http.Request) {
	var req protocol.PluginHello
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Protocol != protocol.Version {
		writeErr(w, http.StatusConflict, "protocol mismatch: expected "+protocol.Version)
		return
	}

	project := filepath.Base(req.Directory)
	upstreamID := s.hub.RegisterUpstream(project, req.Directory, req.ParentPID)
	s.log.Info("plugin upstream registered", "upstream_id", upstreamID, "directory", req.Directory)

	writeJSON(w, http.StatusOK, protocol.PluginHelloResponse{
		UpstreamID: upstreamID,
		ServerName: s.cfg.ServerName,
		Protocol:   protocol.Version,
	})
}

// handlePluginEvents ingests the uplink batch and publishes envelopes.
//
// This must never block: the plugin is inside the user's agent and a slow
// response here is a stalled coding session. IngestPluginEvents is all
// in-memory.
func (s *Server) handlePluginEvents(w http.ResponseWriter, r *http.Request) {
	var req protocol.PluginEvents
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.UpstreamID == "" || !s.hub.UpstreamExists(req.UpstreamID) {
		writeErr(w, http.StatusBadRequest, "unknown upstream_id")
		return
	}

	project := s.hub.UpstreamProject(req.UpstreamID)
	s.hub.IngestPluginEvents(req.UpstreamID, project, req.Events)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(req.Events)})
}

// handlePluginDecisions is the downlink long-poll the plugin holds open.
//
// Only the owning upstream's decisions are returned — one window's plugin
// must never be able to apply another window's approval.
func (s *Server) handlePluginDecisions(w http.ResponseWriter, r *http.Request) {
	upstreamID := r.URL.Query().Get("upstream_id")
	if upstreamID == "" || !s.hub.UpstreamExists(upstreamID) {
		writeErr(w, http.StatusBadRequest, "unknown upstream_id")
		return
	}

	cursor, _ := strconv.ParseUint(r.URL.Query().Get("cursor"), 10, 64)
	waitSec := s.cfg.MaxPollWaitSec
	if q := r.URL.Query().Get("wait"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 && n < waitSec {
			waitSec = n
		}
	}

	resp, err := s.hub.PollDecisions(r.Context(), upstreamID, cursor, time.Duration(waitSec)*time.Second)
	if err != nil {
		switch {
		case errors.Is(err, hub.ErrAlreadyAnswered):
			writeErr(w, http.StatusGone, "upstream gone")
		case errors.Is(err, context.Canceled):
			writeJSON(w, http.StatusOK, protocol.DecisionsResponse{Cursor: 0, Decisions: nil})
		default:
			writeErr(w, http.StatusInternalServerError, "poll error")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePluginAck records what the plugin actually applied, which is what
// stops retries and what tells the watch the approval landed.
func (s *Server) handlePluginAck(w http.ResponseWriter, r *http.Request) {
	var req protocol.DecisionAck
	if !decodeJSON(w, r, &req) {
		return
	}
	s.hub.AckDecision(req.UpstreamID, req.ID, req.Status)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleHealth is unauthenticated and must never leak secrets.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.Health{
		Version:   protocol.Version,
		UptimeSec: int64(time.Since(s.started).Seconds()),
		Upstreams: s.hub.Upstreams(),
		Devices:   s.devices.Count(),
		Sessions:  s.devices.Sessions(),
		Pairing:   s.devices.PairingOpen(),
		TLSPin:    s.tlsPin,
		// Listen lets a second window detect that the running daemon was
		// started with settings other than its own, and warn rather than
		// restart a daemon the other windows are using.
		Listen: s.cfg.Listen,
		Paired: s.cfg.PasswordHash != "",
	})
}

// handleRegister enrols a device, and is the only endpoint that hands out a
// device secret.
//
// Gated on an open pairing window regardless of mode. Before that gate this
// endpoint answered at any hour to anyone holding the passphrase, which made
// the passphrase a permanent invitation — and a passphrase leaks by many
// routes that have nothing to do with the network: a screenshot, a clipboard,
// somebody reading your screen.
//
// Two modes, dispatched on whether the request is signed. See
// protocol.RegisterRequest for what each costs.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	pairingKey, err := s.devices.PairingKey()
	if err != nil {
		// 403 rather than 401: the credential may well be right, but enrolment
		// is shut. Telling them to open a window is more useful than implying
		// they typed it wrong.
		writeErr(w, http.StatusForbidden, "pairing is not open; start pairing from the editor")
		return
	}

	if r.Header.Get(protocol.HeaderSig) != "" {
		s.registerSealed(w, r, pairingKey)
		return
	}
	s.registerWithPassphrase(w, r)
}

// registerSealed enrols a device that proved it read the pairing key off the
// screen, and returns the device secret encrypted under that key.
//
// No rate limiting: the pairing key is 32 random bytes, so there is nothing to
// guess, and every forgery is refused by a constant-time HMAC comparison.
func (s *Server) registerSealed(w http.ResponseWriter, r *http.Request, pairingKey []byte) {
	if _, ok := s.verifySignature(w, r, func(keyID string) ([]byte, error) {
		if keyID != protocol.PairingKeyID {
			return nil, auth.ErrBadDevice
		}
		return pairingKey, nil
	}); !ok {
		return
	}

	var req protocol.RegisterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Password != "" {
		// Signing proves far more than the passphrase does, and accepting both
		// at once would mean one of them is decorative. Refuse the ambiguity.
		writeErr(w, http.StatusBadRequest, "a signed registration must not carry a password")
		return
	}

	dev, wrapped, err := s.devices.RegisterSealed(req.DeviceName, req.Platform, pairingKey)
	if err != nil {
		s.log.Error("sealed registration failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "registration failed")
		return
	}
	s.log.Info("device enrolled (sealed)", "device_id", dev.ID, "name", req.DeviceName)

	writeJSON(w, http.StatusOK, protocol.RegisterResponse{
		DeviceID:   dev.ID,
		ServerName: s.cfg.ServerName,
		WrapSalt:   wrapped.WrapSalt,
		WrapNonce:  wrapped.WrapNonce,
		WrapSecret: wrapped.WrapSecret,
	})
}

// registerWithPassphrase is the fallback for when a code cannot be scanned.
//
// It sends the passphrase up and the device secret back down in the clear, so
// it is only as safe as the transport. Kept because typing 32 random bytes is
// not a thing anyone will do, and rate-limited by source IP because the
// passphrase is the one user-chosen secret worth guessing at.
func (s *Server) registerWithPassphrase(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)

	if s.rateLimiter.isLocked(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}

	var req protocol.RegisterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Password == "" {
		writeErr(w, http.StatusBadRequest, "missing password")
		return
	}

	if err := s.devices.VerifyPassword(req.Password); err != nil {
		if errors.Is(err, auth.ErrBadPassword) {
			s.rateLimiter.recordFailure(ip)
			writeErr(w, http.StatusUnauthorized, "bad password")
			return
		}
		writeErr(w, http.StatusInternalServerError, "auth error")
		return
	}

	// Success: clear the IP's failure history so a legitimate user who
	// mistyped a few times is not penalised after a successful pairing.
	s.rateLimiter.reset(ip)

	dev, err := s.devices.Register(req.DeviceName, req.Platform)
	if err != nil {
		// Register only fails if the secret could not be generated or could
		// not be persisted. Both mean the pairing would not survive, so do not
		// hand back a secret that is about to be forgotten.
		s.log.Error("registration failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "registration failed")
		return
	}
	s.log.Info("device enrolled (passphrase, secret sent in the clear)",
		"device_id", dev.ID, "name", req.DeviceName)

	// One enrolment per window, same as the sealed path.
	s.devices.ClosePairing()

	writeJSON(w, http.StatusOK, protocol.RegisterResponse{
		DeviceID:     dev.ID,
		DeviceSecret: dev.Secret,
		ServerName:   s.cfg.ServerName,
	})
}

// handleLogin issues a session key to a device that proves possession of its
// device secret by signing this request with it.
//
// The passphrase is deliberately not involved. It crossed the wire once, at
// registration, and never needs to again — which is what lets the companion
// recover from a prh restart on its own, with nothing for the user to retype.
//
// No rate limit here: unlike the passphrase, a device secret is 32 random
// bytes, so there is nothing to guess at and every forgery attempt is rejected
// by a constant-time HMAC comparison.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	hdr, ok := s.verifySignature(w, r, s.devices.DeviceKey)
	if !ok {
		return
	}

	var req protocol.LoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// The signing key already identifies the device; requiring the body to
	// agree keeps a signed login from being replayed against a different
	// device's registration.
	if req.DeviceID != hdr.KeyID {
		writeErr(w, http.StatusBadRequest, "device_id does not match signing key")
		return
	}

	_, wrapped, err := s.devices.NewSession(hdr.KeyID)
	if err != nil {
		s.log.Error("session creation failed", "device_id", hdr.KeyID, "err", err)
		writeErr(w, http.StatusInternalServerError, "login failed")
		return
	}
	s.log.Info("session issued", "device_id", hdr.KeyID, "key_id", wrapped.KeyID)

	writeJSON(w, http.StatusOK, protocol.LoginResponse{
		KeyID:      wrapped.KeyID,
		WrapSalt:   wrapped.WrapSalt,
		WrapNonce:  wrapped.WrapNonce,
		WrappedKey: wrapped.WrappedKey,
		ExpiresAt:  wrapped.Expires.Unix(),
		ServerName: s.cfg.ServerName,
	})
}

// handleHeartbeat confirms a session is still usable.
//
// Its value is entirely in the failure case: a 401 here is the companion's
// signal that prh restarted and dropped its sessions, so it should log in
// again. Without it that discovery only happens when a prompt is already
// waiting, which is the worst moment to find out.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, dev *auth.Device) {
	writeJSON(w, http.StatusOK, protocol.HeartbeatResponse{
		OK: true,
		// ServerTime doubles as a skew check while things are working, so a
		// drifting clock can be corrected before it starts costing 401s.
		ServerTime: time.Now().Unix(),
		ExpiresAt:  s.sessionExpiry(r),
	})
}

// sessionExpiry reports when the signing session ends, or 0 if it cannot be
// resolved — the heartbeat has already succeeded by this point, so a missing
// expiry is informational rather than an error.
func (s *Server) sessionExpiry(r *http.Request) int64 {
	sess, err := s.devices.Session(r.Header.Get(protocol.HeaderKeyID))
	if err != nil {
		return 0
	}
	return sess.Expires.Unix()
}

// handlePoll is the long-poll. It blocks up to wait seconds for events after
// cursor, returning immediately if any are queued.
//
// hub.ErrCursorTooOld maps to 410 so the companion knows to reset rather than
// retry forever.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request, dev *auth.Device) {
	cursor, _ := strconv.ParseUint(r.URL.Query().Get("cursor"), 10, 64)
	waitSec := s.cfg.MaxPollWaitSec
	if q := r.URL.Query().Get("wait"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 && n < waitSec {
			waitSec = n
		}
	}

	resp, err := s.hub.Poll(r.Context(), cursor, time.Duration(waitSec)*time.Second)
	if err != nil {
		switch {
		case errors.Is(err, hub.ErrCursorTooOld):
			writeErr(w, http.StatusGone, "cursor older than retained history")
		case errors.Is(err, context.Canceled):
			// Client went away or request deadline; return empty.
			writeJSON(w, http.StatusOK, protocol.PollResponse{Cursor: s.hub.Cursor(), Events: []protocol.Envelope{}})
		default:
			writeErr(w, http.StatusInternalServerError, "poll error")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleReply answers a perm or ques envelope.
//
// An already-answered or expired envelope maps to 409 — the companion
// retries on network failure and must not double-approve.
func (s *Server) handleReply(w http.ResponseWriter, r *http.Request, dev *auth.Device) {
	var req protocol.ReplyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.EventID == "" {
		writeErr(w, http.StatusBadRequest, "missing event_id")
		return
	}
	if req.Action == "" {
		writeErr(w, http.StatusBadRequest, "missing action")
		return
	}

	if err := s.hub.Reply(req); err != nil {
		if errors.Is(err, hub.ErrAlreadyAnswered) {
			writeErr(w, http.StatusConflict, "already answered or expired")
			return
		}
		writeErr(w, http.StatusInternalServerError, "reply error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePrompt forwards dictated text as new work.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request, dev *auth.Device) {
	var req protocol.PromptRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	writeErr(w, http.StatusNotImplemented, "prompt not implemented")
}

// authedFunc is a handler that has already resolved a device.
type authedFunc func(http.ResponseWriter, *http.Request, *auth.Device)

// signed dispatches only requests carrying a valid session signature.
//
// This replaced bearer tokens. A bearer token is replayed verbatim on every
// request, so on an unencrypted LAN one captured poll hands an attacker
// permanent authority to approve shell commands. A signature proves possession
// of a key that never crosses the wire, and proves it for that request alone.
func (s *Server) signed(next authedFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var session *auth.Session
		hdr, ok := s.verifySignature(w, r, func(keyID string) ([]byte, error) {
			sess, err := s.devices.Session(keyID)
			if err != nil {
				return nil, err
			}
			session = sess
			return sess.Key, nil
		})
		if !ok {
			return
		}
		_ = hdr

		// The session outlived its device only if the device was revoked
		// mid-session, which Revoke already handles; this is the belt to that
		// braces. A revoked phone must stop approving immediately.
		dev, err := s.devices.Device(session.DeviceID)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "device revoked")
			return
		}
		next(w, r, dev)
	}
}

// verifySignature performs the checks common to every signed route and, on
// success, leaves r.Body readable by the handler.
//
// Order matters. Cheap, non-secret checks run first so that malformed traffic
// costs nothing; the nonce is only recorded after the signature verifies, or
// an observer could burn a nonce belonging to a request they cannot forge and
// deny it to the real client.
func (s *Server) verifySignature(
	w http.ResponseWriter,
	r *http.Request,
	keyFor func(keyID string) ([]byte, error),
) (auth.SigHeaders, bool) {
	hdr, err := auth.ParseSigHeaders(r.Header.Get)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "malformed signature headers")
		return hdr, false
	}

	now := time.Now()
	if err := auth.CheckDate(hdr.Date, now); err != nil {
		// Answer with our clock so a client with a drifting one can measure
		// the offset and retry, instead of failing forever with a 401 that
		// looks like a credential problem. The time is not a secret.
		w.Header().Set(protocol.HeaderServerTime, strconv.FormatInt(now.Unix(), 10))
		writeErr(w, http.StatusUnauthorized, "request date outside leeway")
		return hdr, false
	}

	if s.devices.NonceUsed(hdr.KeyID, hdr.Nonce, now) {
		writeErr(w, http.StatusUnauthorized, "nonce already used")
		return hdr, false
	}

	key, err := keyFor(hdr.KeyID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unknown or expired key")
		return hdr, false
	}

	body, ok := readBody(w, r)
	if !ok {
		return hdr, false
	}

	if err := auth.Verify(key, auth.Canonical(r.Method, r.URL.RequestURI(), hdr.Date, hdr.Nonce, body), hdr.Sig); err != nil {
		writeErr(w, http.StatusUnauthorized, "bad signature")
		return hdr, false
	}

	s.devices.RememberNonce(hdr.KeyID, hdr.Nonce, now)
	return hdr, true
}

// readBody buffers the body so it can be hashed, then puts it back for the
// handler to decode. The cap matches decodeJSON's.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	defer r.Body.Close()
	buf, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable request body")
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	return buf, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON reports whether decoding succeeded, writing 400 if it did not.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request body")
		return false
	}
	return true
}

// clientIP extracts the remote address, stripping any port. Under VSCode
// Remote this is the remote host's address, which is correct — the phone
// reaches that host, not the VSCode client.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

// rateLimiter tracks failed attempts per IP within a sliding window. After
// maxAttempts failures, the IP is locked until the oldest failure expires.
type rateLimiter struct {
	mu          sync.Mutex
	maxAttempts int
	window      time.Duration
	failures    map[string][]time.Time
}

func newRateLimiter(maxAttempts int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		maxAttempts: maxAttempts,
		window:      window,
		failures:    make(map[string][]time.Time),
	}
}

func (rl *rateLimiter) isLocked(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.recentLocked(ip)) >= rl.maxAttempts
}

func (rl *rateLimiter) recordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.failures[ip] = append(rl.failures[ip], time.Now())
}

func (rl *rateLimiter) reset(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.failures, ip)
}

// recentLocked returns failures within the window. Must be called with mu held.
func (rl *rateLimiter) recentLocked(ip string) []time.Time {
	cutoff := time.Now().Add(-rl.window)
	fails := rl.failures[ip]
	out := fails[:0]
	for _, f := range fails {
		if f.After(cutoff) {
			out = append(out, f)
		}
	}
	rl.failures[ip] = out
	return out
}
