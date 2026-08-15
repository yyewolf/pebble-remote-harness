package auth

import (
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/yyewolf/pebble-remote-harness/api/internal/protocol"
)

func testNonce(b byte) string {
	raw := make([]byte, protocol.NonceMinLen)
	for i := range raw {
		raw[i] = b
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// The whole point of signing method and path is that a captured signature
// cannot be moved to another route. Polling and approving are different
// privileges.
func TestCanonicalBindsMethodPathAndBody(t *testing.T) {
	key := []byte("k")
	base := Canonical("GET", "/v1/poll?cursor=0", "1700000000", "n", []byte(`{}`))

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"method", Canonical("POST", "/v1/poll?cursor=0", "1700000000", "n", []byte(`{}`))},
		{"path", Canonical("GET", "/v1/reply", "1700000000", "n", []byte(`{}`))},
		{"query", Canonical("GET", "/v1/poll?cursor=9", "1700000000", "n", []byte(`{}`))},
		{"date", Canonical("GET", "/v1/poll?cursor=0", "1700000001", "n", []byte(`{}`))},
		{"nonce", Canonical("GET", "/v1/poll?cursor=0", "1700000000", "m", []byte(`{}`))},
		{"body", Canonical("GET", "/v1/poll?cursor=0", "1700000000", "n", []byte(`{"a":1}`))},
	} {
		if Sign(key, tc.got) == Sign(key, base) {
			t.Errorf("changing %s did not change the signature", tc.name)
		}
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	c := Canonical("POST", "/v1/reply", "1700000000", "n", []byte(`{"action":"once"}`))
	sig := Sign([]byte("right"), c)

	if err := Verify([]byte("right"), c, sig); err != nil {
		t.Fatalf("correct key rejected: %v", err)
	}
	if err := Verify([]byte("wrong"), c, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key accepted: %v", err)
	}
}

func TestCheckDateRejectsBothDirections(t *testing.T) {
	now := time.Unix(1700000000, 0)

	for _, tc := range []struct {
		name    string
		offset  int64
		wantErr bool
	}{
		{"now", 0, false},
		{"just inside past", -protocol.ClockLeewaySec, false},
		{"just inside future", protocol.ClockLeewaySec, false},
		{"stale", -protocol.ClockLeewaySec - 1, true},
		// A future-dated request is a request recorded now to be used later.
		{"future", protocol.ClockLeewaySec + 1, true},
	} {
		err := CheckDate(strconv.FormatInt(now.Unix()+tc.offset, 10), now)
		if tc.wantErr && !errors.Is(err, ErrClockSkew) {
			t.Errorf("%s: expected skew rejection, got %v", tc.name, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: expected acceptance, got %v", tc.name, err)
		}
	}
}

func TestParseSigHeadersRequiresAllFour(t *testing.T) {
	full := map[string]string{
		protocol.HeaderKeyID: "k1",
		protocol.HeaderDate:  "1700000000",
		protocol.HeaderNonce: testNonce(1),
		protocol.HeaderSig:   "sig",
	}

	if _, err := ParseSigHeaders(func(k string) string { return full[k] }); err != nil {
		t.Fatalf("complete headers rejected: %v", err)
	}

	for missing := range full {
		if _, err := ParseSigHeaders(func(k string) string {
			if k == missing {
				return ""
			}
			return full[k]
		}); !errors.Is(err, ErrMalformedSig) {
			t.Errorf("missing %s was accepted", missing)
		}
	}
}

// A short nonce would collide between honest clients and make the replay
// cache look broken.
func TestParseSigHeadersRejectsShortNonce(t *testing.T) {
	short := base64.RawURLEncoding.EncodeToString(make([]byte, protocol.NonceMinLen-1))
	_, err := ParseSigHeaders(func(k string) string {
		switch k {
		case protocol.HeaderKeyID:
			return "k1"
		case protocol.HeaderDate:
			return "1700000000"
		case protocol.HeaderNonce:
			return short
		default:
			return "sig"
		}
	})
	if !errors.Is(err, ErrMalformedSig) {
		t.Fatalf("short nonce accepted: %v", err)
	}
}

func TestNonceCacheBlocksReplayAndExpires(t *testing.T) {
	c := newNonceCache()
	now := time.Unix(1700000000, 0)
	n := testNonce(2)

	if c.used("k1", n, now) {
		t.Fatal("fresh nonce reported as used")
	}
	c.remember("k1", n, now)
	if !c.used("k1", n, now) {
		t.Fatal("replay was not blocked")
	}

	// Another device's identical nonce must not be blocked by ours.
	if c.used("k2", n, now) {
		t.Fatal("nonce cache is not scoped per key")
	}

	// Past the window the request would fail CheckDate anyway, so the entry
	// is free to expire.
	if c.used("k1", n, now.Add(c.ttl+time.Second)) {
		t.Fatal("nonce never expires")
	}
}

func TestNonceCacheStaysBounded(t *testing.T) {
	c := newNonceCache()
	now := time.Unix(1700000000, 0)

	for i := 0; i < c.max*2; i++ {
		c.remember("k1", strconv.Itoa(i), now)
	}
	if len(c.seen) > c.max {
		t.Fatalf("cache grew past its cap: %d > %d", len(c.seen), c.max)
	}
}
