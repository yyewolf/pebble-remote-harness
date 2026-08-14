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
    { "kind": "permission",        // permission | question | idle | error
      "request_id": "per_01H…",    // the ID a decision must match
      "session_id": "ses_01H…",
      "action": "bash",
      "resources": ["rm -rf build/"],
      "can_save": true }           // whether "always" is offered
  ] }
```

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

## Hop 1 — `prh` ↔ companion (JSON/HTTP)

Default bind `0.0.0.0:8477`. Plaintext HTTP on a trusted LAN; see Security.

### `POST /v1/register`

Trades the shared password for a revocable per-device token. Called once from
the companion's settings screen.

```jsonc
// request
{ "password": "...", "device_name": "Pixel 8", "platform": "android" }

// 200
{ "device_id": "dev_7f3a", "token": "prh_...", "server_name": "workstation" }
// 401 on bad password, 429 after repeated failures
```

The token is a bearer credential for every other endpoint. The Kilo password
never leaves the dev box, and the `prh` password never reaches the watch.

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
  "type":    "perm",             // perm | ques | idle | err | note
  "project": "infra",            // <= 24 chars; which window is asking
  "session": "ses_ab12",         // opaque id, for /v1/prompt routing
  "title":   "bash",             // <= 32 chars, the action
  "body":    "rm -rf build/",    // <= 256 chars, the resource
  "choices": ["Approve", "Always", "Reject"],  // <= 6, <= 24 chars each
  "expires": 1765400000          // unix seconds; past this, it is stale
}
```

| `type` | Source event | Watch behaviour |
|---|---|---|
| `perm` | `permission.v2.asked` | Wake app, vibrate, show prompt card |
| `ques` | `question.v2.asked` | Wake app, vibrate, show choice list |
| `idle` | `session.idle` | Notify only, no reply expected |
| `err`  | `session.error` | Notify only, no reply expected |
| `note` | internal | Status text, no reply expected |

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

### `GET /v1/health`

Unauthenticated liveness, for the extension's status bar. Returns version,
uptime, connected Kilo instances, registered device count. No secrets.

## Hop 2 — companion ↔ watchapp (AppMessage)

AppMessage buffers are small and inbox size is negotiated at connect. Keep
every message under ~1 KB and expect the watch to reject oversized dicts.

Keys are declared in `watchapp/package.json` under `messageKeys`, which
generates `MESSAGE_KEY_*` in C.

### Companion → watch

| Key | Type | Notes |
|---|---|---|
| `EVENT_ID` | string | echoed back in the reply |
| `EVENT_TYPE` | uint8 | 1 perm, 2 ques, 3 idle, 4 err, 5 note |
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

- The pairing password is stored **hashed** (argon2id) in `prh` config; the
  plaintext lives only in VSCode `SecretStorage`.
- Registration is rate-limited; repeated failures lock out the source IP.
- Device tokens are independently revocable from the extension.
- `prh` binds `0.0.0.0` by default because the phone must reach it. Narrow
  this to the LAN interface if the host is multi-homed.
- **Traffic is plaintext HTTP, and envelope bodies are command lines and file
  paths.** This is the weakest link in the design. LAN-trust only; off-LAN,
  put it behind Tailscale rather than port-forwarding. The intended fix is a
  self-signed certificate whose fingerprint travels in the pairing QR, so the
  companion can pin it without a CA.

**Hop 2 (companion ↔ watch)** — Bluetooth, and out of our hands. Anyone who
can see your wrist can read what the agent proposed.
