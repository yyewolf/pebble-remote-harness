package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yyewolf/pebble-remote-harness/api/internal/protocol"
)

var (
	ErrBadSignature = errors.New("auth: bad signature")
	ErrClockSkew    = errors.New("auth: request date outside leeway")
	ErrReplay       = errors.New("auth: nonce already used")
	ErrMalformedSig = errors.New("auth: malformed signature headers")
)

// Canonical builds the string that gets signed.
//
// Every field here is one an attacker would otherwise be free to change on a
// captured request:
//
//	scheme  — a future scheme cannot be made to look like this one
//	method  — a signed GET cannot be turned into a DELETE
//	path    — a signed /v1/poll cannot be retargeted at /v1/reply, which is
//	          the difference between reading a prompt and approving a command
//	date    — bounds how long a capture stays useful
//	nonce   — makes each request single-use inside that bound
//	body    — the approval itself cannot be edited
//
// Dropping any line widens the scheme from "this exact request" to "anything
// resembling it". The body is hashed rather than included so that signing does
// not require buffering it twice.
//
// path must be the full request URI, query string included: the cursor and
// wait parameters are part of what was authorised.
func Canonical(method, path, date, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		protocol.SigScheme,
		method,
		path,
		date,
		nonce,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

// Sign returns the base64url HMAC-SHA256 of the canonical string.
func Sign(key []byte, canonical string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// SigHeaders is the parsed signature material from a request.
type SigHeaders struct {
	KeyID string
	Date  string
	Nonce string
	Sig   string
}

// ParseSigHeaders pulls the four mandatory headers, rejecting anything
// incomplete. A partially-signed request is not a degraded request, it is an
// unauthenticated one.
func ParseSigHeaders(get func(string) string) (SigHeaders, error) {
	h := SigHeaders{
		KeyID: strings.TrimSpace(get(protocol.HeaderKeyID)),
		Date:  strings.TrimSpace(get(protocol.HeaderDate)),
		Nonce: strings.TrimSpace(get(protocol.HeaderNonce)),
		Sig:   strings.TrimSpace(get(protocol.HeaderSig)),
	}
	if h.KeyID == "" || h.Date == "" || h.Nonce == "" || h.Sig == "" {
		return h, ErrMalformedSig
	}
	// A short nonce makes accidental collisions between honest clients
	// plausible, at which point the replay cache starts rejecting real
	// requests and the obvious "fix" is to remove it.
	raw, err := base64.RawURLEncoding.DecodeString(h.Nonce)
	if err != nil || len(raw) < protocol.NonceMinLen {
		return h, ErrMalformedSig
	}
	return h, nil
}

// CheckDate enforces the leeway in both directions.
//
// Future-dated requests are rejected as firmly as stale ones: allowing them
// would let an attacker pre-record a request now and hold it, which is exactly
// what the date is meant to prevent.
func CheckDate(date string, now time.Time) error {
	secs, err := strconv.ParseInt(date, 10, 64)
	if err != nil {
		return ErrMalformedSig
	}
	delta := now.Unix() - secs
	if delta < 0 {
		delta = -delta
	}
	if delta > protocol.ClockLeewaySec {
		return ErrClockSkew
	}
	return nil
}

// Verify recomputes the signature and compares it in constant time.
func Verify(key []byte, canonical, sig string) error {
	want := Sign(key, canonical)
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1 {
		return nil
	}
	return ErrBadSignature
}

// nonceCache remembers recently-accepted nonces so that a request captured
// inside the leeway window still cannot be replayed.
//
// Entries only need to outlive the window a request could legitimately arrive
// in, which is twice the leeway: a request dated leeway-seconds in the past
// may still be presented leeway-seconds from now.
type nonceCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	seen map[string]time.Time
}

func newNonceCache() *nonceCache {
	return &nonceCache{
		ttl: 2 * protocol.ClockLeewaySec * time.Second,
		// Bounded so that a flood of signed-but-rejected traffic cannot grow
		// the map without limit. Only *accepted* nonces are stored, so
		// reaching this cap takes a genuinely busy daemon.
		max:  8192,
		seen: make(map[string]time.Time),
	}
}

// used reports whether the nonce has already been accepted for this key.
//
// Scoped per key so that two devices choosing the same random nonce cannot
// lock each other out.
func (c *nonceCache) used(keyID, nonce string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.seen[keyID+"\x00"+nonce]
	return ok && exp.After(now)
}

// remember records an accepted nonce. Call it only after the signature has
// verified: recording on failure would let an attacker burn the nonce of a
// request they merely observed, denying it to the real client.
func (c *nonceCache) remember(keyID, nonce string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.seen) >= c.max {
		c.sweepLocked(now)
		// Still full of live entries: drop everything rather than grow. This
		// reopens the replay window for the leeway period under sustained
		// load, which is the lesser evil against unbounded memory in a daemon
		// that must not fall over.
		if len(c.seen) >= c.max {
			c.seen = make(map[string]time.Time)
		}
	}
	c.seen[keyID+"\x00"+nonce] = now.Add(c.ttl)
}

func (c *nonceCache) sweepLocked(now time.Time) {
	for k, exp := range c.seen {
		if !exp.After(now) {
			delete(c.seen, k)
		}
	}
}
