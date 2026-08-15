// Kilo plugin for the Pebble Remote Harness.
//
// Runs inside every kilo server. Reports permission prompts to prh over a
// unix socket and applies the decisions that come back.
//
// This file holds credentials that grant shell access through the agent and
// it can approve commands, so two rules dominate everything else:
//
//   1. Fail open. A bug here must never break someone's coding session.
//   2. Only ever apply a decision for a request this plugin forwarded.
//
// Protocol: docs/protocol.md (Hop 0). Threat model: docs/plugin.md.

import { existsSync } from "fs"
import { homedir } from "os"
import { join } from "path"
import http from "http"

const PROTOCOL = "v1"
const PLUGIN_VERSION = "0.1.0"

// Bounded queue for the fire-and-forget uplink. Dropping on overflow is
// correct: a slow prh must never block a coding turn.
const QUEUE_MAX = 64

// Applied-decision nonces retained for idempotency. Large enough that a
// redelivery can never outlive its entry in practice, small enough that a
// window open for days does not accumulate a set without bound.
const MAX_SEEN_NONCES = 512

// messageID -> role entries retained. Bounds the same way.
const MAX_MESSAGE_ROLES = 256

/**
 * Mirrors config.DefaultSocketPath in api/internal/config. The plugin is
 * loaded by Kilo, not by us, so it cannot be passed the path — both sides
 * compute it, and changing one means changing the other.
 */
function socketPath() {
  const runtime = process.env.XDG_RUNTIME_DIR
  if (runtime) return join(runtime, "prh", "plugin.sock")
  return join(homedir(), ".local", "state", "prh", "plugin.sock")
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms))
}

