# Implementation guide

For whoever picks this up next. The design is settled and every component
compiles; what remains is filling in ~70 `TODO`s. This document says in what
order, and how to know when each is done.

## Read these first, in this order

1. `architecture.md` — the five components and why each exists
2. `plugin.md` — the security model; read before touching the plugin or the
   socket, because several rules there are load-bearing
3. `protocol.md` — the three hops and their payloads
4. `kilo-integration.md` — the captured event shapes
5. `toolchain.md` — how to build each component

## What is verified vs. what is assumed

Trust the first column; re-check the second before relying on it.

| Claim | Status |
|---|---|
| `permission.asked` payload shape | **captured live** |
| Reply bodies (`{"reply": "once"…}`) | from the OpenAPI spec |
| `permission.ask` hook does not fire | **probed** |
| Plugin can hold a background loop | **probed** |
| Plugin fails open with no daemon | **probed** |
| Global install path + `hello` handshake | **tested end to end** |
| Extension's detect/install of the plugin | **known broken** — see `plugin.md` |
| Socket election + stale reclaim | **tested** |
| Plugin routes absent from TCP | **tested** |
| `app.START` wakes a closed watchapp | **tested on hardware** |
| AppMessage keys are 10000-based | **observed on the wire** |
| PebbleKit 4.0.1 resolves and packages | **built** |
| `question.asked` payload shape | **schema-derived, NOT observed** |
| `permission.replied` retract flow | schema + one observation |
| Windows named pipe instead of `AF_UNIX` | unexplored |

## Milestones

Ordered by dependency. **M1 and M2 need no phone and no watch** — the entire
Kilo half is testable with `curl`, which is where to start.

### M1 — events reach `prh`

*Files:* `plugin/src/index.js`, `api/internal/httpapi`, `api/internal/hub`

- plugin: `hello()` and `report()` over the unix socket
- `prh`: `handlePluginHello`, `handlePluginEvents`, bind the upstream to the
  connection so a closing window drops it
- hub: `Translate` (v1 field names) and `Publish` with the ring buffer

**Done when:** with the plugin installed and `prh` running, triggering a real
permission prompt in Kilo makes an envelope appear on
`GET /v1/poll?cursor=0` — correct `project`, `title`, `body`, and an
`Always: <pattern>` choice.

### M2 — replies reach Kilo

*Files:* `api/internal/hub`, `plugin/src/index.js`

- `prh`: `handleReply` → decision queue; `handlePluginDecisions` long-poll
- plugin: `downlink()`, `apply()`, `ack`

**Done when:** `POST /v1/reply` with `{"action":"once"}` resolves the prompt in
the VSCode UI. Then check the safety rules actually hold: replay the same
decision (must be idempotent), send an unknown `request_id` (must be
refused), let one expire (must stay pending, never approve).

### M3 — device auth

*Files:* `api/internal/auth`, `api/internal/httpapi`

- argon2id via `golang.org/x/crypto` — the module's first dependency
- `/v1/register`, bearer auth, rate limiting

**Done when:** a wrong password is rejected and rate-limited, and a revoked
token stops working immediately.

### M4 — the companion

*Files:* `companion/`

- settings + `prh://` deep link, token in `EncryptedSharedPreferences`
- foreground service holding the long-poll
- `PebbleBridge`: `startAppOnPebble`, `sendDataToPebble`, reply receiver

**Done when:** with the watchapp closed, a real prompt lights up your wrist.

Watch for: message keys are **10000-based**; battery-optimisation exemption is
required or Doze kills the poll; the classic PebbleKit provider is the
connection-state source.

### M5 — the watchapp

*Files:* `watchapp/src/c/main.c`

- `parse_choices`, a `MenuLayer` for `ques`, `gone` dismissal
- ack handling: show "sent" until the companion confirms
- dictation wiring for `REPLY_TEXT`

**Done when:** every envelope type renders, and approving from the watch
resolves the prompt in VSCode.

`emery` has `PBL_TOUCH` — a tappable choice list beats paging with UP/DOWN.

### M6 — the extension

*Files:* `extension/src/`

- socket-based election, detached spawn, adopt-or-start
- pairing QR, password management, device revocation
- plugin detect/install/remove

**Done when:** two windows produce exactly one daemon, and closing one leaves
the other's upstream working.

## Cross-cutting, currently absent

- **No tests anywhere, and no CI.** Highest-value first tests: hub translation
  against the captured `permission.asked` payload, ring-buffer eviction and
  the `410` cursor path, and the plugin's decision-safety rules.
- **TLS on Hop 1.** Prompt bodies are command lines in plaintext. The intended
  fix is a self-signed cert with its fingerprint in the pairing QR.
- **The OpenAPI spec is not vendored.** Regenerate it with the snippet at the
  end of `kilo-integration.md` when you need a shape that is not documented.

## Re-running the probes

Everything asserted here was checked with a command, and all of them are
cheap to repeat after a Kilo or Core-app upgrade.

```bash
# Kilo's live API surface
KILO=~/.vscode-server/extensions/kilocode.kilo-code-*/bin/kilo
KILO_SERVER_PASSWORD=x $KILO serve --port 4098 &
curl -su kilo:x http://127.0.0.1:4098/doc > openapi.json

# Which hooks fire: drop a plugin in .kilo/plugins/ that logs, then
# trigger a prompt. See the probe described in kilo-integration.md.

# Pebble app compatibility
adb shell content query --uri content://com.getpebble.android.provider.basalt/state
# -> Row: 0 0=1, ...   column 0 is "connected"

# The wake path, with the watchapp closed
adb shell am broadcast -a com.getpebble.action.app.START \
  --es uuid 630aaa1e-ad28-4694-950d-a25105a7390b
```

## Traps already paid for

- Translate the **v1** permission shape. v2 is in the schema, is the tempting
  choice, and never fires — it would yield empty envelopes silently.
- AppMessage keys start at **10000**. Wrong keys are ignored without error.
- Editing `messageKeys` needs `pebble clean` first.
- AGP 9 rejects the standalone Kotlin plugin.
- The Core app's package is `coredevices.coreapp`, not the classic name that
  still appears inside its dex.
- PKJS starts automatically with the watchapp, so the fallback `pkjs` and the
  companion can both send messages. Keep pkjs inert.
- Plugin config goes in `~/.config/kilo/opencode.json`. The `plugin` array in
  `kilo.jsonc` is ignored even though the rest of that file works.
- Kilo runs on **Bun**, whose `http.Agent` ignores `socketPath` and dials
  localhost:80 instead. Set `socketPath` per request.
- Nothing logs a plugin load. Watch `upstreams` in `GET /v1/health`; a plugin
  that fails open is indistinguishable from one that was never loaded.
