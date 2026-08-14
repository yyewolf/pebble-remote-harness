# Kilo Code integration

Findings from Kilo Code `7.4.22` (`kilocode.kilo-code`, installed under
`~/.vscode-server/extensions/`). Re-verify after a Kilo upgrade — none of this
is a published, stable API.

## The bundled server

Kilo Code ships `bin/kilo`, a ~167 MB binary that is opencode under the hood
(its help banner prints `opencode`). Relevant subcommands:

```
kilo serve      starts a headless kilo server
kilo acp        start ACP (Agent Client Protocol) server
kilo remote     enable remote connection for real-time session relay
```

`kilo serve` flags: `--port` (default 0 = random), `--hostname` (default
`127.0.0.1`), `--mdns` (service discovery, defaults hostname to `0.0.0.0`),
`--cors`.

## Authentication

The server refuses to run unsecured:

```
Warning: KILO_SERVER_PASSWORD is not set; server is unsecured.
```

With `KILO_SERVER_PASSWORD` set it answers HTTP Basic:

```
HTTP/1.1 401 Unauthorized
www-authenticate: Basic realm="Secure Area"
```

Any username works; the password is the shared secret. Confirmed working:

```bash
KILO_SERVER_PASSWORD=hunter2 ./bin/kilo serve --port 4096 --hostname 127.0.0.1
curl -u "kilo:hunter2" http://127.0.0.1:4096/doc      # 761 KB OpenAPI spec
```

**This is the password the VSCode extension generates and manages.** It is not
the same secret the watch holds — see `protocol.md`.

## Events we consume

`GET /event` is an SSE stream (`text/event-stream`) carrying ~200 event types.
The plugin sees the same bus through its `event` hook.

**`permission.asked` is what actually fires — not `permission.v2.asked`.**
Both are declared in the OpenAPI schema, and the v2 name is the tempting one,
but a live permission prompt on Kilo 7.4.22 emits the v1 shape. Captured
verbatim from a real `bash` prompt:

```jsonc
{
  "id": "per_0020638b70010OokT5yF5718FJ",
  "sessionID": "ses_ffdf9cdc3ffegilIaGS5r4iI8J",
  "permission": "bash",                 // the action — NOT "action"
  "patterns": ["ls -la"],               // the resource — NOT "resources"
  "metadata": {
    "command": "ls -la",
    "description": "List files in current directory"
  },
  "always": ["ls *"],                   // what "Always" would grant — NOT "save"
  "tool": { "messageID": "msg_…", "callID": "chatcmpl-tool-…" }
}
```

Three details that matter:

- **`always` is a pattern, not the command.** Choosing "always" here approves
  `ls *`, not `ls -la`. The watch must show the pattern, or the user is
  consenting to something broader than what they read.
- **`metadata.description`** is a human sentence and is often better on a
  200px screen than the raw command.
- Field names differ from v2 throughout. Translate from v1.

| Event | Payload | Becomes |
|---|---|---|
| `permission.asked` | above | `perm` envelope |
| `permission.replied` | `{sessionID, requestID, reply}` | **retracts** the envelope |
| `question.asked` | `{id, sessionID, …}` | `ques` envelope |
| `session.idle` | `{sessionID}` | `idle` envelope |
| `session.error` | `{sessionID, error}` | `err` envelope |

`permission.replied` was an unexpected find and is worth handling: it fires
when a prompt is answered *anywhere*, including in the VSCode UI. Without it
the watch keeps asking a question that has already been settled at the desk.

Everything else — `session.next.text.delta`, `message.part.updated`, LSP
diagnostics, PTY traffic — is dropped at the hub.

## Replying

```jsonc
// POST /permission/{requestID}/reply     ← the one we use
{ "reply": "once" | "always" | "reject",  // required
  "message": "…",                          // optional
  "interactive": true }                    // optional

// POST /permission/{requestID}/always-rules
{ "approvedAlways": ["ls *"], "deniedAlways": [] }

// POST /session/{sessionID}/permissions/{permissionID}   ← older variant
{ "response": "once" | "always" | "reject" }
```

Query parameters `directory` and `workspace` are accepted on all three.

## Plugin hooks

Probed against a real permission prompt. Of the candidate hook names:

