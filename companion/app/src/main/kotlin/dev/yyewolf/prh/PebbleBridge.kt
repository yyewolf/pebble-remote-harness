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
 * Compatibility is **verified** against coredevices.coreapp 1.8.0.7: the
 * classic com.getpebble.action.* surface is registered at runtime (so it is
 * absent from the app's manifest but present in its dex), and the classic
 * content provider answers live queries. See docs/android-companion.md.
 *
 * Use classic, not PebbleKit 2: only classic exposes app.START, and launching
 * a closed watchapp is the one capability this project cannot do without.
 */
class PebbleBridge(private val context: Context) {

    companion object {
        /** Must match watchapp/package.json. */
        val WATCHAPP_UUID: UUID = UUID.fromString("630aaa1e-ad28-4694-950d-a25105a7390b")

        // Message keys are allocated by the Pebble build from the messageKeys
        // array in watchapp/package.json, starting at 10000 — NOT at 0.
        // Observed on the wire: a STATUS push arrived as key=10007, matching
        // watchapp/build/js/message_keys.json.
        //
        // Getting this wrong fails silently: the watchapp finds none of the
        // keys it is looking for and simply ignores the message.
        //
        // TODO: generate these from message_keys.json at build time.
        // Reordering the array in package.json renumbers everything.
        const val KEY_EVENT_ID = 10000
        const val KEY_EVENT_TYPE = 10001
        const val KEY_PROJECT = 10002
        const val KEY_SESSION = 10003
        const val KEY_TITLE = 10004
        const val KEY_BODY = 10005
        const val KEY_CHOICES = 10006
        const val KEY_STATUS = 10007
        const val KEY_REPLY_ID = 10008
        const val KEY_REPLY_ACTION = 10009
        const val KEY_REPLY_CHOICE = 10010
        const val KEY_REPLY_TEXT = 10011

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
