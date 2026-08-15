// Package protocol defines the wire types shared between prh, the Android
// companion, and (indirectly) the watchapp.
//
// These types are the contract described in docs/protocol.md. Changing one
// here means changing the companion's models and the watchapp's message keys.
package protocol

// Version prefixes every HTTP route. Breaking changes bump it.
const Version = "v1"

// EventType discriminates envelopes. The numeric values are what cross
// AppMessage as MESSAGE_KEY_EVENT_TYPE, so they are part of the contract.
type EventType string

const (
	EventPerm EventType = "perm" // permission.asked     -> needs a reply
	EventQues EventType = "ques" // question.asked       -> needs a reply
	EventIdle EventType = "idle" // session.idle         -> notify only
	EventErr  EventType = "err"  // session.error        -> notify only
	EventNote EventType = "note" // internal status      -> notify only

	// EventGone retracts a prompt answered elsewhere, e.g. in the VSCode UI.
	// Driven by Kilo's permission.replied. Without it the watch keeps asking
	// a question already settled at the desk.
	EventGone EventType = "gone"
)

// Wire returns the uint8 the watchapp expects for this type.
func (t EventType) Wire() uint8 {
	switch t {
	case EventPerm:
		return 1
	case EventQues:
		return 2
	case EventIdle:
		return 3
	case EventErr:
		return 4
	case EventGone:
		return 6
	default:
		return 5
	}
}

// NeedsReply reports whether the watch should present answer affordances.
func (t EventType) NeedsReply() bool {
	return t == EventPerm || t == EventQues
}

// Field limits. prh truncates so that truncation is consistent everywhere;
// the companion must not re-truncate.
const (
	MaxProject = 24
	MaxTitle   = 32
	MaxBody    = 256
	MaxChoice  = 24
	MaxChoices = 6
)

// Envelope is one watch-sized event. Kept small: every field eventually
// crosses Bluetooth.
type Envelope struct {
	ID   string    `json:"id"`
	Seq  uint64    `json:"seq"`
	Type EventType `json:"type"`

	// Project names the window that is asking. One prh serves every VSCode
	// window, so without this the watch cannot tell which repository wants to
	// run the command it is about to approve.
	Project string `json:"project"`

	// Session is opaque; it routes dictation back to the right session and
	// means nothing to a human.
	Session string `json:"session"`

	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Choices []string `json:"choices,omitempty"`
	Expires int64    `json:"expires,omitempty"` // unix seconds
}

// ReplyAction is how the user answered.
type ReplyAction string

const (
	ActionOnce   ReplyAction = "once"
	ActionAlways ReplyAction = "always"
	ActionReject ReplyAction = "reject"
	ActionChoice ReplyAction = "choice"
	ActionText   ReplyAction = "text"
)

// -- Hop 1 authentication ---------------------------------------------------
//
// Three credentials with deliberately different lifetimes. See docs/protocol.md
// for the reasoning; the short version is that nothing reusable may cross the
// wire twice, because Hop 1 is plaintext HTTP on a LAN.
//
//  1. the pairing passphrase — crosses the wire exactly once per device, at
//     registration, and is argon2id-hashed at rest
//  2. the device secret — issued at registration, then never transmitted
//     again; it only ever signs
//  3. the session key — wrapped under the device secret at login, then only
//     ever signs
//
// Every request after registration is authenticated by an HMAC over its own
// method, path, date, nonce and body. A captured request is worthless: it
// cannot be replayed (nonce), cannot be held (date), and cannot be retargeted
// at another route (method and path are signed).

// SigScheme is the first line of every canonical string. It exists so that a
// future scheme change cannot be made to look like this one.
const SigScheme = "PRH1"

// Signature headers. All four are mandatory on a signed request.
const (
	// HeaderKeyID names the key that produced the signature: a device ID on
	// POST /v1/login, a session key ID everywhere else.
	HeaderKeyID = "X-Prh-Key"
	HeaderDate  = "X-Prh-Date"  // unix seconds
	HeaderNonce = "X-Prh-Nonce" // base64url, >= 16 bytes, single-use
	HeaderSig   = "X-Prh-Sig"   // base64url HMAC-SHA256

	// HeaderServerTime is returned on a skew rejection so the client can
	// measure its offset and retry instead of failing forever. The current
	// time is not a secret, so answering with it costs nothing.
	HeaderServerTime = "X-Prh-Time"
)

// ClockLeewaySec bounds how far a request's date may be from the server's.
//
// Tight on purpose: the leeway is exactly the window in which a captured
// request could be replayed if the nonce cache were ever bypassed. Ten seconds
// is comfortable for two NTP-synced machines on the same LAN, and the
// HeaderServerTime hint covers the case where one of them is not.
const ClockLeewaySec = 10

// NonceMinLen is the minimum accepted nonce length in decoded bytes. Below
// this, collisions between honest clients become plausible and the replay
// cache stops being a reliable defence.
const NonceMinLen = 16

// SessionTTLSec is how long a session key stays valid. Sessions are held in
// memory only, so a prh restart invalidates every one of them — which is the
// case the heartbeat exists to detect and repair.
const SessionTTLSec = 12 * 3600

// RegisterRequest trades the shared passphrase for a per-device secret. This
// is the only request that carries the passphrase, and the only one that is
// not signed — there is nothing to sign with yet.
type RegisterRequest struct {
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
	Platform   string `json:"platform"`
}

// RegisterResponse hands over the device secret. It is returned exactly once
// and cannot be recovered afterwards; a device that loses it must re-pair with
// the passphrase.
type RegisterResponse struct {
	DeviceID     string `json:"device_id"`
	DeviceSecret string `json:"device_secret"` // base64url, 32 bytes
	ServerName   string `json:"server_name"`
}

