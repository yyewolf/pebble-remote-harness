# Architecture

Four components, one job: let a Pebble Time 2 approve or deny an agent's
permission prompts — even when the watchapp is closed — and get told when a
session stops.

```
  VSCode (remote or local)
  ┌──────────────────────────────┐
  │ Kilo Code extension          │
  │   └─ bin/kilo serve  (:0)    │  headless opencode server, random port
  │        ┌───────────────────┐ │
  │        │ prh plugin        │ │  plugin/ — runs in-process, holds the
  │        │ in-process        │ │  credentials and never shares them
  │        └─────────┬─────────┘ │
  └──────────────────┼───────────┘
                     │  POST /plugin/v1/events      (uplink)
                     │  GET  /plugin/v1/decisions   (downlink long-poll)
                     │  over a unix socket, 0700 dir
                     ▼
  ┌──────────────────────────────┐
  │ prh — Go API           :8477 │  this repo, api/
  │   plugin channel (AF_UNIX)   │  holds NO Kilo credentials
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

## Why a plugin rather than an API client

Kilo Code spawns one server per VSCode window with a random port and a fresh
64-character password, neither of which is knowable up front. `prh` could
discover both — the credentials sit in `/proc/<pid>/environ` — but that
credential grants shell access through the agent, and moving it around is the
last thing a "secure" design should do. It is also Linux-only.

A plugin runs *inside* each kilo server, so it needs no discovery and shares
no secrets. `prh` cannot call Kilo's API at all; it can only ask the plugin
for four scoped operations. See `plugin.md` for the security model and
`kilo-integration.md` for the superseded fallback.

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

- `config/`   — bind address, password hash, socket path
- `protocol/` — wire types shared with the companion and watch
- `hub/`      — event translation, per-device cursors, the acked queue
- `httpapi/`  — `/v1/*` for the companion, `/plugin/v1/*` on the unix socket
- `kilo/`     — **fallback only.** Direct API client for a standalone `prh`
  with no plugin installed. Linux-only, and it handles a credential the
  plugin path never exposes. Deletable once the plugin path is proven.
- `waker/`    — optional extra alert channels (timeline pins, ntfy)

### `plugin/` — the Kilo plugin

An npm module installed globally with `kilo plugin -g`, so it loads into every
kilo server on the machine. Pushes permission and question events up to `prh`
and long-polls for decisions, which it applies through its in-process client.

It is the only component holding Kilo credentials, and it never transmits
them. It must fail open: no `prh`, no socket, no problem — the coding session
continues untouched.

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
