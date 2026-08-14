package dev.yyewolf.prh

import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import com.getpebble.android.kit.PebbleKit
import com.getpebble.android.kit.util.PebbleDictionary
import com.getpebble.android.kit.Constants
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

    private var receiver: PebbleKit.PebbleDataReceiver? = null

    /** Whether a watch is currently connected. */
    fun isConnected(): Boolean =
        PebbleKit.isWatchConnected(context)

    /**
     * Launches the watchapp.
     *
     * Launching is asynchronous and there is no completion callback, so the
     * envelope send needs a delay or a readiness handshake from the watchapp.
     */
    fun wakeWatchApp() {
        PebbleKit.startAppOnPebble(context, WATCHAPP_UUID)
    }

    /**
     * Pushes an envelope to the watch.
     *
     * Keeps the dictionary under ~1 KB: the negotiated AppMessage inbox is
     * small and an oversized dict is rejected outright rather than truncated.
     */
    fun send(envelope: Envelope) {
        val dict = PebbleDictionary()
        dict.addString(KEY_EVENT_ID, envelope.id)
        dict.addUint8(KEY_EVENT_TYPE, envelope.type.wire.toByte())
        dict.addString(KEY_PROJECT, envelope.project)
        dict.addString(KEY_SESSION, envelope.session)
        dict.addString(KEY_TITLE, envelope.title)
        dict.addString(KEY_BODY, envelope.body)
        if (envelope.choices.isNotEmpty()) {
            dict.addString(KEY_CHOICES, envelope.choices.joinToString(CHOICE_SEPARATOR.toString()))
        } else {
            dict.addString(KEY_CHOICES, "")
        }
        PebbleKit.sendDataToPebble(context, WATCHAPP_UUID, dict)
    }

    /**
     * Registers the handler for replies coming back from the watch.
     *
     * Registers the BroadcastReceiver manually with RECEIVER_NOT_EXPORTED on
     * API 34+, because PebbleKit's own registerReceivedDataHandler does not
     * pass the flag and crashes on Android 14+.
     */
    fun onReply(handler: (Reply) -> Unit) {
        if (receiver != null) return

        receiver = object : PebbleKit.PebbleDataReceiver(WATCHAPP_UUID) {
            override fun receiveData(ctx: Context, transactionId: Int, dict: PebbleDictionary) {
                val id = dict.getString(KEY_REPLY_ID) ?: return
                val actionWire = dict.getInteger(KEY_REPLY_ACTION)?.toInt() ?: return
                val action = ReplyAction.fromWire(actionWire) ?: return

                val choice = dict.getInteger(KEY_REPLY_CHOICE)?.toInt() ?: 0
                val text = dict.getString(KEY_REPLY_TEXT) ?: ""
                handler(Reply(eventId = id, action = action, choice = choice, text = text))

                PebbleKit.sendAckToPebble(ctx, transactionId)
            }
        }

        val filter = IntentFilter()
        filter.addAction("com.getpebble.android.app.RECEIVE")
        filter.addDataScheme("pebble")
        filter.addDataAuthority(WATCHAPP_UUID.toString(), null)

        if (android.os.Build.VERSION.SDK_INT >= android.os.Build.VERSION_CODES.TIRAMISU) {
            context.registerReceiver(receiver, filter, Context.RECEIVER_NOT_EXPORTED)
        } else {
            context.registerReceiver(receiver, filter)
        }
    }

    /** Unregisters the reply receiver. Call when the service is destroyed. */
    fun shutdown() {
        receiver?.let {
            try { context.unregisterReceiver(it) } catch (e: Exception) { /* already unregistered */ }
            receiver = null
        }
    }
}
