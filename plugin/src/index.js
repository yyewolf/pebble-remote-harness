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

const PROTOCOL = "v1"
const PLUGIN_VERSION = "0.1.0"

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
  }

  // TODO: implement. POST /plugin/v1/hello with { protocol, plugin_version,
  // kilo_version, directory, project_id, parent_pid: process.env.KILO_PARENT_PID }.
  // A 409 means protocol mismatch: log once and stay disabled rather than
  // guessing at a format we do not understand.
  async function hello() {
    return null
  }

  // TODO: implement. Fire-and-forget POST /plugin/v1/events with a bounded
  // queue that drops on overflow. Never await this on the agent's path: a
  // slow prh must not become a stalled turn.
  function report(event) {}

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
            // TODO: record in state.pending, then report()
            break

          case "session.idle":
          case "session.error":
            // TODO: report() — notification only, no reply expected
            break
        }
      } catch {
        // Swallowed on purpose. See rule 1.
      }
    },
  }
}

export default PrhPlugin
