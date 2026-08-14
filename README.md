# pebble-remote-harness

Approve your coding agent's permission prompts from a Pebble Time 2, and get
told when a session stops.

> **Status: scaffold.** Every component's structure, wire protocol, and open
> questions are written down. Almost no behaviour is implemented — the Go
> daemon builds and serves `/v1/health`, and everything else returns 501 or
> throws `NotImplementedError`. See [Current state](#current-state).

## How it works

```
kilo serve  ──SSE──▶  prh (Go)  ──long-poll──▶  Android companion  ──BLE──▶  Pebble Time 2
```

Kilo Code ships a headless [opencode](https://opencode.ai) server that already
emits `permission.v2.asked`, `question.v2.asked`, `session.idle`, and
`session.error` — so the hard part, hooking into the agent's approval flow,
needs no patching. `prh` collapses that ~200-event-type firehose into five
watch-sized envelopes and holds them in a replayable queue. The Android
companion carries the network because PebbleKit JS is killed whenever the
watchapp closes; it calls `startAppOnPebble()` to wake the watch when a prompt
lands.

Full detail in [docs/architecture.md](docs/architecture.md).

## Layout

| Path | What |
|---|---|
| `api/` | `prh`, the Go daemon. Stdlib only so far. |
| `extension/` | VSCode extension: owns the password, manages the daemon, shows the pairing QR. |
| `companion/` | Android app: holds the long-poll, wakes the watchapp. |
| `watchapp/` | Pebble Time 2 app (`emery`). Pure UI, never speaks HTTP. |
| `docs/` | Architecture, wire protocol, Kilo findings, wake strategies. |

## Docs

- [architecture.md](docs/architecture.md) — the four components and why each exists
- [protocol.md](docs/protocol.md) — the v1 wire contract, both hops
- [kilo-integration.md](docs/kilo-integration.md) — what Kilo Code's bundled server exposes, and how it was found
- [android-companion.md](docs/android-companion.md) — the companion's job, and the probe that must pass first
- [notifications.md](docs/notifications.md) — waking a closed watchapp, and the fallbacks

## Current state

| Component | Builds here | Implemented |
|---|---|---|
| `api/` | yes — `bin/prh` runs | types, config, routing, `/v1/health` |
| `extension/` | yes — `tsc` clean | manifest, commands, status bar shape |
| `companion/` | yes — `app-debug.apk`, 4.0 MB | data models only |
| `watchapp/` | yes — `watchapp.pbw`, 17 KB | UI skeleton, button map, AppMessage decode |

`make all` builds all four from clean. Toolchain versions and install paths
are in [docs/toolchain.md](docs/toolchain.md) — everything lives in `$HOME`
and none of it needed root.

Building is not working: every component compiles, and almost none of it
does anything yet. The watchapp does at least fit comfortably — 2810 bytes of
RAM against a 128K budget, leaving ~125K of heap.

## Before writing more code

Two things are unresolved, and one of them can invalidate real work:

1. **Does the Core Devices mobile app still implement the classic PebbleKit
   Android intent surface?** The entire companion design rests on it. The
   probe is in [docs/android-companion.md](docs/android-companion.md). Settle
   this first.
2. **The exact reply request bodies** for Kilo's permission and question
   endpoints. They are in the live `/doc` spec; nobody has transcribed them.

Server discovery *was* on this list and is now settled: Kilo Code spawns one
server per VSCode window with a random port and its own password, both
readable from `/proc/<pid>/environ`, and `KILO_PARENT_PID` ties each server to
the window that owns it. See
[docs/kilo-integration.md](docs/kilo-integration.md#discovery).

Also worth knowing: Kilo's API is undocumented and unstable, so a Kilo upgrade
can break the integration. `api/internal/kilo` is deliberately the only place
that knows those shapes.

## Building

```bash
make api          # go build
make extension    # npm install && tsc
make watchapp     # pebble build   (needs the Pebble SDK)
make companion    # gradle assembleDebug  (needs JDK + Android SDK)
```

## Security

The password you set in the extension is a LAN-trust credential, and traffic
is plaintext HTTP. Off-LAN, put it behind Tailscale — do not port-forward it.
Three secrets stay separate: Kilo's `KILO_SERVER_PASSWORD` never leaves the
dev box, the pairing password is stored hashed and typed once, and the phone
holds only a revocable device token.

## License

MIT