| Hook | Fires? | Signature |
|---|---|---|
| `event` | **yes** | `({ event })` — the whole bus |
| `tool.execute.before` | **yes** | `({tool, sessionID, callID}, {args})` |
| `permission.ask` | **no** | did not fire |
| `permission.asked` | **no** | did not fire |

So **observe the bus; do not try to intercept the prompt.** `permission.ask`
appears in the binary's strings but is not delivered to plugins in 7.4.22.

`tool.execute.before` does fire and its second argument is mutable — writing
to `output.args` would rewrite the command the agent is about to run. We do
not use it. Rewriting a command out from under a user who is about to approve
it would defeat the entire point of asking.

## Endpoints we call

```
POST /permission/{requestID}/reply                       reply to a permission
POST /permission/{requestID}/always-rules                persist an allow rule
POST /session/{sessionID}/permissions/{permissionID}     per-session variant
POST /api/session/{sessionID}/question/{requestID}/reply answer a question
POST /api/session/{sessionID}/question/{requestID}/reject
POST /session/{sessionID}/abort                          stop a running turn
POST /session/{sessionID}/prompt_async                   send a new prompt
GET  /session                                            list sessions
GET  /session/status
GET  /permission                                         pending permissions
```

`prompt_async` is what dictation feeds into.

## Discovery

> **Superseded.** The primary integration is the plugin channel in
> `plugin.md`, which needs no discovery at all: the plugin runs inside the
> kilo process and already has everything. Everything below is the fallback
> for a standalone `prh` with no plugin installed, and it is Linux-only.
>
> It is also the *less safe* path — it moves a credential that grants shell
> access. Prefer the plugin.

Kilo Code spawns **one server per VSCode window**, each with its own random
port and its own password, so discovery is not optional.

The spawn looks like this — note `--port 0`:

```
~/.vscode-server/extensions/kilocode.kilo-code-7.4.22-linux-x64/bin/kilo serve --port 0
```

Credentials are passed in the environment, and `/proc/<pid>/environ` is
readable by the same UID:

```
KILO_SERVER_PASSWORD=<64 chars>   per-instance; HTTP Basic as kilo:<password>
KILO_PARENT_PID=<pid>             the VSCode extension host that spawned it
KILO_CLIENT=vscode
```

The port is not in the command line — take it from the process's listening
socket (`/proc/<pid>/fd` socket inodes against `/proc/net/tcp`).

Verified end to end against a live instance: environ password → `200` on
`/global/health` → a real session list from `/session`.

### Correlating a server to a window

`KILO_PARENT_PID` is the whole trick. Our extension runs inside the same
window as Kilo Code, so it compares `KILO_PARENT_PID` against its own
`process.pid` and finds *its* server with no ambiguity. It then registers
that upstream with `prh`. N windows produce N registrations; `prh` never has
to guess which server owns which project.

This also keeps the platform-specific part in one place: only the extension
reads `/proc`, so a macOS port changes one file rather than the daemon.

### Consequences

- **Credentials rotate.** A VSCode reload means a new port *and* a new
  password. Treat 401 and connection-refused as "re-discover", not "fail".
- **Upstreams are dynamic.** `prh` cannot take its upstream list from static
  config; it needs a loopback-only admin endpoint for registration.
- **`/proc` is Linux and same-UID.** Blocked under `hidepid=2` mounts.

### Do not trust daemon.json

`~/.local/state/kilo/daemon.json` (mode 0600) looks like the answer:

```jsonc
{ "pid": …, "hostname": "127.0.0.1", "port": 4097,
  "url": "http://127.0.0.1:4097", "username": "kilo",
  "password": "…", "token": "<base64 of kilo:password>", … }
```

It is **single-slot** — one daemon, not a per-window registry — and it goes
stale: when observed it named a dead PID and a port nothing was listening on.
Useful as a fallback hint, wrong as a source of truth.

## Open questions

- **Stability.** None of this is documented or supported. A Kilo upgrade can
  break it. The `kilo` package is deliberately the only place that knows these
  details.

## Reproducing

```bash
KILO=~/.vscode-server/extensions/kilocode.kilo-code-*/bin/kilo
KILO_SERVER_PASSWORD=testpw $KILO serve --port 4098 --hostname 127.0.0.1 &
curl -su kilo:testpw http://127.0.0.1:4098/doc > openapi.json
```
