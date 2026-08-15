# pebble-remote-harness

[![ci](https://github.com/yyewolf/pebble-remote-harness/actions/workflows/ci.yml/badge.svg)](https://github.com/yyewolf/pebble-remote-harness/actions/workflows/ci.yml)

Approve your coding agent's permission prompts from a Pebble Time 2, and get
told when a session stops.

> **Status: scaffold.** Every component's structure, wire protocol, and open
> questions are written down. Almost no behaviour is implemented — the Go
> daemon builds and serves `/v1/health`, and everything else returns 501 or
> throws `NotImplementedError`. See [Current state](#current-state).

## How it works

```
kilo plugin ──unix socket──▶ prh (Go) ──long-poll──▶ Android companion ──BLE──▶ Pebble Time 2
  in-process                 one per machine          wakes the watchapp
```

Kilo Code ships a headless [opencode](https://opencode.ai) server that already
emits `permission.v2.asked`, `question.v2.asked`, `session.idle`, and
`session.error` — so the hard part, hooking into the agent's approval flow,
needs no patching.

A **plugin** inside each kilo server reports those events to `prh` and applies
the decisions you make. That inversion is the security design: Kilo's password
grants shell access through the agent, so `prh` is never given one. It holds
no Kilo credentials and can only ask the plugin for four scoped operations.

**One `prh` per machine**, not per window. Every VSCode window feeds the same
daemon, so the phone pairs once and sees every project — with the project
named on the watch, because approving the right command in the wrong
repository is the mistake this must not enable.

The Android companion carries the network because PebbleKit JS is killed
whenever the watchapp closes; it calls `startAppOnPebble()` to wake the watch
when a prompt lands.

Full detail in [docs/architecture.md](docs/architecture.md).

## Layout

| Path | What |
|---|---|
| `api/` | `prh`, the Go daemon. Stdlib only so far. |
| `plugin/` | Kilo plugin. Holds the credentials, shares none of them. |
| `extension/` | VSCode extension: owns the password, elects the daemon, shows the pairing QR. |
| `companion/` | Android app: holds the long-poll, wakes the watchapp. |
| `watchapp/` | Pebble Time 2 app (`emery`). Pure UI, never speaks HTTP. |
| `docs/` | Architecture, wire protocol, security model, Kilo findings. |

## Docs

- [implementation.md](docs/implementation.md) — **start here to build**: ordered milestones, what is verified vs assumed, traps already paid for
- [architecture.md](docs/architecture.md) — the five components, and one daemon across many windows
- [plugin.md](docs/plugin.md) — **the security model**: threat model, trust boundary, install lifecycle
- [protocol.md](docs/protocol.md) — the v1 wire contract, all three hops
- [kilo-integration.md](docs/kilo-integration.md) — what Kilo Code's bundled server exposes, and how it was found
- [android-companion.md](docs/android-companion.md) — the companion's job, and the probe that must pass first
- [notifications.md](docs/notifications.md) — waking a closed watchapp, and the fallbacks
- [toolchain.md](docs/toolchain.md) — verified versions and how to install them without root
- [release.md](docs/release.md) — how a tag becomes a release, and why there is no Windows VSIX

## Current state

| Component | Builds here | Implemented |
|---|---|---|
| `api/` | yes — `bin/prh` runs | types, routing, `/v1/health`, **socket + singleton election** |
| `plugin/` | yes — loads in a real kilo server | fail-open path; hooks are stubs |
| `extension/` | yes — `tsc` clean | manifest, commands, status bar shape |
| `companion/` | yes — `app-debug.apk`, 4.0 MB | data models only |
| `watchapp/` | yes — `watchapp.pbw`, 18 KB | UI skeleton, button map, AppMessage decode |

`make all` builds all four from clean. Toolchain versions and install paths
are in [docs/toolchain.md](docs/toolchain.md) — everything lives in `$HOME`
and none of it needed root.

Building is not working: every component compiles, and almost none of it does
anything yet. Two things beyond compilation are genuinely verified — the
daemon's socket election (a second instance refuses to start, and a socket
left by a `SIGKILL` is reclaimed) and the plugin's fail-open path (loads into
a real kilo server with no daemon running, zero errors, sessions unaffected).

The watchapp fits comfortably: 3029 bytes of RAM against a 128K budget,
leaving ~125K of heap.

## Before writing more code

The design questions are settled. What remains is implementation, plus one
end-to-end test.

Resolved along the way, each by probing rather than assuming:

- **PebbleKit compatibility.** The Core Devices app (`coredevices.coreapp`
  1.8.0.7) supports classic PebbleKit. Its manifest declares no receivers,
  which looks fatal, but it registers them at runtime and the dex carries the
  full `com.getpebble.action.*` set including `app.START`. Its classic content
  provider answers live queries: watch connected, AppMessage supported,
  firmware 4.33.2. See
  [android-companion.md](docs/android-companion.md#compatibility-verified).
- **Server discovery.** Not needed — the plugin runs inside each kilo server.
- **The permission event shape.** `permission.asked`, not the v2 name, with
  different field names throughout.
- **Reply bodies.** `{"reply": "once"|"always"|"reject"}`.

**The wake mechanism is proven on hardware.** With the watchapp closed, an
`app.START` broadcast opened it on the wrist, and the watch confirmed the
launch back over the protocol. That test also caught a silent-failure bug —
AppMessage keys start at 10000, not 0, so the companion's constants would have
produced dictionaries the watchapp ignores without complaint.

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

## Installing

Grab the [latest release](https://github.com/yyewolf/pebble-remote-harness/releases/latest)
— every asset there is built from its tag by CI, and `SHA256SUMS` covers all of
them. Pick the `.vsix` matching your platform; it carries `prh` and the Kilo
plugin, so it is the only step on the VSCode side.

Or build the same set yourself:

```bash
make package      # everything installable, into dist/
```

| Artifact | Where it goes |
| --- | --- |
| `pebble-remote-harness-0.1.0.vsix` | VSCode → Extensions → *Install from VSIX* |
| `prh-companion.apk` | `adb install -r dist/prh-companion.apk` |
| `prh-watchapp.pbw` | `pebble install --phone <ip>`, or the Pebble app |
| `prh` | standalone daemon; only needed to run without VSCode |

The VSIX carries the `prh` binary and the Kilo plugin, so installing it is the
only step on the VSCode side. The plugin is still **not** installed silently —
run *Pebble Harness: Install Kilo plugin*, which states plainly that it loads
into every Kilo session on the machine.

Then pair: *Pebble Harness: Pair a phone*, or `prh pair` for the same URL on
the command line.

## Security

Full model in [docs/plugin.md](docs/plugin.md). The short version:

- **`prh` holds no Kilo credentials.** It cannot call Kilo's API at all. A
  compromised `prh` can approve prompts the agent already proposed — that is
  the entire blast radius, and it is enforced by structure rather than policy.
- **The plugin channel is a unix socket** in a `0700` directory, never a
  loopback port, which any local process could squat before `prh` binds it.
- **The boundary is the user account, not the process.** Anyone running as you
  can already read `KILO_SERVER_PASSWORD` out of `/proc`. We do not pretend
  otherwise, and we do not add secrets that would only be protected by the
  same permissions.
- **The LAN hop is TLS with a pinned self-signed certificate.** The pairing
  code carries both a one-time enrolment key and the certificate's public-key
  fingerprint, so the phone learns which machine to trust over a channel an
  attacker on the LAN cannot reach. Every request after that is HMAC-signed
  against a session key, with a dated, nonced canonical string so a captured
  request cannot be replayed. Off-LAN, still put it behind Tailscale rather
  than port-forwarding it.
- Decisions carry a nonce so a retry cannot become a second approval, and a
  timeout leaves a prompt pending rather than approving it.

## License

MIT
