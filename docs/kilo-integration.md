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
Four matter:

| Event | Payload | Becomes |
|---|---|---|
| `permission.v2.asked` | `{id, sessionID, action, resources[], save[], metadata, source}` | `perm` envelope |
| `question.v2.asked` | `{id, sessionID, questions[], tool}` | `ques` envelope |
| `session.idle` | `{sessionID}` | `idle` envelope |
| `session.error` | `{sessionID, error}` | `err` envelope |

Legacy `permission.asked` and `question.asked` also exist with different
shapes (`{id, sessionID, permission, patterns[], metadata, always[], tool}`).
Handle both; prefer v2.

Everything else — `session.next.text.delta`, `message.part.updated`, LSP
diagnostics, PTY traffic — is dropped at the hub.

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

## Open questions

- **Discovery.** How does the extension learn the port of the `kilo serve`
  instance Kilo Code itself spawned? Options: read Kilo's own config/state
  dir, use `--mdns` service discovery, or have `prh` spawn its own dedicated
  instance. Unresolved.
- **Reply body shapes.** The exact request bodies for the reply endpoints are
  in `/doc` but have not been transcribed here. Pull them from the live spec
  before implementing `kilo/client.go`.
- **Stability.** None of this is documented or supported. A Kilo upgrade can
  break it. The `kilo` package is deliberately the only place that knows these
  details.

## Reproducing

```bash
KILO=~/.vscode-server/extensions/kilocode.kilo-code-*/bin/kilo
KILO_SERVER_PASSWORD=testpw $KILO serve --port 4098 --hostname 127.0.0.1 &
curl -su kilo:testpw http://127.0.0.1:4098/doc > openapi.json
```
