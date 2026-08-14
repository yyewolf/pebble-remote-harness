# The Kilo plugin

`prh` learns about permission prompts through a Kilo plugin rather than by
discovering Kilo's servers and borrowing their credentials. This document is
the lifecycle and the security model; `kilo-integration.md` covers what the
plugin hooks into.

## The inversion that makes this safe

The obvious design is: find each `kilo serve`, read its password, let `prh`
drive its HTTP API. That works (verified) but it means moving an **RCE
credential** around. The Kilo password grants `/session/{id}/shell`,
`prompt_async`, and `permission/allow-everything` — anyone holding it owns the
machine as thoroughly as the agent does.

So the plugin does not hand over credentials. **It is the connector.**

```
  kilo serve process
  ┌──────────────────────────────────┐
  │  plugin (in-process)             │
  │    has: client, serverUrl,       │   credentials never leave
  │         KILO_SERVER_PASSWORD     │   this process
  │                                  │
  │    uplink   POST /plugin/v1/events
  │    downlink GET  /plugin/v1/decisions  (long-poll)
  └───────────────┬──────────────────┘
                  │  HTTP over a unix socket
                  ▼
              prh daemon
```

`prh` holds **no Kilo credentials at all**. It cannot call Kilo's API. It can
only ask the plugin to do one of four things — answer a permission, answer a
question, send a prompt, abort a turn. That is the entire blast radius of a
compromised `prh`, and it is least privilege by construction rather than by
policy.

Both halves are verified against Kilo 7.4.22: a plugin sees `serverUrl`,
`process.env.KILO_SERVER_PASSWORD`, `directory` and `project`, and it can hold
a background async loop that keeps ticking after load — which is what the
downlink long-poll needs.

## Transport: a unix socket, not loopback TCP

The local channel is HTTP over `AF_UNIX` at:

```
${XDG_RUNTIME_DIR:-~/.local/state}/prh/plugin.sock      # socket
${XDG_RUNTIME_DIR:-~/.local/state}/prh/                 # dir, mode 0700
```

Loopback TCP was rejected. Any local process can bind `127.0.0.1:8477` before
`prh` does and harvest whatever plugins connect and say — a port-squatting
attack that needs no privileges. A socket inside a `0700` directory cannot be
squatted by another user, and the path is not reachable from the network at
all.

This also answers *how the plugin authenticates `prh`*: by the socket's
location. Only the owning user can create a file in that directory, so a
socket found there was placed there by that user's `prh`.

`prh` should additionally check peer credentials on accept (`SO_PEERCRED` on
Linux, `LOCAL_PEERCRED` on macOS) and reject connections whose UID is not its
own. Cheap, and it turns a directory-permission assumption into a verified
fact.

Windows has no `XDG_RUNTIME_DIR`; modern Windows supports `AF_UNIX`, but a
named pipe with a matching ACL is the idiomatic equivalent. Unresolved.

## What the trust boundary actually is

Be honest about this, because it bounds every other decision:

> The security boundary is the **user account**, not the process.

Any process running as you can already read `/proc/<pid>/environ` (Linux) and
lift the Kilo password directly, with or without this project. A same-UID
attacker is not a threat we can defend against — they have already won.

So the goal is **not to make things worse**:

| Threat | Handled |
|---|---|
| Another **user** on the box | yes — `0700` dir, socket perms, peer-UID check |
| A process on the **LAN** | yes — the plugin channel is not on the network |
| **Port squatting** by a local process | yes — no TCP port to squat |
| Credential theft via `prh` | yes — `prh` never holds Kilo credentials |
| A **same-UID** attacker | no, and neither does Kilo. Out of scope, stated plainly |

Adding a pre-shared key on top of the socket would look reassuring and buy
nothing: it would have to live in a file protected by the same permissions
that already protect the socket. Do not add security theatre.

## Authentication summary

| Direction | Mechanism |
|---|---|
| plugin → `prh` | connect to a socket in a `0700` dir; `prh` verifies peer UID |
| `prh` → plugin | the socket path is unforgeable by other users |
| `prh` → companion | pairing password, then a revocable device token |
| `prh` → Kilo | **none — deliberately impossible** |

## Installation lifecycle

Global means **every window and every project**, including agent sessions that
have nothing to do with the harness.

### What actually loads a plugin

Measured on Kilo 7.4.22, because none of it was guessable:

- **`~/.config/kilo/opencode.json` is the file that works.** Its `plugin`
  array is read and honoured.
- **`~/.config/kilo/kilo.jsonc`'s `plugin` array is ignored.** Verified by
  removing the entry from `opencode.json` while leaving it in `kilo.jsonc`:
  the plugin stopped loading entirely. Everything *else* in `kilo.jsonc`
  (model, providers, permissions) is honoured, which makes this trap
  convincing — the file plainly works, just not for plugins.
- **An absolute path resolves; a bare package name is what `kilo plugin -g`
  writes.** The working entry is
  `/home/yewolf/.config/kilo/node_modules/@yyewolf/prh-plugin`.
