// Package kilo talks to a headless `kilo serve` instance.
//
// This is the only package that knows Kilo's (undocumented, unstable) API
// shapes. See docs/kilo-integration.md; re-verify after every Kilo upgrade.
package kilo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ErrNotImplemented marks the scaffold stubs.
var ErrNotImplemented = errors.New("kilo: not implemented")

// Upstream event type strings we care about. Everything else is dropped.
//
// Verified against a live prompt on Kilo 7.4.22: the **v1** names are what
// actually fire. The v2 names are declared in the OpenAPI schema and are the
// tempting choice, but nothing emits them; translating from v2 yields empty
// envelopes. See docs/kilo-integration.md.
const (
	TypePermissionAsked   = "permission.asked"
	TypePermissionReplied = "permission.replied"
	TypeQuestionAsked     = "question.asked"
	TypeSessionIdle       = "session.idle"
	TypeSessionError      = "session.error"

	// Declared but not observed. Kept so a future Kilo that starts emitting
	// them is recognised rather than silently ignored.
	TypePermissionAskedV2 = "permission.v2.asked"
	TypeQuestionAskedV2   = "question.v2.asked"
)

// Event is one decoded SSE frame. Properties stays raw so the hub can decode
// per-type without this package growing a struct per event.
type Event struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// PermissionAsked is the payload of permission.asked, captured verbatim from
// a live bash prompt.
type PermissionAsked struct {
	ID        string `json:"id"`        // "per..."
	SessionID string `json:"sessionID"` // "ses..."

	// Permission is the action, e.g. "bash". Named "permission", not "action".
	Permission string `json:"permission"`

	// Patterns is the resource, e.g. ["ls -la"].
	Patterns []string `json:"patterns"`

	// Metadata carries "command" and a human "description", the latter often
	// reading better than the raw command on a 200px screen.
	Metadata map[string]any `json:"metadata"`

	// Always is what choosing "always" would grant — a *pattern*, broader
	// than the command. Approving "ls -la" with always grants "ls *". The
	// watch must display this, or the user consents to more than they read.
	Always []string `json:"always"`

	Tool struct {
		MessageID string `json:"messageID"`
		CallID    string `json:"callID"`
	} `json:"tool"`
}

// PermissionReplied fires when a prompt is answered anywhere, including in
// the VSCode UI. It is how a prompt already settled at the desk gets
// retracted from the watch instead of being asked twice.
type PermissionReplied struct {
	SessionID string `json:"sessionID"`
	RequestID string `json:"requestID"`
	Reply     string `json:"reply"` // once | always | reject
}

// QuestionAsked is the payload of question.v2.asked.
//
// TODO: transcribe QuestionV2Info from the live /doc spec. The shape of an
// individual question (prompt text, choice list, whether free text is allowed)
// determines how the watch renders it.
type QuestionAsked struct {
	ID        string          `json:"id"` // "que..."
	SessionID string          `json:"sessionID"`
	Questions json.RawMessage `json:"questions"`
}

// SessionRef is the payload of session.idle.
type SessionRef struct {
	SessionID string `json:"sessionID"`
}

// Client is one kilo serve instance.
type Client struct {
	BaseURL  string // http://127.0.0.1:4096
	Password string // KILO_SERVER_PASSWORD; sent as HTTP Basic, any username
	HTTP     *http.Client
}

// New returns a Client with a sane HTTP client. The timeout is deliberately
// zero-ish for streaming; Events sets its own deadlines.
func New(baseURL, password string) *Client {
	return &Client{
		BaseURL:  baseURL,
		Password: password,
		HTTP:     &http.Client{Timeout: 0},
	}
}

// Events consumes GET /event and emits decoded frames until ctx is done.
//
// TODO: implement. Notes for whoever does:
//   - The response is text/event-stream; parse `data:` lines with bufio.
//   - Auth is HTTP Basic with any username and Client.Password.
//   - Reconnect with backoff on EOF; kilo restarts when VSCode reloads.
//   - Filter to the Type* constants above before sending, or the channel
//     drowns in per-token text deltas.
func (c *Client) Events(ctx context.Context) (<-chan Event, error) {
	return nil, ErrNotImplemented
}

// PermissionReply is the body of POST /permission/{requestID}/reply.
type PermissionReply struct {
	Reply       string `json:"reply"` // once | always | reject — required
	Message     string `json:"message,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
}

// AlwaysRules is the body of POST /permission/{requestID}/always-rules.
type AlwaysRules struct {
	ApprovedAlways []string `json:"approvedAlways,omitempty"`
	DeniedAlways   []string `json:"deniedAlways,omitempty"`
}

// ReplyPermission answers a permission request.
//
// Endpoint: POST /permission/{requestID}/reply, body PermissionReply.
// Both accept optional `directory` and `workspace` query parameters.
//
// TODO: implement. "always" persists the broader Always pattern from the
// request, so only send it when the user was actually shown that pattern.
func (c *Client) ReplyPermission(ctx context.Context, requestID string, reply PermissionReply) error {
	return ErrNotImplemented
}

// ReplyQuestion answers a question request.
//
// Endpoint: POST /api/session/{sessionID}/question/{requestID}/reply
// Rejection: POST /api/session/{sessionID}/question/{requestID}/reject
func (c *Client) ReplyQuestion(ctx context.Context, sessionID, requestID string, choice int, text string) error {
	return ErrNotImplemented
}

// Prompt sends new work to a session. Endpoint: POST /session/{id}/prompt_async
func (c *Client) Prompt(ctx context.Context, sessionID, text string) error {
	return ErrNotImplemented
}

// Abort stops the current turn. Endpoint: POST /session/{id}/abort
func (c *Client) Abort(ctx context.Context, sessionID string) error {
	return ErrNotImplemented
}

// Ping checks reachability and credentials against GET /global/health.
func (c *Client) Ping(ctx context.Context) error {
	return ErrNotImplemented
}

// Instance is one discovered kilo serve process.
type Instance struct {
	PID       int
	ParentPID int    // KILO_PARENT_PID: the VSCode extension host that spawned it
	BaseURL   string // http://127.0.0.1:<port from the listening socket>
	Password  string // KILO_SERVER_PASSWORD, per-instance
}

// Discover enumerates running kilo serve processes owned by this user.
//
// Kilo Code spawns one server per VSCode window with `--port 0` and a fresh
// password, so both rotate on every reload. See docs/kilo-integration.md.
//
// TODO: implement.
//   - scan /proc/*/cmdline for "bin/kilo serve"
//   - read /proc/<pid>/environ for KILO_SERVER_PASSWORD and KILO_PARENT_PID
//   - resolve the port from the process's listening socket, since --port 0
//     means it is absent from the command line
//
// Prefer the extension-side handoff: the extension matches ParentPID against
// its own process.pid and registers the upstream, which is unambiguous and
// keeps this Linux-only code off the daemon's critical path. This function is
// the fallback for a standalone prh.
func Discover(ctx context.Context, timeout time.Duration) ([]Instance, error) {
	return nil, ErrNotImplemented
}
