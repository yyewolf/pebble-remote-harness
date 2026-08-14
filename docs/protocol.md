# Wire protocol

Two hops, two encodings.

```
prh  --JSON over HTTP-->  companion  --AppMessage dict-->  watchapp
```

Version: `v1`. Breaking changes bump the path prefix.

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
  "session": "ses_ab12",
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

`prh` maps this onto the right Kilo endpoint — `POST
/permission/{id}/reply`, `/permission/{id}/always-rules`, or
`/api/session/{sid}/question/{id}/reply`. The companion stays ignorant of
Kilo's shapes.

### `POST /v1/prompt`

Dictation that starts new work rather than answering a prompt. Forwards to
`POST /session/{sessionID}/prompt_async`.

```jsonc
{ "session": "ses_ab12", "text": "run the tests again" }
```

### `GET /v1/health`

Unauthenticated liveness, for the extension's status bar. Returns version,
uptime, connected Kilo instances, registered device count. No secrets.

### `POST /admin/upstream` — loopback only

Kilo Code spawns one server per VSCode window, each with a random port and
its own password, both rotating on every reload. Upstreams therefore cannot
come from static config: each window's extension discovers its own server and
registers it here.

```jsonc
{ "name": "infra",                       // project label, for the watch
  "base_url": "http://127.0.0.1:4096",
  "password": "...",                     // KILO_SERVER_PASSWORD
  "parent_pid": 2025346 }                // extension host, the identity key
```

Re-registering the same `parent_pid` replaces the entry, which is what a
VSCode reload produces. `DELETE /admin/upstream/{parent_pid}` drops it.

**This endpoint must bind loopback only.** It accepts a Kilo password in
plaintext and is not part of the surface the companion talks to. It is
separate from device auth: the extension is trusted by being on the box.

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
| `SESSION` | string | short label for the header |
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

- The password is stored **hashed** (argon2id) in `prh` config; the plaintext
  lives only in VSCode `SecretStorage`.
- Registration is rate-limited; repeated failures lock out the source IP.
- Device tokens are independently revocable from the extension.
- Traffic is plaintext HTTP. This is a LAN-trust design. Off-LAN, put it
  behind Tailscale rather than exposing the port — do not port-forward it.
- `prh` binds `0.0.0.0` by default because the phone must reach it. Narrow
  this to the LAN interface if the host is multi-homed.