- There are **two** global module roots, `~/.config/kilo/node_modules` and
  `~/.kilocode/node_modules`, and the instance bootstrap reads config from
  `~/.kilocode/` as well. Only the `~/.config/kilo` absolute path was proven.

Confirm a load by watching `upstreams` in `GET /v1/health` — it increments
when the plugin's `hello` lands. Kilo logs **nothing** on plugin load,
success or failure, so the daemon is the only observable signal.

### `kilo plugin -g` cannot install this plugin

```
$ kilo plugin -g @yyewolf/prh-plugin
■  404 Not Found - GET https://registry.npmjs.org/@yyewolf%2fprh-plugin
```

It resolves from npm only. Until the package is published, installing means
linking the module into a global root and adding its absolute path to
`opencode.json` by hand. `extension/src/kiloPlugin.ts` still shells out to
`kilo plugin -g` and will therefore fail — see the note there.

Also note `kilo plugin` has **no subcommands**: `kilo plugin list` treats
`list` as a module name and writes `{"plugin": ["list"]}` into the local
`.kilo/opencode.json`.

### The extension must not install it silently

Installing a plugin into every future agent session is a global, security-
relevant act. It should not be a side effect of installing a VSCode extension.

The flow:

1. On activation the extension **detects** the plugin by reading
   `~/.config/kilo/package.json` and comparing the pinned version.
2. If missing or stale, it surfaces a non-modal prompt — never a silent fix.
3. `Pebble Harness: Install Kilo plugin` states plainly that this affects all
   projects, then installs.
4. `Pebble Harness: Remove Kilo plugin` reverses it.

Steps 1 and 3 are both wrong as implemented. Detection reads the npm
`dependencies` map, where a local install appears as
`"file:../../workspace/…"` and parses to a junk version, so a working install
reports as `outdated`. Installation shells out to `kilo plugin -g`, which
404s for an unpublished package. Fixing this means choosing: publish to npm,
or have the extension write `opencode.json` itself.

Version is pinned by the extension, not floated. The plugin sends its protocol
version in `hello`; `prh` rejects a mismatch loudly rather than guessing.

### Races and restarts

- **Several windows at once.** Each one may notice a stale plugin
  simultaneously. Guard the install with a lock file; user-initiated install
  makes this rare rather than routine.
- **VSCode reload.** Kilo restarts, the plugin reloads and re-registers.
  `prh` keys an upstream on `(parent_pid, directory)` and replaces it.
- **`prh` restarts.** The plugin's long-poll fails; reconnect with backoff.
- **`kilo serve --pure`** disables external plugins. The harness then silently
  does nothing, so the extension should detect it and say so.

## The plugin must fail open

It runs inside the user's coding agent. A bug in it must never break a coding
session:

- No socket, no `prh`, no key → **no-op silently**. Not an error, not a toast.
- Every hook wrapped so a throw cannot escape into Kilo.
- The uplink is fire-and-forget with a bounded queue; drop events rather than
  block a turn.
- Never block the agent waiting for `prh`. A prompt with no answer stays
  pending exactly as it would without the harness.

## Rules for applying decisions

The plugin is the thing that can actually approve a command, so it is the last
line of defence:

- Apply a decision **only** for a request ID the plugin itself forwarded and
  that is still pending. Never trust a request ID that arrives unsolicited.
- Each decision carries a nonce; a repeat is dropped. `prh` retries on network
  failure, and a retry must not become a second approval.
- **Never** call `permission/allow-everything`, and never accept a decision
  that would. It is not in the vocabulary.
- Decisions expire. Past the deadline the request stays pending — timeouts
  resolve to *no answer*, never to *approve*.
- Forward only the action and resource strings `prh` already truncates. Never
  file contents.

## Residual risks

- **A compromised `prh` can approve prompts.** Not arbitrary code execution,
  but it can say yes to something the agent proposed. This is inherent to
  remote approval and is the reason the reply path is scoped so narrowly.
- **The `/v1` LAN surface is plaintext HTTP.** Prompt bodies are command lines
  and file paths. See `protocol.md`; the fix is a pinned self-signed
  certificate distributed through the pairing QR, or Tailscale.
- **The watch displays commands.** Anyone who can see your wrist can read what
  the agent proposed.
- **Kilo's plugin API is undocumented** and can break on upgrade. That risk
  now sits in a file we control instead of in `prh`.

## Open questions

- ~~The `permission.ask` hook signature is unverified.~~ **Settled**: probed
  against a real prompt, `permission.ask` does not fire in Kilo 7.4.22. Use
  the `event` hook and translate the v1 `permission.asked` payload. See
  `kilo-integration.md`.
- Whether `~/.config/kilo/plugins/*.js` is a supported global location. A file
  placed there loaded correctly, then the directory was pruned — the npm
  dependency route is the documented one and the one to rely on.
- Windows: named pipe versus `AF_UNIX`.
