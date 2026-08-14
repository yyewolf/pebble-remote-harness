package dev.yyewolf.prh

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.coroutines.delay
import org.json.JSONObject
import org.json.JSONArray
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL

/**
 * HTTP client for the prh daemon. See docs/protocol.md.
 *
 * Uses [HttpURLConnection] to avoid pulling OkHttp as a dependency. The
 * long-poll is the only demanding part and it needs nothing beyond a long
 * read timeout and careful retry.
 */
class PrhClient(
    private val baseUrl: String,
    private var token: String? = null,
) {

    class CursorTooOld : Exception("cursor fell out of the server's ring buffer")

    /** Trades the pairing password for a device token. */
    suspend fun register(password: String, deviceName: String): String = withContext(Dispatchers.IO) {
        val body = JSONObject().apply {
            put("password", password)
            put("device_name", deviceName)
            put("platform", "android")
        }
        val resp = doJson("POST", "/v1/register", body.toString())
        val token = resp.optString("token", "")
        if (token.isEmpty()) throw IOException("register: no token in response")
        this@PrhClient.token = token
        token
    }

    /**
     * Long-polls GET /v1/poll, blocking up to [waitSeconds].
     *
     * An empty list with an unchanged cursor means timeout; poll again
     * immediately with the same cursor. HTTP 410 means the cursor was
     * evicted: throw [CursorTooOld] so the caller resets.
     */
    suspend fun poll(cursor: Long, waitSeconds: Int = 55): Pair<Long, List<Envelope>> = withContext(Dispatchers.IO) {
        val conn = openConn("GET", "/v1/poll?cursor=$cursor&wait=$waitSeconds")
        conn.readTimeout = (waitSeconds + 10) * 1000
        conn.connectTimeout = 10_000
        try {
            val code = conn.responseCode
            when (code) {
                200 -> {
                    val resp = readJson(conn)
                    val newCursor = resp.optLong("cursor", cursor)
                    val arr = resp.optJSONArray("events") ?: JSONArray()
                    val events = (0 until arr.length()).map { i ->
                        val o = arr.getJSONObject(i)
                        Envelope(
                            id = o.getString("id"),
                            seq = o.optLong("seq", 0),
                            type = EventType.fromSlug(o.getString("type")),
                            project = o.optString("project", ""),
                            session = o.optString("session", ""),
                            title = o.optString("title", ""),
                            body = o.optString("body", ""),
                            choices = o.optJSONArray("choices")?.let { ca ->
                                (0 until ca.length()).map { ci -> ca.getString(ci) }
                            } ?: emptyList(),
                            expires = o.optLong("expires", 0),
                        )
                    }
                    newCursor to events
                }
                410 -> throw CursorTooOld()
                401 -> throw IOException("auth failed (401)")
                else -> throw IOException("poll failed: $code")
            }
        } finally {
            conn.disconnect()
        }
    }

    /**
     * Answers a prompt via POST /v1/reply.
     *
     * Retries on network failure — this retry is what guarantees delivery,
     * since the watch does not retry. HTTP 409 means already-answered and
     * must not be retried.
     */
    suspend fun reply(reply: Reply): Unit = withContext(Dispatchers.IO) {
        val body = JSONObject().apply {
            put("event_id", reply.eventId)
            put("action", reply.action.slug)
            if (reply.action == ReplyAction.CHOICE) put("choice", reply.choice)
            if (reply.action == ReplyAction.TEXT) put("text", reply.text)
        }
        val maxAttempts = 5
        var attempt = 0
        while (true) {
            attempt++
            try {
                val resp = doJson("POST", "/v1/reply", body.toString())
                return@withContext
            } catch (e: HttpException) {
                if (e.code == 409) return@withContext
                if (e.code == 401) throw IOException("auth failed", e)
                throw e
            } catch (e: IOException) {
                if (attempt >= maxAttempts) throw e
                delay(minOf(2000L * (1 shl (attempt - 1)), 30_000L))
            }
        }
    }

    /** Sends dictated text as new work. POST /v1/prompt. */
    suspend fun prompt(session: String, text: String): Unit = withContext(Dispatchers.IO) {
        val body = JSONObject().apply {
            put("session", session)
            put("text", text)
        }
        try {
            doJson("POST", "/v1/prompt", body.toString())
        } catch (e: HttpException) {
            if (e.code == 501) return@withContext
            throw e
        }
    }

    /** Unauthenticated liveness probe, used by the settings screen. */
    suspend fun health(): Boolean = withContext(Dispatchers.IO) {
        try {
            openConn("GET", "/v1/health").use { it.responseCode == 200 }
        } catch (e: IOException) {
            false
        }
    }

    // -- internals ----------------------------------------------------------

    private class HttpException(val code: Int, message: String) : IOException(message)

    private fun openConn(method: String, path: String): HttpURLConnection {
        val url = URL(baseUrl.trimEnd('/') + path)
        val conn = url.openConnection() as HttpURLConnection
        conn.requestMethod = method
        conn.setRequestProperty("Accept", "application/json")
        token?.let { conn.setRequestProperty("Authorization", "Bearer $it") }
        if (method == "POST") {
            conn.setRequestProperty("Content-Type", "application/json")
            conn.doOutput = true
        }
        return conn
    }

    private fun doJson(method: String, path: String, body: String = ""): JSONObject {
        val conn = openConn(method, path)
        conn.readTimeout = 15_000
        conn.connectTimeout = 10_000
        try {
            if (body.isNotEmpty()) {
                conn.outputStream.use { it.write(body.toByteArray()) }
            }
            val code = conn.responseCode
            if (code in 200..299) {
                return readJson(conn)
            }
            val errBody = conn.errorStream?.bufferedReader()?.use { it.readText() } ?: ""
            throw HttpException(code, "HTTP $code: $errBody")
        } finally {
            conn.disconnect()
        }
    }

    private fun readJson(conn: HttpURLConnection): JSONObject {
        val text = conn.inputStream.bufferedReader().use { it.readText() }
        return if (text.isNotEmpty()) JSONObject(text) else JSONObject()
    }
}
