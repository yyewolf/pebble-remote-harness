# Wire protocol

Three hops, three trust levels.

```
kilo plugin  --unix socket-->  prh  --JSON over HTTP-->  companion  --AppMessage-->  watchapp
   in-process                  no Kilo creds            LAN, token           Bluetooth
```

Version: `v1`. Breaking changes bump the path prefix.

## Hop 0 — plugin ↔ `prh` (JSON over a unix socket)

Not on the network. Served on `AF_UNIX` at
`${XDG_RUNTIME_DIR:-~/.local/state}/prh/plugin.sock`, in a `0700` directory,
with a peer-UID check on accept. Rationale and threat model in `plugin.md`.

No credentials cross this channel in either direction. `prh` never learns a
Kilo password and cannot call Kilo's API.

### `POST /plugin/v1/hello`

```jsonc
// request
{ "protocol": "v1",
  "plugin_version": "0.1.0",
  "kilo_version": "7.4.22",
  "directory": "/home/you/workspace/infra",  // becomes the watch's label
  "project_id": "69d07a1c…",
  "parent_pid": 2025346 }                    // extension host, or null outside VSCode

// 200
{ "upstream_id": "up_3f9a", "server_name": "workstation", "protocol": "v1" }
// 409 on protocol mismatch — fail loudly, never guess
```

`(parent_pid, directory)` identifies an upstream. Re-registering replaces it,
which is exactly what a VSCode reload produces.

An upstream's lifetime **is** the connection's lifetime: when the window
closes, the kilo server exits, this socket drops, and `prh` forgets the
upstream. There is no deregistration call to miss and nothing left stale if
VSCode is killed rather than closed.

`directory`'s basename becomes the envelope's `project` label. One `prh`
serves every window, so the watch must be able to tell which repository is
asking before you approve anything.

### `POST /plugin/v1/events`

Uplink, batched, fire-and-forget. The plugin must not block a turn waiting for
this; a bounded queue that drops on overflow is correct.

```jsonc
{ "upstream_id": "up_3f9a",
  "events": [
    { "kind": "permission",        // permission | question | idle | error | replied |
                                   // message | session.created | session.updated |
                                   // session.deleted | session.status
      "request_id": "per_01H…",    // the ID a decision must match
      "session_id": "ses_01H…",
      "action": "bash",            // Kilo's "permission" field
      "resources": ["rm -rf build/"],   // Kilo's "patterns" field
      "description": "Remove the build directory",  // metadata.description
      "always": ["rm *"] }         // what "always" would grant — see below
  ] }
```

Field names follow Kilo's **v1** `permission.asked`, which is what actually
fires; the v2 schema's `action`/`resources`/`save` never appear on the wire.

`always` is broader than the command. Approving `rm -rf build/` with "always"
grants `rm *`. `prh` must render the choice as `Always: rm *` rather than a
bare "Always", or the user consents to something they were never shown.

`kind: "replied"` carries only `request_id` and `session_id`. It fires when a
prompt is answered anywhere — including the VSCode UI — and becomes a `gone`
envelope so the watch stops asking a settled question.

`kind: "message"` carries `msg_role`, `msg_part_id`, `msg_text`, `msg_kind`,
`msg_time_ms` — the accumulated text of one part, not per-token deltas. The
phone renders these into its conversation view; the watch never receives them.

