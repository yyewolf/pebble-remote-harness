package dev.yyewolf.prh

import android.content.Context
import android.util.Log
import com.getpebble.android.kit.PebbleKit
import com.getpebble.android.kit.util.PebbleDictionary
import java.io.IOException
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.Socket
import java.util.UUID
import java.util.concurrent.Executors

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
 *
 * **Inbound AppMessages**: the Core Devices app does NOT broadcast
 * com.getpebble.action.app.RECEIVE to other apps — it routes inbound
 * AppMessages only to PKJS. So PKJS forwards replies to a local HTTP server
 * on the companion (see [PkjsRelayServer]), which then sends them to prh.
 */
class PebbleBridge(private val context: Context) {

    companion object {
        private const val TAG = "PebbleBridge"

        /** Must match watchapp/package.json. */
        val WATCHAPP_UUID: UUID = UUID.fromString("630aaa1e-ad28-4694-950d-a25105a7390b")

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

        /** The local port PKJS connects to for relaying replies. */
        const val PKJS_RELAY_PORT = 8478
    }

    private var relayServer: PkjsRelayServer? = null

    /** Whether a watch is currently connected. */
    fun isConnected(): Boolean =
        PebbleKit.isWatchConnected(context)

    /**
     * Launches the watchapp.
     */
    fun wakeWatchApp() {
        PebbleKit.startAppOnPebble(context, WATCHAPP_UUID)
    }

    /**
     * Pushes an envelope to the watch.
     *
     * Also sets the companion URL in PKJS localStorage via a STATUS message
     * so PKJS knows where to forward replies.
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
     * Starts the PKJS relay server. PKJS connects here to forward watch
     * replies, since the Core Devices app doesn't broadcast RECEIVE.
     */
    fun startRelay(onReply: (Reply) -> Unit) {
        if (relayServer != null) return
        relayServer = PkjsRelayServer(PKJS_RELAY_PORT, onReply)
        relayServer?.start()
        Log.i(TAG, "PKJS relay server listening on port $PKJS_RELAY_PORT")
    }

    /** Stops the relay server and unregisters any receivers. */
    fun shutdown() {
        relayServer?.stop()
        relayServer = null
    }
}

/**
 * Minimal HTTP server that receives reply forwards from PKJS.
 *
 * PKJS runs inside the Core Devices app and cannot use classic PebbleKit
 * broadcasts to deliver watch replies. Instead it POSTs to this local server.
 */
class PkjsRelayServer(
    private val port: Int,
    private val onReply: (Reply) -> Unit,
) {
    private val executor = Executors.newSingleThreadExecutor()
    private var server: ServerSocket? = null
    private var running = false

    fun start() {
        running = true
        executor.execute {
            try {
                server = ServerSocket()
                server!!.bind(InetSocketAddress("127.0.0.1", port))
                Log.i("PkjsRelay", "listening on 127.0.0.1:$port")
                while (running) {
                    val client = try { server!!.accept() } catch (e: IOException) { break }
                    handle(client)
                }
            } catch (e: Exception) {
                Log.e("PkjsRelay", "server error", e)
            }
        }
    }

    fun stop() {
        running = false
        try { server?.close() } catch (e: Exception) {}
        executor.shutdownNow()
    }

    private fun handle(client: Socket) {
        try {
            val input = client.getInputStream().bufferedReader()
            val output = client.getOutputStream()

            val requestLine = input.readLine() ?: return
            val headers = mutableMapOf<String, String>()
            var line: String?
            while (input.readLine().also { line = it } != null && line!!.isNotEmpty()) {
                val parts = line!!.split(": ", limit = 2)
                if (parts.size == 2) headers[parts[0].lowercase()] = parts[1]
            }

            var body = ""
            val contentLength = headers["content-length"]?.toIntOrNull() ?: 0
            if (contentLength > 0) {
                val buf = CharArray(contentLength)
                input.read(buf, 0, contentLength)
                body = String(buf)
            }

            if (requestLine.startsWith("POST")) {
                val reply = parseReply(body)
                if (reply != null) {
                    onReply(reply)
                    output.write("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}".toByteArray())
                } else {
                    output.write("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n".toByteArray())
                }
            } else if (requestLine.startsWith("GET")) {
                output.write("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}".toByteArray())
            } else {
                output.write("HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n".toByteArray())
            }
            output.flush()
        } catch (e: Exception) {
            Log.e("PkjsRelay", "handle error", e)
        } finally {
            try { client.close() } catch (e: Exception) {}
        }
    }

    private fun parseReply(body: String): Reply? {
        try {
            val json = org.json.JSONObject(body)
            val eventId = json.getString("event_id")
            val actionSlug = json.getString("action")
            val action = ReplyAction.entries.firstOrNull { it.slug == actionSlug } ?: return null
            val choice = json.optInt("choice", 0)
            val text = json.optString("text", "")
            return Reply(eventId, action, choice, text)
        } catch (e: Exception) {
            Log.e("PkjsRelay", "parse error: $body", e)
            return null
        }
    }
}
