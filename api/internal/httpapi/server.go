// Package httpapi serves the v1 API consumed by the Android companion.
//
// Routes and payloads are specified in docs/protocol.md.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
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
}

func New(cfg config.Config, h *hub.Hub, devices *auth.Registry, log *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		hub:     h,
		devices: devices,
		log:     log,
		started: time.Now(),
	}
}

// Handler returns the routed handler. Go 1.22+ method patterns keep this flat.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/register", s.handleRegister)
	mux.HandleFunc("GET /v1/poll", s.authed(s.handlePoll))
	mux.HandleFunc("POST /v1/reply", s.authed(s.handleReply))
	mux.HandleFunc("POST /v1/prompt", s.authed(s.handlePrompt))

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

	return mux
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
// TODO: implement. Only hand an upstream the decisions addressed to it —
// one window's plugin must never be able to apply another window's approval.
func (s *Server) handlePluginDecisions(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, "plugin decisions not implemented")
}

// handlePluginAck records what the plugin actually applied, which is what
// stops retries and what tells the watch the approval landed.
func (s *Server) handlePluginAck(w http.ResponseWriter, r *http.Request) {
	var req protocol.DecisionAck
	if !decodeJSON(w, r, &req) {
		return
	}
	writeErr(w, http.StatusNotImplemented, "plugin ack not implemented")
}

// handleHealth is unauthenticated and must never leak secrets.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.Health{
		Version:   protocol.Version,
		UptimeSec: int64(time.Since(s.started).Seconds()),
		Upstreams: s.hub.Upstreams(),
		Devices:   s.devices.Count(),
		// Listen lets a second window detect that the running daemon was
		// started with settings other than its own, and warn rather than
		// restart a daemon the other windows are using.
		Listen: s.cfg.Listen,
		Paired: s.cfg.PasswordHash != "",
	})
}

// handleRegister trades the pairing password for a device token.
//
// TODO (M3): rate-limit by source IP — this is the only endpoint where an
// attacker can guess. Repeated failures should lock the IP out.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
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
			writeErr(w, http.StatusUnauthorized, "bad password")
			return
		}
		if errors.Is(err, auth.ErrRateLimited) {
			writeErr(w, http.StatusTooManyRequests, "too many attempts")
			return
		}
		writeErr(w, http.StatusInternalServerError, "auth error")
		return
	}

	deviceID, token, err := s.devices.Register(req.DeviceName, req.Platform)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "registration failed")
		return
	}
	s.log.Info("device registered", "device_id", deviceID, "name", req.DeviceName)

	writeJSON(w, http.StatusOK, protocol.RegisterResponse{
		DeviceID:   deviceID,
		Token:      token,
		ServerName: s.cfg.ServerName,
	})
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
// TODO: implement. Map an already-answered or expired envelope to 409 —
// the companion retries on network failure and must not double-approve.
func (s *Server) handleReply(w http.ResponseWriter, r *http.Request, dev *auth.Device) {
	var req protocol.ReplyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	writeErr(w, http.StatusNotImplemented, "reply not implemented")
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

// authed resolves the bearer token before dispatching.
//
// Tokens are sent as `Authorization: Bearer prh_...`. A missing or unknown
// token gets 401 — the safe failure direction.
func (s *Server) authed(next authedFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		dev, err := s.devices.Authenticate(token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		}
		next(w, r, dev)
	}
}

// bearerToken extracts the token from an Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
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
