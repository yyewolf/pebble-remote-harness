# @yyewolf/prh-plugin

Kilo plugin for the [Pebble Remote Harness](../README.md). It reports
permission prompts and session events to the `prh` daemon, and applies the
decisions you make on your watch.

## Install

```bash
kilo plugin -g @yyewolf/prh-plugin
```

Global, so it loads into **every** kilo server on the machine — every VSCode
window and every project, including sessions unrelated to the harness. The
VSCode extension will offer to run this for you, but it will not do it
silently: see `docs/plugin.md`.

## Why it exists

`prh` could instead discover each kilo server and use its HTTP API directly.
That means moving `KILO_SERVER_PASSWORD` around, and that password grants
shell access through the agent. This plugin holds the credentials and never
shares them; `prh` cannot call Kilo at all, only ask for four scoped
operations.

## What it sends

Actions and resource strings only — `"bash"`, `"rm -rf build/"` — plus session
and project identifiers. Never file contents. `prh` truncates the strings
before they reach any device.

## Safety rules

These are load-bearing, not stylistic:

- **Fails open.** No daemon, no socket, no problem: the plugin no-ops and your
  coding session is untouched. It never blocks a turn.
- **Only answers what it asked.** A decision is applied only for a request ID
  this plugin forwarded and that is still pending.
- **Never approves twice.** Decisions carry a nonce; repeats are dropped, so a
  network retry cannot become a second approval.
- **Timeouts mean pending, not yes.** An expired decision leaves the prompt
  exactly as it was.
- **`allow-everything` is not representable.** There is no code path to it.

## Connection

HTTP over a unix socket at `$XDG_RUNTIME_DIR/prh/plugin.sock`, falling back to
`~/.local/state/prh/plugin.sock`. Never TCP — a loopback port can be squatted
by any local process before `prh` binds it.

The path is computed independently here and in `api/internal/config`; changing
one means changing the other.