// LoginRequest asks for a session key. The body names the device; possession
// of the device secret is proven by the signature over this request, so the
// secret itself stays off the wire.
type LoginRequest struct {
	DeviceID string `json:"device_id"`
}

// LoginResponse carries the session key sealed under a key derived from the
// device secret, so that the session key is never transmitted in the clear
// either.
//
//	wrapKey = HKDF-SHA256(deviceSecret, salt=WrapSalt, info="prh-session-wrap-v1")
//	sessionKey = AES-256-GCM-Open(wrapKey, WrapNonce, WrappedKey, aad=KeyID)
type LoginResponse struct {
	KeyID      string `json:"key_id"`
	WrapSalt   string `json:"wrap_salt"`   // base64url, 16 bytes
	WrapNonce  string `json:"wrap_nonce"`  // base64url, 12 bytes
	WrappedKey string `json:"wrapped_key"` // base64url, sealed 32-byte key
	ExpiresAt  int64  `json:"expires_at"`  // unix seconds
	ServerName string `json:"server_name"`
}

// HeartbeatResponse confirms a session is still live. The client uses a 401
// here as its signal to log in again, which is how it recovers from a prh
// restart without the user retyping anything.
type HeartbeatResponse struct {
	OK         bool  `json:"ok"`
	ExpiresAt  int64 `json:"expires_at"`
	ServerTime int64 `json:"server_time"`
}

// PollResponse answers GET /v1/poll. An empty Events with an unchanged cursor
// means the long-poll timed out; poll again with the same cursor.
type PollResponse struct {
	Cursor uint64     `json:"cursor"`
	Events []Envelope `json:"events"`
}

// ReplyRequest answers a perm or ques envelope.
type ReplyRequest struct {
	EventID string      `json:"event_id"`
	Action  ReplyAction `json:"action"`
	Choice  int         `json:"choice,omitempty"`
	Text    string      `json:"text,omitempty"`
}

// PromptRequest starts new work from dictation.
type PromptRequest struct {
	Session string `json:"session"`
	Text    string `json:"text"`
}

// Health is the unauthenticated liveness payload. Must never carry secrets.
//
// Listen and Bind let a second VSCode window notice that the running daemon
// was started with different settings than its own, so it can warn instead of
// restarting a daemon other windows are using.
type Health struct {
	Version   string `json:"version"`
	UptimeSec int64  `json:"uptime_sec"`
	Upstreams int    `json:"upstreams"`
	Devices   int    `json:"devices"`
	// Sessions is how many devices currently hold a valid signing key. It
	// drops to zero on restart, which is the condition heartbeats repair.
	Sessions int `json:"sessions"`
	Listen    string `json:"listen"`
	Paired    bool   `json:"paired"`
}

// -- Hop 0: the plugin channel, served on a unix socket ---------------------

// PluginHello registers one kilo server. Identity is (ParentPID, Directory);
// the connection's lifetime is the upstream's lifetime.
type PluginHello struct {
	Protocol      string `json:"protocol"`
	PluginVersion string `json:"plugin_version"`
	KiloVersion   string `json:"kilo_version"`
	Directory     string `json:"directory"`
	ProjectID     string `json:"project_id"`
	ParentPID     int    `json:"parent_pid"`
}

type PluginHelloResponse struct {
	UpstreamID string `json:"upstream_id"`
	ServerName string `json:"server_name"`
	Protocol   string `json:"protocol"`
}

// PluginEvent is one upstream occurrence, before translation to an Envelope.
//
// Field names follow Kilo's live permission.asked payload rather than its v2
// schema, because v1 is what actually fires. See docs/kilo-integration.md.
type PluginEvent struct {
	// Kind is permission | question | idle | error | replied.
	Kind      string `json:"kind"`
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`

	// Action is Kilo's "permission" field, e.g. "bash".
	Action string `json:"action,omitempty"`

	// Resources is Kilo's "patterns" field, e.g. ["ls -la"].
	Resources []string `json:"resources,omitempty"`

	// Description is metadata.description, a human sentence that often reads
	// better than the raw command on a 200px screen.
	Description string `json:"description,omitempty"`

	// Always is what choosing "always" would grant — a pattern, broader than
	// the command. "ls -la" approved with always grants "ls *". Show it, or
	// the user consents to more than they read.
	Always []string `json:"always,omitempty"`
}

type PluginEvents struct {
	UpstreamID string        `json:"upstream_id"`
	Events     []PluginEvent `json:"events"`
}

// Decision is an answer travelling back to the plugin, which applies it.
//
// Nonce exists so that a retry cannot become a second approval, and Expires
// resolves to "leave pending" rather than "approve".
type Decision struct {
	ID        string      `json:"id"`
	RequestID string      `json:"request_id"`
	SessionID string      `json:"session_id"`
	Kind      string      `json:"kind"` // permission | question | prompt | abort
	Action    ReplyAction `json:"action"`
	Choice    int         `json:"choice,omitempty"`
	Text      string      `json:"text,omitempty"`
	Nonce     string      `json:"nonce"`
	Expires   int64       `json:"expires"`
}

type DecisionsResponse struct {
	Cursor    uint64     `json:"cursor"`
	Decisions []Decision `json:"decisions"`
}

// DecisionAck reports what the plugin actually did.
type DecisionAck struct {
	UpstreamID string `json:"upstream_id"`
	ID         string `json:"id"`
	Status     string `json:"status"` // applied | rejected | expired | unknown_request
}
