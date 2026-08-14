package dev.yyewolf.prh

import android.content.Context
import java.util.UUID

/**
 * The reason this app exists.
 *
 * PebbleKit JS is killed with the watchapp, so nothing on the watch can be
 * listening while the app is closed. [wakeWatchApp] is the only mechanism
 * that launches a closed watchapp without the user tapping anything.
 *
 * EVERYTHING HERE IS UNVERIFIED. Classic PebbleKit talks to the official
 * Pebble Android app over an intent surface (com.getpebble.action.*), and the
 * Core Devices app is a rewrite. Run the probe in docs/android-companion.md
 * before writing real code against this class.
 */
class PebbleBridge(private val context: Context) {

    companion object {
        /** Must match watchapp/package.json. */
        val WATCHAPP_UUID: UUID = UUID.fromString("630aaa1e-ad28-4694-950d-a25105a7390b")

        // Message keys are indices assigned by the Pebble build from the
        // messageKeys array in watchapp/package.json.
        // TODO: read the generated mapping rather than hardcoding indices —
        // reordering that array silently breaks the wire otherwise.
        const val KEY_EVENT_ID = 0
        const val KEY_EVENT_TYPE = 1
        const val KEY_PROJECT = 2
        const val KEY_SESSION = 3
        const val KEY_TITLE = 4
        const val KEY_BODY = 5
        const val KEY_CHOICES = 6
        const val KEY_STATUS = 7
        const val KEY_REPLY_ID = 8
        const val KEY_REPLY_ACTION = 9
        const val KEY_REPLY_CHOICE = 10
        const val KEY_REPLY_TEXT = 11

        const val CHOICE_SEPARATOR = '\u001F'
    }

    /** Whether a watch is currently connected. */
    fun isConnected(): Boolean = throw NotImplementedError("isConnected")

    /**
     * Launches the watchapp.
     *
     * TODO: implement with PebbleKit.startAppOnPebble(context, WATCHAPP_UUID).
     * Launching is asynchronous and there is no completion callback, so the
     * envelope send needs a delay or a readiness handshake from the watchapp.
     */
    fun wakeWatchApp() {
        throw NotImplementedError("wakeWatchApp")
    }

    /**
     * Pushes an envelope to the watch.
     *
     * TODO: implement with PebbleKit.sendDataToPebble. Keep the dictionary
     * under ~1 KB: the negotiated AppMessage inbox is small and an oversized
     * dict is rejected outright rather than truncated.
     */
    fun send(envelope: Envelope) {
        throw NotImplementedError("send")
    }

    /**
     * Registers the handler for replies coming back from the watch.
     *
     * TODO: implement with PebbleKit.registerReceivedDataHandler, and ack
     * every message — the watchapp waits on the ack to clear its pending
     * state.
     */
    fun onReply(handler: (Reply) -> Unit) {
        throw NotImplementedError("onReply")
    }
}