`kind: "session.*"` carries `session_title`, `session_dir`, `session_status`
(from Kilo's `Session` struct and `session.status`), so `prh` can build a
session registry from events without ever calling Kilo's API.

`prh` truncates to the limits below and turns these into envelopes.

### `GET /plugin/v1/decisions?upstream_id=…&cursor=…&wait=…`

Downlink long-poll. The plugin holds this open and applies what arrives.

```jsonc
{ "cursor": 12,
  "decisions": [
    { "id": "dec_7",
      "request_id": "per_01H…",   // must be pending and plugin-forwarded
      "session_id": "ses_01H…",
      "kind": "permission",
      "action": "once",           // once | always | reject | choice | text
      "choice": 0,
      "text": "",
      "nonce": "9f2c…",           // dedupe; a retry must not double-approve
      "expires": 1765400000 }
  ] }
```

Rules the plugin enforces, not `prh` — see `plugin.md`: unknown or
already-answered `request_id` is dropped, repeated `nonce` is dropped, expiry
means *leave pending* rather than approve, and `allow-everything` is not a
representable action.

### `POST /plugin/v1/ack`

Reports what was applied, so `prh` can stop retrying and tell the watch.

```jsonc
{ "upstream_id": "up_3f9a", "id": "dec_7", "status": "applied" }
// status: applied | rejected | expired | unknown_request
```

## Hop 1 — `prh` ↔ companion (JSON over HTTPS)

Default bind `0.0.0.0:8477`, **TLS with a self-signed certificate**. There is
no CA and no hostname check: the companion pins the server's public key, having
learned it from the pairing QR.

### TLS

prh generates an **ECDSA P-256** certificate on first start, at `cert_path` /
`key_path` beside the config, and serves TLS 1.2+.

P-256 rather than Ed25519 despite the smaller certificate: the companion
targets `minSdk 26`, and Android's TLS stack cannot verify Ed25519
certificates that far back — the handshake simply fails on the phone. The size
argument is void anyway, because what travels in the QR is a 32-byte hash,
identical for either algorithm.

The pin is:

```
base64url( SHA-256( DER SubjectPublicKeyInfo ) )
```

which is exactly `PublicKey.getEncoded()` on Android and
`cert.RawSubjectPublicKeyInfo` in Go — the two sides never have to parse each
other's certificate format.

**Pin the key, not the certificate.** Two things follow, and both are why:

- prh can reissue the certificate — new SANs, longer validity — and every
  paired phone keeps working, as long as the key file survives. Deleting
  `key.pem` is what forces a re-pair; deleting `cert.pem` is harmless.
- the daemon's address can change with DHCP and nothing cares, because nobody
  is matching a hostname against it.

The client side is a `TrustManager` that ignores the chain and compares the
pin, plus a permissive `HostnameVerifier`. Disabling hostname verification is
normally how people accidentally accept any server; it is safe here **only**
because the pin is the identity check. The two are a pair — removing one means
restoring the other in the same commit.

### Authentication

Every request except `/v1/health` is **signed**, `/v1/register` included when
a pairing code was scanned. With the scanned path, no credential ever crosses
the wire in a usable form at all.

Three credentials, each with a different lifetime:

| Credential | Lives | Crosses the wire | Purpose |
|---|---|---|---|
| pairing key | memory, for the life of one window | never — read off the screen by camera | authorise one enrolment |
| pairing passphrase | argon2id hash in `config.json` | legacy mode; no shipped client offers it | authorise one enrolment |
| device secret | `devices.json` (0600) and the phone's `EncryptedSharedPreferences` | never, if enrolled with a pairing key | prove identity at login |
| session key | memory on both sides, 12h TTL | never in the clear — wrapped at login | sign every request |

Why not bearer tokens: a bearer token is replayed verbatim on every request,
so on an unencrypted LAN a single sniffed poll hands an attacker permanent
authority to approve shell commands. A signature proves possession of a key
that never crosses the wire, and proves it for one specific request.

#### Enrolment: the pairing window

Enrolment is the one exchange whose compromise hands over everything — the
passphrase going up enrols any device forever, the device secret coming down
impersonates this one. So it is gated twice: in **time**, by a window the user
opens deliberately, and in **content**, by sealing the response.

Arm a window from the editor (**Pebble Harness: Pair**) or the CLI:

```console
$ prh pair -ttl 120
Pairing open for 120 seconds. In the companion app, scan or enter:

  prh://192.168.1.10:8477?k=NkmADdvpCgeF3W4HUq_7ZQgvbUYmBlGDvj9EQKI24k4&f=_ieyxg5iIuaUj7lrMjDTO4I1zBprGh9HSzwUOlR7UkY

The window closes as soon as one device enrols.
```

`k` is a freshly generated 32-byte **pairing key**, not the passphrase, and `f`
is prh's **TLS pin**. Both reach the phone by screen-to-camera — a channel
nothing on the network can touch — which is what makes the code a root of
trust rather than a convenience. `k` is single-use: the window shuts the moment
one device enrols, and on expiry regardless. `f` is long-lived and stored.

The presence of `f` selects the scheme: the phone talks **https** and pins the
certificate, and refuses to connect if the handshake fails — falling back to
`http` is precisely the downgrade an attacker would induce, so it is not even
an option. Every current code carries `f`, and cleartext is disabled in the
companion's manifest.

Without an open window, every registration is refused with `403`.

#### `POST /v1/register`

Two modes. The difference is not stylistic.

**Sealed** — signed with the pairing key (`X-Prh-Key: pair`), no password:

```jsonc
// request
{ "device_name": "Pixel 8", "platform": "android" }

// 200 — the secret is encrypted; nothing usable crosses the wire
{ "device_id": "dev_7f3a", "server_name": "workstation",
  "wrap_salt": "<16B>", "wrap_nonce": "<12B>", "wrap_secret": "<sealed 32B>" }
```

Unwrap, exactly as for a session key but with its own info string:

```
wrapKey      = HKDF-SHA256(pairingKey, salt=wrap_salt, info="prh-pairing-wrap-v1")
deviceSecret = AES-256-GCM-Open(wrapKey, wrap_nonce, wrap_secret, aad=device_id)
```

An attacker who captured this entire exchange holds a signature and a sealed
blob, and can do nothing with either.

**Passphrase** — the legacy mode, still accepted by prh but offered by no
shipped client:

```jsonc
// request
{ "password": "...", "device_name": "Pixel 8", "platform": "android" }

// 200 — the secret travels in the clear
{ "device_id": "dev_7f3a", "device_secret": "<base64url, 32 bytes>",
  "server_name": "workstation" }
```

The companion no longer implements this: without a scanned code there is no TLS
pin, and without a pin there is no way to distinguish prh from anything else
answering on that address. The endpoint remains because it is rate-limited and
cheap, not because anything uses it.

Responses: `403` window shut or expired, `401` bad pairing key or bad password,
`400` a signed request that also carries a password, `429` repeated passphrase
failures from one IP. Rate limiting applies to the passphrase mode only — a
pairing key is 32 random bytes, so there is nothing to guess.

#### `POST /v1/login`

Trades the device secret for a session key. **Signed with the device secret**
(`X-Prh-Key: <device_id>`), so the secret itself stays off the wire.

```jsonc
// request
{ "device_id": "dev_7f3a" }

// 200
{ "key_id": "key_9c1d", "wrap_salt": "<16B>", "wrap_nonce": "<12B>",
  "wrapped_key": "<sealed 32B>", "expires_at": 1765432100,
  "server_name": "workstation" }
// 401 unknown device or bad signature — re-pair by scanning a fresh code
// 400 device_id does not match the signing key
```

Unwrap:

```
wrapKey    = HKDF-SHA256(deviceSecret, salt=wrap_salt, info="prh-session-wrap-v1")
sessionKey = AES-256-GCM-Open(wrapKey, wrap_nonce, wrapped_key, aad=key_id)
```

The key ID is authenticated as AEAD additional data, so a swapped key ID fails
to open rather than binding a good key to the wrong session. Logging in again
replaces the device's previous session rather than adding one.

#### Signing a request

```
X-Prh-Key:   <key_id>            (device_id on /v1/login)
X-Prh-Date:  <unix seconds>
X-Prh-Nonce: <base64url, >= 16 random bytes>
X-Prh-Sig:   <base64url HMAC-SHA256 of the canonical string>
```

The canonical string, joined with `\n`:

```
PRH1
<METHOD>
<path and query, exactly as sent>
<X-Prh-Date>
<X-Prh-Nonce>
<lowercase hex SHA-256 of the body, empty body included>
```

Each line closes an attack that the others do not:

- **method and path** — a captured `GET /v1/poll` cannot be rewritten into a
  `POST /v1/reply`. That is the difference between reading a prompt and
  approving `rm -rf`.
- **query** — `?cursor=` is part of what was authorised.
- **date** — ±10s leeway, enforced in *both* directions. Future-dating would
  otherwise let an attacker record a request now and hold it.
- **nonce** — single-use per key, cached for 2× the leeway, so a capture
  cannot be replayed even inside the window the date allows.
- **body hash** — a `reject` cannot be edited into an `always` in flight.

A skew rejection carries `X-Prh-Time: <unix seconds>`. The client adopts the
offset and retries; without it a phone with a drifting clock fails every
request with a 401 indistinguishable from a bad credential.

#### `POST /v1/heartbeat`

```jsonc
// 200
{ "ok": true, "expires_at": 1765432100, "server_time": 1765388900 }
// 401 — session gone, log in again
```

Sessions are memory-only, so a `prh` restart drops all of them. The heartbeat
is how the companion finds that out on a schedule instead of discovering it
when a prompt is already blocking the agent. Recovery needs nothing from the
user: the device secret is persisted on both sides, so the phone logs in again
by itself. The companion beats every 4 minutes against a 12h TTL.

### `GET /v1/poll?cursor=<n>&wait=<seconds>`

Long-poll. Blocks up to `wait` (default 25, max 55) for events after `cursor`.
Returns immediately if any are already queued.

```jsonc
// 200 — events available
{ "cursor": 42, "events": [ /* envelopes, oldest first */ ] }

// 200 — nothing happened, poll again with the same cursor
{ "cursor": 17, "events": [] }
```

The cursor is a monotonic per-device sequence. The companion persists it and
replays from it after a crash or reboot. `prh` retains a bounded ring buffer
(default 200 events); a cursor older than the buffer gets a `410` and the
companion resets to the newest cursor.

### Envelopes

Deliberately small — every field crosses Bluetooth eventually. Long strings
are truncated by `prh`, not by the companion, so truncation is consistent.

```jsonc
{
  "id":      "evt_0042",         // ack target, also the reply target
  "seq":     42,
  "type":    "perm",             // perm | ques | idle | err | note | msg | gone
  "project": "infra",            // <= 24 chars; which window is asking
  "session": "ses_ab12",         // opaque id, for /v1/prompt routing
  "title":   "bash",             // <= 32 chars, the action
  "body":    "rm -rf build/",    // <= 256 chars, the resource
  "choices": ["Approve", "Always", "Reject"],  // <= 6, <= 24 chars each
  "expires": 1765400000          // unix seconds; past this, it is stale
}
```

`msg` envelopes carry extra fields the companion uses for its conversation view
and the watch never sees (they do not cross Bluetooth):

```jsonc
{
  "type":      "msg",
  "session":   "ses_ab12",
  "msg_role":  "assistant",      // user | assistant
  "msg_part_id": "p_01H…",       // part id, for replace-in-place
  "msg_text":  "I'll remove the build directory",  // accumulated part text, <= 4096
  "msg_kind":  "text",           // text | reasoning | tool | step-start | file | patch
  "msg_time":  1765400000000     // unix ms
}
```

| `type` | Source event | Stream | Watch behaviour | Companion behaviour |
|---|---|---|---|---|
| `perm` | `permission.v2.asked` | both | Wake app, vibrate, show prompt card | Inline approve in conversation view |
| `ques` | `question.v2.asked` | both | Wake app, vibrate, show choice list | Inline approve in conversation view |
| `idle` | `session.idle` | `/v1/poll` | Notify only, no reply expected | Status update |
| `err`  | `session.error` | `/v1/poll` | Notify only, no reply expected | Status update |
| `note` | internal | `/v1/poll` | Status text, no reply expected | Status update |
| `msg`  | `message.part.updated` | conversation | **never sent** — does not cross Bluetooth | Conversation view |
| `gone` | `permission.replied` | both | Dismiss `id`; it was answered elsewhere | Clear pending prompt |

**`msg` envelopes are never published to `/v1/poll`.** The poll ring is the
watch's stream: it is small, and the companion downloads all of it before
discarding what the watch does not need. A streaming reply emits hundreds of
part updates, which would spend the phone's mobile data on content the poll
path throws away and — the real damage — evict pending permission envelopes
from the ring before the watch ever polled them. Conversation has its own
endpoint so it cannot crowd out the prompts.

Prompts appear on **both** streams: `/v1/poll` for the watch, and the owning
session's conversation so the phone can render approve/reject inline under the
context that led to the request. When the prompt is settled — on the watch, on
the phone, or at the desk — a `gone` envelope replaces it under the same `id`
in both places.

### `POST /v1/reply`

```jsonc
{
  "event_id": "evt_0042",
  "action":   "once",   // once | always | reject | choice | text
  "choice":   0,        // index into choices[], when action=choice
  "text":     "..."     // dictation result, when action=text
}
// 200 {"ok":true}   409 if already answered or expired
```

`prh` does not call Kilo. It turns this into a decision on Hop 0 and the
plugin applies it, so the companion stays ignorant of Kilo's shapes and `prh`
stays incapable of anything the plugin does not offer.

`200` means the decision was queued, not that it was applied — the plugin's
`ack` is what confirms that. The watch should show "sent" until the ack
arrives.

### `POST /v1/prompt`

Dictation that starts new work rather than answering a prompt. Becomes a
`kind: "prompt"` decision on Hop 0.

```jsonc
{ "session": "ses_ab12", "text": "run the tests again" }
```

### Sessions + conversation (phone UI)

The phone has a session list and a per-session conversation view, so you can
read what the agent is doing before approving from the phone — the watch's
200px screen cannot show enough context for that. These endpoints are signed
like the rest of Hop 1; the conversation data never crosses Bluetooth.

`prh` builds the session registry from events the plugin forwards
(`session.created/updated/deleted/status`), not by calling Kilo, so it needs
no credentials. Conversation content comes from `message.part.updated`, which
the plugin forwards as `kind: "message"` events.

#### `GET /v1/sessions`

```jsonc
// 200
{ "sessions": [
  { "id": "ses_ab12",
    "project": "infra",           // which window owns it
    "title": "Fix the login bug",  // Kilo's session title, truncated
    "dir": "infra",                // working directory basename
    "status": "busy",              // idle | busy | retry | unknown
    "updated": 1765400000000,      // unix ms, last activity
    "has_prompt": true,            // a perm/ques envelope is pending
    "prompt_id": "evt_0042",       // the pending envelope id
    "prompt_type": "perm"          // perm | ques
  } ] }
```

`has_prompt` is what the session list badges — a session with a pending prompt
is the one you want to open.

#### `GET /v1/sessions/{id}/conversation?cursor=<n>&wait=<seconds>`

Long-poll, like `/v1/poll`. Returns the session's conversation entries newer
than the per-session cursor: `msg` envelopes, plus any `perm`/`ques` prompt
belonging to this session and the `gone` that retracts it.

```jsonc
// 200
{ "cursor": 3,
  "events": [ /* envelopes, creation order, oldest first */ ] }
// 404 unknown session (prh restarted, or it was never seen)
```

The cursor is per-session, separate from the `/v1/poll` cursor, so the
conversation view and the watch's prompt stream advance independently.

Entries are keyed by `id` — the part id for a message, the envelope id for a
prompt — and an entry that already exists is **replaced in place**, keeping its
original position while taking a fresh `cursor` value. The agent streams by
re-sending a part with more text, dozens to hundreds of times per reply, so a
client must render by `id` rather than appending: the same `id` arriving again
is a correction, not a new message. `prh` keeps the last 50 entries per
session, counted as messages rather than updates — without the replace, one
streaming reply would evict the entire history behind it.

An entry whose text is empty is a retraction: the agent removed that part, and
the client should drop the row.

#### `POST /v1/sessions/{id}/prompt`

Replies in a session — starts a new turn by sending text. Becomes a
`kind: "prompt"` decision on Hop 0, which the plugin applies via Kilo's
`prompt_async`. `prh` never holds credentials, so it cannot send the text
itself.

```jsonc
{ "text": "use the queue approach instead" }   // <= 4096 bytes, truncated
// 200 {"ok":true}
// 400 empty text
// 404 unknown session
// 409 the window that owned the session is gone — retrying will not help
```

This is the "reply in sessions" affordance. It does not answer a pending
permission prompt — use `/v1/reply` for that. It starts new work, the same way
dictation does, but from the phone with full context visible.

The plugin applies it through `client.session.promptAsync` and deduplicates on
the decision's nonce, so a redelivered decision cannot inject the same message
into a session twice.

### Admin plane — unix socket only

Served on the plugin socket, never on the network listener. Arming enrolment
must not be reachable by the people enrolment defends against, and anyone who
can open that socket is already this UID and has better options than pairing a
phone.

```jsonc
POST   /admin/v1/pairing   { "pairing_key": "<base64url, >= 32 bytes>", "ttl_sec": 120 }
DELETE /admin/v1/pairing   // close early, e.g. the user shut the panel
GET    /admin/v1/pairing   // { "open": true, "expires_at": 1765400000 }
```

The caller supplies the key rather than prh minting one, because whoever opens
the window is also who has to display it. Minting it in prh would mean sending
the same secret back over the socket for no gain.

### `GET /v1/health`

Unauthenticated liveness, for the extension's status bar. Returns version,
uptime, connected Kilo instances, registered device count, and live session
count. No secrets.

`sessions` drops to zero on restart while `devices` does not — that difference
is exactly the condition heartbeats repair.

## Hop 2 — companion ↔ watchapp (AppMessage)

AppMessage buffers are small and inbox size is negotiated at connect. Keep
every message under ~1 KB and expect the watch to reject oversized dicts.

Keys are declared in `watchapp/package.json` under `messageKeys`, which
generates `MESSAGE_KEY_*` in C.

### Companion → watch

| Key | Type | Notes |
|---|---|---|
| `EVENT_ID` | string | echoed back in the reply |
| `EVENT_TYPE` | uint8 | 1 perm, 2 ques, 3 idle, 4 err, 5 note, 6 gone |
| `PROJECT` | string | which window is asking; shown in the header |
| `SESSION` | string | opaque session id, for dictation routing |
| `TITLE` | string | the action |
| `BODY` | string | the resource, truncated |
| `CHOICES` | string | `\x1f`-separated, empty when none |
| `STATUS` | uint8 | 0 disconnected, 1 connected, 2 degraded |

### Watch → companion

| Key | Type | Notes |
|---|---|---|
| `REPLY_ID` | string | must match `EVENT_ID` |
| `REPLY_ACTION` | uint8 | 1 once, 2 always, 3 reject, 4 choice, 5 text |
| `REPLY_CHOICE` | uint8 | index, when action is 4 |
| `REPLY_TEXT` | string | dictation result, when action is 5 |

The watch does not retry. If an ack does not arrive it shows a failure and the
companion's own `POST /v1/reply` retry is what actually guarantees delivery.

## Buttons

```
  UP     Approve once          (list: previous choice)
  SELECT Always allow          (list: pick)
  DOWN   Reject                (list: next choice)
  LONG SELECT  Dictate a reply
  BACK   Dismiss (leaves the prompt unanswered and pending)
```

Dictation uses the PT2 microphone via the `dictation` API, producing
`REPLY_ACTION=5`. Dismissing is not answering: the prompt stays pending in
`prh` until it expires or is answered elsewhere.

## Security

Each hop has its own trust level, and the design keeps the strongest secret on
the innermost one. Full threat model in `plugin.md`.

**Hop 0 (plugin ↔ `prh`)** — never on the network.

- `AF_UNIX` socket in a `0700` directory, peer UID verified on accept.
- Carries no credentials in either direction. `prh` holds no Kilo password and
  cannot call Kilo's API; the four decision kinds are its entire reach.
- Decisions carry a nonce so a retry cannot become a second approval, and
  expiry means *leave pending*, never *approve*.

**Hop 1 (`prh` ↔ companion)** — the exposed surface.

- The pairing passphrase is stored **hashed** (argon2id) in `prh` config; the
  plaintext lives only in VSCode `SecretStorage`. No shipped client offers
  passphrase pairing any more, so the plaintext no longer crosses the wire at
  all.
- **Enrolment is gated on a pairing window** the user opens deliberately, and
  closes after one device. Before that gate, anyone who ever learned the
  passphrase — by sniffing, a screenshot, a clipboard, or reading your screen —
  could enrol at any hour.
- The enrolment response is **sealed** under a key that reached the phone by
  camera, so capturing the exchange yields nothing.
- Passphrase registration is rate-limited on the server for the same reason the
  window gate exists; repeated failures lock out the source IP. Neither login
  nor sealed registration is rate-limited, and neither needs to be — both keys
  are 32 random bytes, so there is nothing to guess.
- **Every other request is signed**, so no credential is replayable. See
  Authentication above for what each signed field defends against.
- Devices are independently revocable from the extension, and revoking one
  drops its live sessions immediately rather than letting them run out their
  TTL.
- `prh` binds `0.0.0.0` by default because the phone must reach it. Narrow
  this to the LAN interface if the host is multi-homed.
- **Traffic is encrypted.** TLS 1.2+ with a self-signed certificate whose key
  the companion pins from the pairing QR. Envelope bodies — command lines and
  file paths — are no longer readable on the wire, and there is no CA to
  mis-issue against.
- Signing is kept *on top of* TLS rather than replaced by it. They answer
  different questions: TLS says the channel is private and leads to the right
  machine, the signature says this specific request came from an enrolled
  device and has not been replayed. The signature also survives a mistake in
  the hand-rolled pinning code, which is exactly the sort of thing worth
  double-covering.
- Off-LAN, still put it behind Tailscale rather than port-forwarding. TLS makes
  exposure survivable, not advisable.

### Secrets at rest

Signing needs the key on both sides, so `devices.json` holds device secrets in
**plaintext** at mode 0600, written atomically (temp file, fsync, rename) so a
crash cannot truncate it into an empty registry that silently deauthorises
every phone.

That is a deliberate trade against the previous design, which stored only
token *hashes*. It moves an exposure from the wire to the disk:

- before — a credential replayed on every request over unencrypted LAN HTTP,
  where sniffing it is passive and undetectable
- after — a credential in a 0600 file, readable only by a process already
  running as this user, which by then can read the source, the Kilo password,
  and the SSH keys anyway

On a machine an attacker already owns, the device secret is not the
interesting thing they found. On a LAN they merely share, it was.

The companion mirrors this: the device secret goes in
`EncryptedSharedPreferences`. The session key is never written to disk — it is
short-lived, cheap to re-obtain, and worthless after a restart. Neither is the
passphrase, which would be a *stronger* secret than the one it protects, since
it can enrol new devices.

**Hop 2 (companion ↔ watch)** — Bluetooth, and out of our hands. Anyone who
can see your wrist can read what the agent proposed.
