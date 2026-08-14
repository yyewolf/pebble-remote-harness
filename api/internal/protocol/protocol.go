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
	EventPerm EventType = "perm" // permission.v2.asked  -> needs a reply
	EventQues EventType = "ques" // question.v2.asked    -> needs a reply
	EventIdle EventType = "idle" // session.idle         -> notify only
	EventErr  EventType = "err"  // session.error        -> notify only
	EventNote EventType = "note" // internal status      -> notify only
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

// RegisterRequest trades the shared password for a per-device token.
type RegisterRequest struct {
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
	Platform   string `json:"platform"`
}

type RegisterResponse struct {
	DeviceID   string `json:"device_id"`
	Token      string `json:"token"`
	ServerName string `json:"server_name"`
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
type PluginEvent struct {
	Kind      string   `json:"kind"` // permission | question | idle | error
	RequestID string   `json:"request_id"`
	SessionID string   `json:"session_id"`
	Action    string   `json:"action"`
	Resources []string `json:"resources,omitempty"`
	CanSave   bool     `json:"can_save,omitempty"`
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
