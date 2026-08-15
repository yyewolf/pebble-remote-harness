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

	// EventMsg is a conversation message part. It carries what the agent (or
	// user) is saying, so the phone can show context a 200px screen cannot.
	// The watch ignores it: this data never crosses Bluetooth.
	EventMsg EventType = "msg"

	// EventGone retracts a prompt answered elsewhere, e.g. in the VSCode UI.
	// Driven by Kilo's permission.replied. Without it the watch keeps asking
	// a question already settled at the desk.
	EventGone EventType = "gone"
)

// Wire returns the uint8 the watchapp expects for this type. The watch ignores
// types it does not recognise, so msg/5-note fall into the default.
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
	case EventMsg:
		return 7
	default:
		return 5
	}
}

// NeedsReply reports whether the watch should present answer affordances.
// msg envelopes do not: they are context for the phone, not a prompt for the
// wrist.
func (t EventType) NeedsReply() bool {
	return t == EventPerm || t == EventQues
}

// CrossesBluetooth reports whether the companion forwards this envelope to
// the watch. msg does not — conversation is phone-only and stays off the
// slow, snooppable Bluetooth hop.
func (t EventType) CrossesBluetooth() bool {
	switch t {
	case EventPerm, EventQues, EventIdle, EventErr, EventNote, EventGone:
		return true
	}
	return false
}

// Field limits. prh truncates so that truncation is consistent everywhere;
// the companion must not re-truncate.
const (
	MaxProject = 24
	MaxTitle   = 32
	MaxBody    = 256
	MaxChoice  = 24
	MaxChoices = 6

	// MaxMsgText bounds a conversation part. It is far larger than MaxBody
	// because it renders on a phone rather than a 200px watch screen, but it
	// is bounded all the same: a tool part can carry a whole file's diff, and
	// without a cap one `cat` of a large file would sit in memory per session
	// and be re-sent on every conversation poll.
	MaxMsgText = 4096

	// MaxPromptText bounds text the phone sends into a session. The phone is
	// a keyboard, not a file upload.
	MaxPromptText = 4096
)

// SessionStatus values reported in SessionSummary.Status.
const (
	StatusIdle    = "idle"
	StatusBusy    = "busy"
	StatusRetry   = "retry"
	StatusUnknown = "unknown"
)

// Envelope is one watch-sized event. Kept small: every field eventually
// crosses Bluetooth.
//
// The Msg* fields are populated only for type == EventMsg and never cross
// Bluetooth — the companion consumes them for its conversation view and the
// watch never sees them. Keeping them on the same struct lets one poll stream
// serve both surfaces without a second endpoint.
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

	// -- msg-only fields --------------------------------------------------
	//
	// Populated when Type == EventMsg. The companion renders these into a
	// conversation; the watch never receives them (CrossesBluetooth is
	// false for EventMsg).

	// MsgRole is "user", "assistant", or "tool" — who said it.
	MsgRole string `json:"msg_role,omitempty"`
	// MsgPartID identifies the part within the message, for replace/update.
	MsgPartID string `json:"msg_part_id,omitempty"`
	// MsgText is the accumulated text of the part. A part may be updated many
	// times as the agent streams; the latest text replaces the prior.
	MsgText string `json:"msg_text,omitempty"`
	// MsgKind labels the part for the UI: "text", "reasoning", "tool",
	// "step-start", "file", "patch". Lets the phone render tool calls and
	// reasoning distinctly from prose.
	MsgKind string `json:"msg_kind,omitempty"`
	// MsgTime is the part's created/updated time in unix milliseconds.
	MsgTime int64 `json:"msg_time,omitempty"`
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

// PairingKeyID is the literal X-Prh-Key value on a sealed registration. The
// pairing key is anonymous — there is no device yet to name — so it signs
// under a fixed identifier rather than an issued one.
const PairingKeyID = "pair"

// DefaultPairingTTLSec is how long a pairing window stays armed. Long enough
// to scan a code and switch apps, short enough that an unattended window is
// not a standing invitation.
const DefaultPairingTTLSec = 120

// RegisterRequest enrols a device. Two modes, and the difference matters:
//
//   - **sealed** (preferred): the request is signed with the pairing key from
//     the QR, Password is empty, and the device secret comes back encrypted.
//     Nothing usable crosses the wire, so capturing the exchange gains
//     nothing.
//   - **passphrase** (fallback, for when a code cannot be scanned): Password
//     carries the pairing passphrase and the secret comes back in the clear.
//     Anyone who can read the traffic gets both. Only safe on a transport you
//     trust.
//
// Both modes require an open pairing window.
type RegisterRequest struct {
	// Password is empty in sealed mode.
	Password   string `json:"password,omitempty"`
	DeviceName string `json:"device_name"`
	Platform   string `json:"platform"`
}

// RegisterResponse hands over the device secret, once and only once. A device
// that loses it must enrol again through a new pairing window.
//
// In sealed mode DeviceSecret is empty and the Wrap* fields carry it instead:
//
//	wrapKey      = HKDF-SHA256(pairingKey, salt=WrapSalt, info="prh-pairing-wrap-v1")
//	deviceSecret = AES-256-GCM-Open(wrapKey, WrapNonce, WrapSecret, aad=DeviceID)
//
// In passphrase mode DeviceSecret is populated and the Wrap* fields are empty.
type RegisterResponse struct {
	DeviceID   string `json:"device_id"`
	ServerName string `json:"server_name"`

	// Passphrase mode only: base64url, 32 bytes, in the clear.
	DeviceSecret string `json:"device_secret,omitempty"`

	// Sealed mode only.
	WrapSalt   string `json:"wrap_salt,omitempty"`   // base64url, 16 bytes
	WrapNonce  string `json:"wrap_nonce,omitempty"`  // base64url, 12 bytes
	WrapSecret string `json:"wrap_secret,omitempty"` // base64url, sealed 32 bytes
}

