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

  // Fail open: no daemon, no harness, no noise. Not an error, not a toast.
  // The user may simply not be running prh today.
  if (!existsSync(sock)) return {}

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

  // Fire-and-forget POST /plugin/v1/events with a bounded queue that drops
  // on overflow. Never await this on the agent's path: a slow prh must not
  // become a stalled turn.
  function report(event) {
    if (!state.upstreamId) return
    if (state.queue.length >= QUEUE_MAX) {
      // Drop the oldest to make room; newest events are more relevant.
      state.queue.shift()
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
        if (state.queue.length < 8) {
          state.queue.unshift(...batch.slice(0, 8 - state.queue.length))
        }
      })
      .finally(() => {
        flushing = false
        if (state.queue.length > 0) flush()
      })
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

    state.seenNonces.add(decision.nonce)
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

  // Long-poll GET /plugin/v1/decisions, apply each decision, POST
  // /plugin/v1/ack with the result, reconnect with backoff. Verified in
  // Kilo 7.4.22: a plugin can hold a background loop that keeps running
  // after load, which is what this needs.
  async function downlink() {
    let backoff = 1000

    while (!state.stopped) {
      try {
        const { status, json } = await get(
          `/plugin/v1/decisions?upstream_id=${state.upstreamId}&cursor=${state.cursor}&wait=30`,
        )
        if (status !== 200 || !json) {
          backoff = Math.min(backoff * 2, 30000)
          await sleep(backoff)
          continue
        }

        backoff = 1000 // reset on success

        for (const dec of json.decisions || []) {
          const result = await apply(dec)
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

  await hello()
  if (state.upstreamId) downlink()

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
        }
      } catch {
        // Swallowed on purpose. See rule 1.
      }
    },
  }
}

export default PrhPlugin