export const PrhPlugin = async ({ client, directory, project, serverUrl }) => {
  const sock = socketPath()

  // Every request to prh goes over the unix socket; it is the only transport.
  //
  // `socketPath` is set on each request rather than on a shared http.Agent.
  // Kilo runs on Bun, and Bun's http.Agent ignores `socketPath` — it dials
  // localhost:80 instead and the plugin dies with ECONNREFUSED against a
  // socket that is demonstrably alive. Per-request `socketPath` is honoured by
  // both runtimes. Do not "simplify" this back into an Agent.
  const post = (path, body) => request("POST", path, body)
  const get = (path) => request("GET", path, null)

  function request(method, path, body) {
    return new Promise((resolve, reject) => {
      const data = body === null ? null : Buffer.from(JSON.stringify(body))
      const opts = { socketPath: sock, path, method, headers: {} }
      if (data) {
        opts.headers["content-type"] = "application/json"
        opts.headers["content-length"] = data.length
      }
      const req = http.request(
        opts,
        (res) => {
          let buf = ""
          res.on("data", (c) => (buf += c))
          res.on("end", () => {
            let json = null
            try { json = buf ? JSON.parse(buf) : null } catch {}
            resolve({ status: res.statusCode, json })
          })
        },
      )
      req.on("error", reject)
      req.end(data)
    })
  }

  const state = {
    upstreamId: null,
    // Request IDs this plugin forwarded and that are still open. A decision
    // for anything not in here is refused — that is the check that stops prh,
    // or anything impersonating it, from approving something we never asked
    // about.
    pending: new Map(),
    // Nonces already applied. A retried decision must not approve twice.
    seenNonces: new Set(),
    cursor: 0,
    stopped: false,
    // Bounded uplink queue. Drops on overflow; never blocks the agent.
    queue: [],
    // messageID -> role ("user" | "assistant"). message.part.updated carries
    // a part with a messageID but no role, so we track message.updated to
    // know whether a text part is the user's prompt or the agent's reply.
    messageRoles: new Map(),
  }

  async function hello() {
    try {
      const { status, json } = await post("/plugin/v1/hello", {
        protocol: PROTOCOL,
        plugin_version: PLUGIN_VERSION,
        kilo_version: process.env.KILO_VERSION || "unknown",
        directory,
        // `project` is an object — {id, worktree, vcs, time, sandboxes} — not
        // a string. Sending it whole makes prh reject the hello as a malformed
        // body, and the plugin then fails open into permanent silence.
        project_id: (project && project.id) || "",
        parent_pid: process.env.KILO_PARENT_PID ? parseInt(process.env.KILO_PARENT_PID, 10) : 0,
      })
      if (status === 409) {
        // Protocol mismatch: stay disabled. Guessing at a format we do not
        // understand would be worse than silence.
        return null
      }
      if (status !== 200 || !json) return null
      state.upstreamId = json.upstream_id
      return json.upstream_id
    } catch {
      return null // fail open
    }
  }

  // Conversation content. Everything else is a prompt or a lifecycle change
  // that the watch is waiting on, and must never be dropped to make room.
  const isChatter = (event) => event.kind === "message"

  // Fire-and-forget POST /plugin/v1/events with a bounded queue that drops
  // on overflow. Never await this on the agent's path: a slow prh must not
  // become a stalled turn.
  function report(event) {
    if (!state.upstreamId) return

    // Supersede a queued update for the same part. The agent re-sends a part
    // every few tokens and each report carries the *accumulated* text, so an
    // older entry for that part is strictly redundant — collapsing them turns
    // hundreds of queued updates into one and keeps a streaming reply from
    // filling the queue at all.
    if (isChatter(event) && event.msg_part_id) {
      const at = state.queue.findIndex(
        (q) => isChatter(q) && q.msg_part_id === event.msg_part_id,
      )
      if (at >= 0) {
        state.queue[at] = event
        flush()
        return
      }
    }

    if (state.queue.length >= QUEUE_MAX) {
      // Drop the oldest *conversation* event to make room. Dropping blindly
      // by age would let a streaming reply evict the permission request behind
      // it — the prompt would never reach prh, and the watch would never buzz
      // for the approval this whole project exists to deliver.
      const at = state.queue.findIndex(isChatter)
      if (at >= 0) {
        state.queue.splice(at, 1)
      } else {
        state.queue.shift()
      }
    }
    state.queue.push(event)
    flush()
  }

  let flushing = false
  function flush() {
    if (flushing || state.queue.length === 0) return
    flushing = true
    const batch = state.queue.splice(0, state.queue.length)
    post("/plugin/v1/events", { upstream_id: state.upstreamId, events: batch })
      .catch(() => {
        // Fail open: re-queue up to a handful, drop the rest. A dead prh
        // should not accumulate an unbounded backlog.
        //
        // Prompts go back before conversation: if only a few events survive a
        // failed flush, they should be the ones somebody is waiting on.
        const room = 8 - state.queue.length
        if (room > 0) {
          const keep = batch
            .filter((e) => !isChatter(e))
            .concat(batch.filter(isChatter))
            .slice(0, room)
          state.queue.unshift(...keep)
        }
      })
      .finally(() => {
        flushing = false
        if (state.queue.length > 0) flush()
      })
  }

  /**
   * Records a nonce as applied, bounding the set so a window left open for
   * days does not grow it without limit. Insertion order is oldest-first, so
   * the first key is the one to drop.
   */
  function rememberNonce(nonce) {
    state.seenNonces.add(nonce)
    if (state.seenNonces.size > MAX_SEEN_NONCES) {
      state.seenNonces.delete(state.seenNonces.values().next().value)
    }
  }

  /**
   * Renders a part as the text the phone shows.
   *
   * Only text and reasoning parts carry `.text`. A tool part carries `tool`
   * and a `state` whose shape depends on status — reading `part.text` off one
   * yields undefined, which is why tool calls showed up in the conversation as
   * an empty row with a label and nothing else. `state.title` is Kilo's own
   * one-line summary (the command, the file path); the input is the fallback
   * for a call that has not produced a title yet.
   */
  function partText(part) {
    if (typeof part.text === "string" && part.text !== "") return part.text

    if (part.type === "tool") {
      const st = part.state || {}
      const label = st.title || part.tool || "tool"
      if (st.status === "error" && st.error) return `${label}\n${st.error}`
      if (st.status === "completed" && st.output) return `${label}\n${st.output}`
      if (st.status === "running" || st.status === "pending") return `${label}…`
      return label
    }

    // step-start, snapshot and friends have nothing to show. Returning "" lets
    // prh keep the part as a positional marker without rendering a blank row.
    return ""
  }

  /**
   * Applies one decision from prh.
   *
   * Every rejection below is deliberate. Loosening any of them turns a
   * compromised prh into an approval oracle.
   */
  async function apply(decision) {
    const req = state.pending.get(decision.request_id)
    if (!req) return "unknown_request" // never asked about this
    if (state.seenNonces.has(decision.nonce)) return "applied" // idempotent retry
    if (decision.expires && Date.now() / 1000 > decision.expires) {
      return "expired" // a timeout leaves the prompt pending, never approves
    }

    rememberNonce(decision.nonce)
    state.pending.delete(decision.request_id)

    // Apply via `client`. There is deliberately no branch that calls
    // permission/allow-everything — it is not in the vocabulary and must not
    // become reachable by adding an action here.
    try {
      switch (decision.action) {
        case "once":
        case "always":
        case "reject": {
          // POST /session/{id}/permissions/{permissionID}
          //
          // `client` is the *v1* KiloClient, whose sole permission method is
          // this generated name. There is no `permissionReply` — calling one
          // throws a TypeError that the catch below swallows, so the ack says
          // "rejected", nothing is logged anywhere, and the prompt hangs
          // pending forever. That was a real bug; do not rename this to
          // something friendlier without checking @kilocode/sdk first.
          //
          // "always" persists the broader pattern via always-rules in a
          // future iteration; for now the reply itself is what the agent
          // sees.
          const res = await client.postSessionIdPermissionsPermissionId({
            path: { id: req.sessionID, permissionID: decision.request_id },
            body: { response: decision.action },
          })
          // hey-api clients resolve with {data, error} instead of throwing,
          // so a 400/404 arrives here rather than in the catch.
          if (res && res.error) return "rejected"
          return "applied"
        }

        case "choice":
        case "text":
          // Not wired, and deliberately not faked. Question replies live on
          // POST /question/{requestID}/reply, which exists only on the *v2*
          // client; the v1 client the plugin is handed has no question API at
          // all. Its body is `answers: string[][]` — one list of chosen option
          // strings per question — which cannot be built from the bare integer
          // `choice` this decision carries. Wiring it up means first
          // forwarding QuestionInfo.options in the question.asked report
          // above, then answering with strings rather than an index.
          //
          // Until then this refuses rather than throwing: the question stays
          // pending in Kilo, which is what an unanswered question does
          // without the harness. Fail open.
          return "rejected"

        default:
          return "rejected"
      }
    } catch {
      // A failed apply does not get un-deleted from pending: prh will not
      // re-send (the cursor advanced), and the prompt stays pending in Kilo
      // exactly as it would without the harness. Fail open.
      return "rejected"
    }
  }

  /**
   * Applies a prompt decision: sends text into a session as a new turn.
   *
   * This is the "reply in sessions" affordance. The phone sends text; prh
   * queues a kind:"prompt" decision; the plugin calls Kilo's prompt_async,
   * which starts a new turn in that session. prh never holds credentials, so
   * it cannot send the text itself — same inversion as permission replies.
   *
   * `client.promptAsync` is on the v1 SDK the plugin is handed. Its body
   * takes `parts: [{ type: "text", text }]`; we send exactly one text part.
   * Returns 204 on success.
   */
  async function applyPrompt(decision) {
    if (!decision.session_id || !decision.text) return "rejected"

    // Same idempotency guard the permission path has. Without it a redelivered
    // decision — a dropped ack, a poll that resumed from a stale cursor —
    // injects the user's message into the session a second time, and an agent
    // that has already started acting on it begins again.
    if (state.seenNonces.has(decision.nonce)) return "applied"
    rememberNonce(decision.nonce)

    try {
      // `client.session.promptAsync`, not `client.promptAsync`.
      //
      // The v1 KiloClient exposes exactly one method at the top level
      // (postSessionIdPermissionsPermissionId); everything else hangs off a
      // namespace — session, config, file, and so on. Calling the bare name
      // throws a TypeError, which the catch below turns into "rejected": the
      // ack says the plugin refused, nothing is logged, and the message the
      // user typed on their phone disappears without a trace. That is the
      // exact failure the permission path documents above; do not re-flatten
      // this call without checking @kilocode/sdk first.
      const res = await client.session.promptAsync({
        path: { id: decision.session_id },
        body: { parts: [{ type: "text", text: decision.text }] },
      })
      // hey-api clients resolve with {data, error} rather than throwing, so a
      // 404 for a deleted session arrives here, not in the catch.
      if (res && res.error) return "rejected"
      return "applied"
    } catch {
      return "rejected"
    }
  }

  // Long-poll GET /plugin/v1/decisions, apply each decision, POST
  // /plugin/v1/ack with the result, reconnect with backoff. Verified in
  // Kilo 7.4.22: a plugin can hold a background loop that keeps running
  // after load, which is what this needs.
  async function downlink() {
    let backoff = 1000

    while (!state.stopped) {
      // Registration lives inside the loop, not once before it.
      //
      // Fail open: no socket, no daemon, no harness, no noise — the user may
      // simply not be running prh today. But both checks belong here rather
      // than at load, because prh deletes its socket on a clean exit and
      // recreates it on start. Gating at load meant every Kilo instance that
      // happened to start before prh stayed dead for its entire life, and
      // since nothing logs a plugin load, that looks exactly like a plugin
      // that was never installed. The cost of retrying is one stat() and at
      // most one connect() per backoff tick.
      if (!state.upstreamId) {
        if (!existsSync(sock) || !(await hello())) {
          backoff = Math.min(backoff * 2, 30000)
          await sleep(backoff)
          continue
        }
        // A fresh upstream has a fresh decision ring; keeping the old cursor
        // would skip past everything queued for us.
        state.cursor = 0
        backoff = 1000
      }

      try {
        const { status, json } = await get(
          `/plugin/v1/decisions?upstream_id=${state.upstreamId}&cursor=${state.cursor}&wait=30`,
        )
        // prh restarted and has forgotten this upstream: 400 unknown
        // upstream_id, or 410 once it is dropped mid-poll. Re-register rather
        // than backing off against an ID that will never become valid again.
        if (status === 400 || status === 410) {
          state.upstreamId = null
          continue
        }
        if (status !== 200 || !json) {
          backoff = Math.min(backoff * 2, 30000)
          await sleep(backoff)
          continue
        }

        backoff = 1000 // reset on success

        for (const dec of json.decisions || []) {
          let result
          if (dec.kind === "prompt") {
            result = await applyPrompt(dec)
          } else {
            result = await apply(dec)
          }
          await post("/plugin/v1/ack", {
            upstream_id: state.upstreamId,
            id: dec.id,
            status: result,
          }).catch(() => {})
        }

        state.cursor = json.cursor || state.cursor
      } catch {
        backoff = Math.min(backoff * 2, 30000)
        await sleep(backoff)
      }
    }
  }

  // Not awaited: downlink() now owns registration and retries forever, so
  // awaiting it would block plugin load until prh happened to be up.
  downlink()

  return {
    /**
     * The only hook we use.
     *
     * Probed against a real prompt on Kilo 7.4.22: `permission.ask` never
     * fires, so there is nothing to intercept — we observe the bus instead.
     * `tool.execute.before` does fire and can rewrite the command about to
     * run; we deliberately do not use it. Rewriting a command out from under
     * someone who is about to approve it defeats the point of asking.
     *
     * Wrapped so a throw can never escape into Kilo.
     */
    event: async ({ event }) => {
      try {
        if (state.stopped || !state.upstreamId) return
        const p = event.properties || {}

        switch (event.type) {
          // v1 is what actually fires. Field names are permission/patterns/
          // always — NOT the action/resources/save of the v2 schema.
          case "permission.asked":
            state.pending.set(p.id, { sessionID: p.sessionID })
            report({
              kind: "permission",
              request_id: p.id,
              session_id: p.sessionID,
              action: p.permission,
              resources: p.patterns,
              description: p.metadata?.description,
              // Broader than the command: "ls -la" approved with always
              // grants "ls *". prh shows it on the choice.
              always: p.always,
            })
            break

          // Fires when a prompt is answered anywhere, including the VSCode
          // UI. Retracts it from the watch instead of asking twice.
          case "permission.replied":
            state.pending.delete(p.requestID)
            report({
              kind: "replied",
              request_id: p.requestID,
              session_id: p.sessionID,
            })
            break

          case "question.asked":
            state.pending.set(p.id, { sessionID: p.sessionID })
            report({
              kind: "question",
              request_id: p.id,
              session_id: p.sessionID,
            })
            break

          case "session.idle":
            report({
              kind: "idle",
              session_id: p.sessionID,
            })
            break

          case "session.error":
            report({
              kind: "error",
              session_id: p.sessionID,
              description: p.error,
            })
            break

          // Conversation content. The agent or user is saying something; the
          // phone shows it for context. message.part.updated carries the
          // accumulated part text in `part.text` plus an optional `delta`; we
          // forward the whole part so the phone's view is always current
          // without a streaming protocol.
          case "message.part.updated": {
            const part = p.part || {}
            if (!part.sessionID || !part.id) break
            const text = partText(part)
            // A part with nothing to show is not worth a round trip. Dropping
            // it here keeps step-start markers and empty tool stubs out of the
            // phone's view and off the uplink entirely.
            if (text === "") break
            const role = state.messageRoles.get(part.messageID) || "assistant"
            report({
              kind: "message",
              session_id: part.sessionID,
              msg_role: role,
              msg_part_id: part.id,
              msg_text: text,
              msg_kind: part.type || "text",
              msg_time_ms: (part.time && (part.time.start || part.time.end)) || Date.now(),
            })
            break
          }

          // A part the agent retracted. Reported as an empty message so prh
          // replaces the entry rather than leaving output on the phone that
          // the session no longer contains.
          case "message.part.removed": {
            if (!p.sessionID || !p.partID) break
            report({
              kind: "message",
              session_id: p.sessionID,
              msg_part_id: p.partID,
              msg_text: "",
              msg_kind: "removed",
              msg_time_ms: Date.now(),
            })
            break
          }

          // Track messageID -> role so message.part.updated knows whether a
          // part is the user's prompt or the agent's reply. message.updated
          // carries the full Message struct with role and id.
          case "message.updated": {
            const info = p.info || {}
            if (info.id) {
              state.messageRoles.set(info.id, info.role || "assistant")
              // Bound the map so long sessions don't grow it forever.
              if (state.messageRoles.size > MAX_MESSAGE_ROLES) {
                const firstKey = state.messageRoles.keys().next().value
                state.messageRoles.delete(firstKey)
              }
            }
            break
          }

          case "message.removed": {
            if (p.messageID) state.messageRoles.delete(p.messageID)
            break
          }

          // Session lifecycle. Lets prh build a session registry from
          // events rather than polling GET /session, so it needs no
          // credentials. The `info` field is Kilo's Session struct.
          case "session.created":
          case "session.updated": {
            const info = p.info || {}
            report({
              kind: event.type, // "session.created" | "session.updated"
              session_id: info.id || "",
              session_title: info.title || "",
              session_dir: info.directory || "",
              session_updated_ms: info.time ? (info.time.updated || Date.now()) : Date.now(),
            })
            break
          }

          case "session.deleted": {
            const info = p.info || {}
            report({
              kind: "session.deleted",
              session_id: info.id || "",
            })
            break
          }

          case "session.status": {
            if (!p.sessionID) break
            const status = p.status || {}
            report({
              kind: "session.status",
              session_id: p.sessionID,
              session_status: status.type || "unknown",
              // Kilo's status event carries no timestamp, but a status change
              // *is* activity: without this the session never moves in a list
              // sorted by recency.
              session_updated_ms: Date.now(),
            })
            break
          }
        }
      } catch {
        // Swallowed on purpose. See rule 1.
      }
    },
  }
}

export default PrhPlugin
