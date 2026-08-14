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

export const PrhPlugin = async ({ client, directory, project, serverUrl }) => {
  const sock = socketPath()

  // Fail open: no daemon, no harness, no noise. Not an error, not a toast.
  // The user may simply not be running prh today.
  if (!existsSync(sock)) return {}

  // One HTTP agent per plugin instance, pinned to the unix socket. Every
  // request to prh reuses this; it is the only transport.
  const agent = new http.Agent({ socketPath: sock, maxSockets: 1 })

  function post(path, body) {
    return new Promise((resolve, reject) => {
      const data = Buffer.from(JSON.stringify(body))
      const req = http.request(
        { agent, path, method: "POST", headers: { "content-type": "application/json", "content-length": data.length } },
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

  function get(path) {
    return new Promise((resolve, reject) => {
      const req = http.request({ agent, path, method: "GET" }, (res) => {
        let buf = ""
        res.on("data", (c) => (buf += c))
        res.on("end", () => {
          let json = null
          try { json = buf ? JSON.parse(buf) : null } catch {}
          resolve({ status: res.statusCode, json })
        })
      })
      req.on("error", reject)
      req.end()
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
        project_id: project,
        parent_pid: process.env.KILO_PARENT_PID ? parseInt(process.env.KILO_PARENT_PID, 10) : null,
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

    // TODO: apply via `client`. There is deliberately no branch that calls
    // permission/allow-everything — it is not in the vocabulary and must not
    // become reachable by adding an action here.
    switch (decision.action) {
      case "once":
      case "always":
      case "reject":
      case "choice":
      case "text":
        return "applied"
      default:
        return "rejected"
    }
  }

  // TODO: implement the downlink. Long-poll GET /plugin/v1/decisions, apply
  // each decision, POST /plugin/v1/ack with the result, reconnect with
  // backoff. Verified in Kilo 7.4.22: a plugin can hold a background loop
  // that keeps running after load, which is what this needs.
  async function downlink() {}

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
