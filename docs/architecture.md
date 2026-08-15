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
                 │  POST /v1/register  (pairing key -> sealed device secret)
                 │  POST /v1/login     (device secret -> session key)
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

## One daemon, many windows

There is exactly **one `prh` per user per machine**. The phone pairs with one
endpoint and sees every project on it. Windows come and go underneath.

This is the whole reason the extension is a lifecycle manager rather than a
host: a per-window daemon would mean a per-window port, a per-window pairing,
and a watch that has to be told which window to listen to.

### Electing the daemon

Every window runs the same logic on activation, and it is safe to run
concurrently:

1. **Try to connect** to the plugin socket. If it answers, a daemon is
   already running — adopt it and stop.
2. If the connection is refused but the socket file exists, it is stale.
3. **Take an exclusive `flock`** on `…/prh/daemon.lock`. Only one window wins.
4. The winner unlinks a stale socket and **spawns `prh` detached** — its own
   process group, `unref()`'d, not a child that dies with the extension host.
5. Losers wait briefly for the socket to appear, then adopt it.

The socket is the election token, so there is no separate registry to go
stale. A crashed daemon leaves a socket that fails to connect, which is
step 2.

### Lifetime

`prh` outlives every window deliberately. Closing the last VSCode window must
not drop the phone's endpoint — that is the moment you are most likely to walk
away from the desk, which is exactly when the watch matters.

It exits only on an explicit **Stop daemon** command. No idle timeout: an idle
daemon is the normal state of a harness that is waiting for you to be
interrupted.

### Windows that disagree

The first window to start the daemon sets its bind address and port. Later
windows must **not** restart it to apply their own settings — that would drop
every other window's upstream and unpair nothing gracefully. They compare
their settings against `/v1/health` and surface a warning instead.

Changing the pairing password goes *through* the running daemon, never by
restarting it.

### Upstreams come and go

An upstream is a plugin connection, and its lifetime is that connection's
lifetime. When a window closes, its kilo server exits, its plugin's socket
drops, and `prh` drops the upstream — no `DELETE` call to miss, no stale entry
if VSCode is killed rather than closed.

Reconnection from the same `(parent_pid, directory)` replaces the entry, which
is what a window reload looks like.

### Telling projects apart on the wrist

With every window feeding one endpoint, "may I run `rm -rf build/`?" is a
dangerous question without a label. Envelopes therefore carry a `project`
string derived from the plugin's `directory`, and the watch shows it in the
header. Approving the right command in the wrong repository is precisely the
mistake this design must not enable.

Multiple *machines* remain unsolved — one remote host, one `prh`, one pairing.
Pairing the companion with several servers is future work.

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

It also forwards what the agent is **saying** (`message.part.updated`) and
the session lifecycle (`session.created/updated/deleted/status`), so the
phone can show conversation context and a session list. And it applies
`prompt` decisions — text the phone sends into a session — via Kilo's
`prompt_async`, which is the "reply in sessions" affordance.

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
- **session list + per-session conversation view** — the phone surface for
  context the 200px watch screen cannot show. Lists every session across
  every window (`GET /v1/sessions`), renders the conversation
  (`GET /v1/sessions/{id}/conversation`), and lets you approve a pending
  prompt or reply with text (`POST /v1/sessions/{id}/prompt`) from the phone.
  `msg` envelopes never reach the watch: conversation is phone-only and stays
  off the slow, snooppable Bluetooth hop.

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
