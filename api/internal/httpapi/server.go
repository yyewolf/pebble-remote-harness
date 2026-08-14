// Package httpapi serves the v1 API consumed by the Android companion.
//
// Routes and payloads are specified in docs/protocol.md.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
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

// handleHealth is unauthenticated and must never leak secrets.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.Health{
		Version:   protocol.Version,
		UptimeSec: int64(time.Since(s.started).Seconds()),
		Upstreams: s.hub.Upstreams(),
		Devices:   s.devices.Count(),
	})
}

// handleRegister trades the pairing password for a device token.
//
// TODO: implement, and rate-limit by source IP — this is the only endpoint
// where an attacker can guess. Repeated failures should lock the IP out.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req protocol.RegisterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	writeErr(w, http.StatusNotImplemented, "register not implemented")
}

// handlePoll is the long-poll. It blocks up to wait seconds for events after
// cursor, returning immediately if any are queued.
//
// TODO: implement. Parse cursor and wait, clamp wait to
// cfg.MaxPollWaitSec, call hub.Poll, and map hub.ErrCursorTooOld to 410 so
// the companion knows to reset rather than retry forever.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request, dev *auth.Device) {
	writeErr(w, http.StatusNotImplemented, "poll not implemented")
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
// TODO: implement once auth.Registry.Authenticate exists. Until then every
// authenticated route is unreachable, which is the safe failure direction.
func (s *Server) authed(next authedFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotImplemented, "authentication not implemented")
	}
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
