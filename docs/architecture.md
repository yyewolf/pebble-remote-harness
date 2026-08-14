# Architecture

Four components, one job: let a Pebble Time 2 approve or deny an agent's
permission prompts — even when the watchapp is closed — and get told when a
session stops.

```
  VSCode (remote or local)
  ┌──────────────────────────────┐
  │ Kilo Code extension          │
  │   └─ bin/kilo serve  :4096   │  headless opencode server
  │        HTTP + SSE, Basic auth│
  └──────────────┬───────────────┘
                 │  GET /event   (SSE firehose, ~200 event types)
                 │  POST /permission/{id}/reply
                 ▼
  ┌──────────────────────────────┐
  │ prh — Go API           :8477 │  this repo, api/
  │   kilo/   SSE consumer       │
  │   hub/    translate + queue  │
  │   httpapi/ long-poll + auth  │
  └──────────────┬───────────────┘
                 │  POST /v1/register   (password -> device token)
                 │  GET  /v1/poll       (long-poll, watch-sized envelopes)
                 │  POST /v1/reply
                 ▼
  ┌──────────────────────────────┐
  │ Android companion            │  companion/
  │   foreground service, always │
  │   holds the long-poll        │
  │   PebbleKit Android          │
  └──────────────┬───────────────┘
                 │  startAppOnPebble(UUID)   <- wakes a closed watchapp
                 │  sendDataToPebble(dict)
                 ▼
  ┌──────────────────────────────┐
  │ Pebble Time 2  (emery)       │  watchapp/
  │   200x228, 64 colors         │  buttons + dictation
  └──────────────────────────────┘
```

## Why the Android companion carries the network

PebbleKit JS is started and killed with the watchapp — there is no background
networking on Pebble. An Android foreground service has no such limit, so the
companion holds the long-poll continuously and uses
`PebbleKit.startAppOnPebble()` to **launch the watchapp automatically** when a
permission request arrives. That is the only path that wakes the watch without
you tapping anything.

Consequences, all good:

- Registration (server IP, password) happens in a real Android settings
  screen, not a Clay config page and certainly not on watch buttons.
- The watchapp becomes a pure UI surface. It never speaks HTTP.
- PebbleKit JS is not needed at all. `watchapp/src/pkjs/` exists only as a
  fallback transport if the companion route fails to pan out — see the risk
  below.

## The risk that decides this design

Classic PebbleKit Android talks to the official Pebble Android app over a
documented intent surface (`com.getpebble.action.app.START`, broadcast
receivers, a content provider). **The Core Devices mobile app is a rewrite,
and whether it still implements that surface is unverified.**

This is the single assumption the companion path rests on. Verify it before
writing real code — see `docs/android-companion.md` for the specific probe.
If it does not hold, the fallbacks in `docs/notifications.md` apply and
`watchapp/src/pkjs/` becomes the transport again.

## Component responsibilities

### `api/` — the Go daemon (`prh`)

Owns all state. Runnable standalone (`prh serve`) so it survives a VSCode
restart; the extension is a lifecycle manager, not a host.

- `config/`   — bind address, password hash, upstream Kilo URLs
- `protocol/` — wire types shared with the companion and watch
- `kilo/`     — SSE consumer and reply client for one Kilo instance
- `hub/`      — event translation, per-device cursors, the acked queue
- `httpapi/`  — `/v1/register`, `/v1/poll`, `/v1/reply`, `/v1/prompt`
- `waker/`    — optional extra alert channels (timeline pins, ntfy)

Volume, transport, durability, and secret-splitting are why this exists rather
than pointing the companion straight at `kilo serve`: Kilo emits ~200 event
types including per-token deltas, the watch needs five; events need a cursor
and an ack to survive Bluetooth dropouts; and several VSCode windows should
fan into one endpoint the phone registers against once, with the Kilo password
never leaving the dev box.

### `companion/` — Android app

- foreground service holding `GET /v1/poll`
- PebbleKit bridge: `startAppOnPebble`, `sendDataToPebble`, ack receiver
- settings UI for server address + password, and pairing status
- Android notification as a visible fallback when the watch is unreachable

Min SDK 26 (foreground services), target current. Not compilable in this
checkout — no JDK or Android SDK present.

### `extension/` — VSCode extension

Thin. Generates and stores the password, starts and stops `prh`, shows the LAN
address and a QR to pair the companion, reports connection status. It does not
proxy traffic.

Under VSCode Remote (`.vscode-server`) the extension host and `prh` both run on
the **remote** machine. The phone must reach that host — fine on a LAN, needs
Tailscale or similar otherwise.

### `watchapp/` — Pebble Time 2 app

Pure UI: prompt card, choice list, dictation session. Receives AppMessages
from the companion, sends replies back the same way.

Target platform is `emery`, confirmed against SDK 4.33.1's
`pebble_sdk_platform.py` rather than assumed:

| Capability | Value |
|---|---|
| Display | 200x228, `PBL_COLOR`, `PBL_RECT` |
| Input | buttons, `PBL_TOUCH` |
| Audio | `PBL_MICROPHONE`, `PBL_SPEAKER` |
| Other | `PBL_RGB_BACKLIGHT`, `PBL_COMPASS`, `PBL_HEALTH`, `PBL_SMARTSTRAP` |
| Budget | 128K app binary, 128K app RAM, 256K resources |

`PBL_MICROPHONE` is what makes dictation possible; the built ELF links five
dictation symbols, so the capability is live and not merely declared.

`PBL_TOUCH` is unexploited and worth revisiting — a touch choice list beats
paging through options with UP/DOWN.

Do not confuse `emery` with `gabbro`, the other modern platform in the same
SDK: gabbro is 260x260 and `PBL_ROUND`, so a layout tuned for one is wrong on
the other.