// -- admin plane, served only on the unix socket -----------------------------

// OpenPairingRequest arms a pairing window. It travels over the unix socket
// and never the network: an endpoint that opens enrolment must not be
// reachable by the people enrolment is defending against.
type OpenPairingRequest struct {
	PairingKey string `json:"pairing_key"` // base64url, >= 32 bytes
	TTLSec     int    `json:"ttl_sec,omitempty"`
}

type PairingStatus struct {
	Open      bool  `json:"open"`
	ExpiresAt int64 `json:"expires_at,omitempty"` // unix seconds
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

// -- sessions + conversation (phone UI) --------------------------------------
//
// These endpoints let the companion show what is happening across every
// window and reply to a session from the phone. They are signed like the rest
// of Hop 1; the conversation data never crosses Bluetooth.

// SessionSummary is one entry in GET /v1/sessions. Built from events, not by
// calling Kilo, so prh needs no credentials.
type SessionSummary struct {
	ID         string `json:"id"`
	Project    string `json:"project"`               // which window owns it
	Title      string `json:"title"`                 // Kilo's session title, truncated
	Dir        string `json:"dir"`                   // working directory basename
	Status     string `json:"status"`                // idle | busy | retry | unknown
	Updated    int64  `json:"updated"`               // unix ms, last activity
	HasPrompt  bool   `json:"has_prompt"`            // a perm/ques envelope is pending
	PromptID   string `json:"prompt_id,omitempty"`   // the pending envelope id
	PromptType string `json:"prompt_type,omitempty"` // perm | ques
}

// SessionsResponse answers GET /v1/sessions.
type SessionsResponse struct {
	Sessions []SessionSummary `json:"sessions"`
}

// ConversationResponse answers GET /v1/sessions/{id}/conversation. Messages
// are returned newest-last; the cursor is the seq of the last envelope the
// caller has seen, so a follow-up long-poll fetches only newer ones.
type ConversationResponse struct {
	Cursor  uint64     `json:"cursor"`
	Events  []Envelope `json:"events"`
	HasMore bool       `json:"has_more,omitempty"`
}

// SessionPromptRequest is the body of POST /v1/sessions/{id}/prompt. It
// becomes a kind:"prompt" decision routed to the owning plugin, which calls
// Kilo's prompt_async. prh never holds Kilo credentials, so it cannot send
// the text itself — same inversion as permission replies.
type SessionPromptRequest struct {
	Text string `json:"text"`
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
	// Pairing reports whether enrolment is currently possible. The fact is
	// safe to publish; it is what the extension's status bar shows.
	Pairing bool `json:"pairing"`

	// TLSPin is the base64url SHA-256 of the DER SubjectPublicKeyInfo that
	// clients pin. Publishing it costs nothing — every TLS client receives the
	// certificate itself during the handshake — and the extension needs it to
	// build the pairing QR.
	TLSPin string `json:"tls_pin,omitempty"`
	Listen string `json:"listen"`
	Paired bool   `json:"paired"`
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
//
// The Kind space grew beyond permission/question/idle/error to carry what the
// agent is saying and the session lifecycle, so the phone can show context
// and a session list.
type PluginEvent struct {
	// Kind is permission | question | idle | error | replied |
	// message | session.created | session.updated | session.deleted |
	// session.status.
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

	// -- message + session lifecycle -------------------------------------
	//
	// Populated for kind == "message". The plugin forwards the accumulated
	// part text, not per-token deltas: volume stays bounded and the phone's
	// view is always current without a streaming protocol.

	// MsgRole: "user", "assistant", or "tool".
	MsgRole string `json:"msg_role,omitempty"`
	// MsgPartID identifies the part within its message.
	MsgPartID string `json:"msg_part_id,omitempty"`
	// MsgText is the part's accumulated text.
	MsgText string `json:"msg_text,omitempty"`
	// MsgKind is the part type: "text", "reasoning", "tool", "step-start",
	// "file", "patch".
	MsgKind string `json:"msg_kind,omitempty"`
	// MsgTimeMs is the part's timestamp in unix milliseconds.
	MsgTimeMs int64 `json:"msg_time_ms,omitempty"`

	// -- session lifecycle -----------------------------------------------
	//
	// Populated for kind == "session.*". The plugin forwards what Kilo emits
	// in session.created/updated/deleted (a Session struct) and session.status
	// (idle/busy/retry).

	// SessionTitle is Kilo's session title, e.g. "Fix the login bug".
	SessionTitle string `json:"session_title,omitempty"`
	// SessionDir is the session's working directory, full path.
	SessionDir string `json:"session_dir,omitempty"`
	// SessionStatus is "idle" | "busy" | "retry" for session.status.
	SessionStatus string `json:"session_status,omitempty"`
	// SessionUpdatedMs is the session's last-updated time in unix ms.
	SessionUpdatedMs int64 `json:"session_updated_ms,omitempty"`
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
