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
 *
 * Every request after registration is signed. Two consequences shape this
 * class:
 *
 *  - the body has to be materialised before the request is opened, because it
 *    is part of what gets signed
 *  - a 401 is a *routine* condition, not a failure. It means the session key
 *    is gone — almost always because prh restarted — and the fix is to log in
 *    again and retry once, with no user involvement.
 */
class PrhClient(
    private val baseUrl: String,
    private val deviceId: String? = null,
    private val deviceSecret: String? = null,
) {

    class CursorTooOld : Exception("cursor fell out of the server's ring buffer")

    /**
     * The device is not registered with this prh at all, so no amount of
     * retrying will help — the user has to re-pair with the passphrase. Worth
     * distinguishing from a transient failure so the UI can say something
     * useful instead of retrying forever.
     */
    class NotPaired(message: String) : IOException(message)

    private var sessionKeyId: String? = null
    private var sessionKey: ByteArray? = null

    /**
     * Offset to add to our clock to match the server's, learned from the
     * X-Prh-Time header on a skew rejection.
     *
     * Without this, a phone whose clock has drifted past the leeway fails
     * every request with a 401 that looks exactly like a bad credential, and
     * no amount of re-pairing fixes it.
     */
    private var clockOffsetSec: Long = 0

    val isLoggedIn: Boolean get() = sessionKey != null

    /**
     * Trades the pairing passphrase for a device secret. The only call that
     * carries the passphrase, and the only one that is not signed.
     */
    suspend fun register(password: String, deviceName: String): Pair<String, String> =
        withContext(Dispatchers.IO) {
            val body = JSONObject().apply {
                put("password", password)
                put("device_name", deviceName)
                put("platform", "android")
            }
            val resp = doJson("POST", "/v1/register", body.toString().toByteArray(), null)
            val id = resp.optString("device_id", "")
            val secret = resp.optString("device_secret", "")
            if (id.isEmpty() || secret.isEmpty()) {
                throw IOException("register: no device credentials in response")
            }
            id to secret
        }

    /**
     * Obtains a session key, proving possession of the device secret by
     * signing the request with it.
     *
     * This is what makes a prh restart invisible: the daemon forgets sessions
     * but not devices, so the phone re-establishes one on its own.
     */
    suspend fun login(): Unit = withContext(Dispatchers.IO) {
        val id = deviceId ?: throw NotPaired("no device id")
        val secretB64 = deviceSecret ?: throw NotPaired("no device secret")
        val secret = PrhSigning.decode(secretB64)

        val body = JSONObject().apply { put("device_id", id) }.toString().toByteArray()
        val resp = try {
            doJson("POST", "/v1/login", body, SigningKey(id, secret))
        } catch (e: HttpException) {
            // prh knows nothing about this device: its devices.json was lost
            // or the device was revoked. Only re-pairing fixes that.
            if (e.code == 401) throw NotPaired("prh does not recognise this device; re-pair")
            throw e
        }

        val keyId = resp.optString("key_id", "")
        val key = PrhSigning.unwrapSessionKey(
            deviceSecret = secret,
            wrapSalt = resp.getString("wrap_salt"),
            wrapNonce = resp.getString("wrap_nonce"),
            wrappedKey = resp.getString("wrapped_key"),
            keyId = keyId,
        )
        sessionKeyId = keyId
        sessionKey = key
    }

    /**
     * Confirms the session is still live.
     *
     * Its value is in the failure: a 401 here means prh restarted, and the
     * caller logs in again before a prompt is waiting rather than after.
     */
    suspend fun heartbeat(): Unit = withContext(Dispatchers.IO) {
        val resp = authed("POST", "/v1/heartbeat", null)
        if (resp.code !in 200..299) {
            throw HttpException(resp.code, "heartbeat failed: ${resp.body}")
        }
    }

    /**
     * Long-polls GET /v1/poll, blocking up to [waitSeconds].
     *
     * An empty list with an unchanged cursor means timeout; poll again
     * immediately with the same cursor. HTTP 410 means the cursor was
     * evicted: throw [CursorTooOld] so the caller resets.
     */
    suspend fun poll(cursor: Long, waitSeconds: Int = 55): Pair<Long, List<Envelope>> = withContext(Dispatchers.IO) {
        val path = "/v1/poll?cursor=$cursor&wait=$waitSeconds"
        val resp = authed("GET", path, null, readTimeoutMs = (waitSeconds + 10) * 1000)
        when (resp.code) {
            200 -> {
                val json = JSONObject(resp.body.ifEmpty { "{}" })
                val newCursor = json.optLong("cursor", cursor)
                val arr = json.optJSONArray("events") ?: JSONArray()
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
            else -> throw IOException("poll failed: ${resp.code}")
        }
    }

    /**
     * Answers a prompt via POST /v1/reply.
     *
     * Retries on network failure — this retry is what guarantees delivery,
     * since the watch does not retry. HTTP 409 means already-answered and must
     * not be retried; [authed] has already handled the 401 case by logging in
     * again, so a 401 arriving here is final.
     */
    suspend fun reply(reply: Reply): Unit = withContext(Dispatchers.IO) {
        val body = JSONObject().apply {
            put("event_id", reply.eventId)
            put("action", reply.action.slug)
            if (reply.action == ReplyAction.CHOICE) put("choice", reply.choice)
            if (reply.action == ReplyAction.TEXT) put("text", reply.text)
        }.toString().toByteArray()

        val maxAttempts = 5
        var attempt = 0
        while (true) {
            attempt++
            try {
                val resp = authed("POST", "/v1/reply", body)
                // Already answered, at the desk or by an earlier retry of this
                // very request. Either way the prompt is settled.
                if (resp.code == 409) return@withContext
                if (resp.code !in 200..299) {
                    throw HttpException(resp.code, "reply failed: ${resp.body}")
                }
                return@withContext
            } catch (e: NotPaired) {
                throw e // retrying cannot fix this
            } catch (e: HttpException) {
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
        }.toString().toByteArray()

        val resp = authed("POST", "/v1/prompt", body)
        if (resp.code == 501) return@withContext // not implemented yet, by design
        if (resp.code !in 200..299) throw HttpException(resp.code, "prompt failed: ${resp.body}")
    }

    /** Unauthenticated liveness probe, used by the settings screen. */
    suspend fun health(): Boolean = withContext(Dispatchers.IO) {
        try {
            exec("GET", "/v1/health", null, null, readTimeoutMs = 5_000).code == 200
        } catch (e: IOException) {
            false
        }
    }

    // -- internals ----------------------------------------------------------

    class HttpException(val code: Int, message: String) : IOException(message)

    private class SigningKey(val keyId: String, val key: ByteArray)

    private class Response(val code: Int, val body: String, val serverTime: Long?)

    /**
     * Performs a signed request, recovering from the two failures that are
     * expected rather than exceptional.
     *
     * A 401 means one of:
     *
     *  - we have no session yet, or prh restarted and dropped it — log in and
     *    retry, which is the whole reason the device secret is persisted
     *  - our clock has drifted outside the leeway — the rejection carries the
     *    server's time, so adopt the offset and retry
     *
     * Exactly one retry: if a fresh session signed with a corrected clock is
     * still refused, the problem is not transient and looping would only bury
     * it.
     */
    private suspend fun authed(
        method: String,
        path: String,
        body: ByteArray?,
        readTimeoutMs: Int = 15_000,
    ): Response {
        if (sessionKey == null) {
            login()
        }
        val first = exec(method, path, body, currentKey(), readTimeoutMs)
        if (first.code != 401) return first

        // Adopt the server's clock before retrying: if skew is the problem, a
        // new session signed with the same wrong time fails identically.
        first.serverTime?.let { clockOffsetSec = it - System.currentTimeMillis() / 1000 }
        login()
        return exec(method, path, body, currentKey(), readTimeoutMs)
    }

    private fun currentKey(): SigningKey {
        val id = sessionKeyId ?: throw IOException("no session key id")
        val key = sessionKey ?: throw IOException("no session key")
        return SigningKey(id, key)
    }

    /**
     * One HTTP round trip, signed if a key is supplied.
     *
     * The body is written from a byte array rather than streamed because it
     * has to be hashed into the signature first; there is no way to sign
     * something that has not been produced yet.
     */
    private fun exec(
        method: String,
        path: String,
        body: ByteArray?,
        signing: SigningKey?,
        readTimeoutMs: Int,
    ): Response {
        val conn = URL(baseUrl.trimEnd('/') + path).openConnection() as HttpURLConnection
        conn.requestMethod = method
        conn.setRequestProperty("Accept", "application/json")
        conn.readTimeout = readTimeoutMs
        conn.connectTimeout = 10_000
        if (body != null) {
            conn.setRequestProperty("Content-Type", "application/json")
            conn.doOutput = true
        }

        if (signing != null) {
            val nonce = PrhSigning.newNonce()
            val date = (System.currentTimeMillis() / 1000 + clockOffsetSec).toString()
            conn.setRequestProperty(PrhSigning.HEADER_KEY, signing.keyId)
            conn.setRequestProperty(PrhSigning.HEADER_DATE, date)
            conn.setRequestProperty(PrhSigning.HEADER_NONCE, nonce)
            conn.setRequestProperty(
                PrhSigning.HEADER_SIG,
                PrhSigning.sign(signing.key, PrhSigning.canonical(method, path, date, nonce, body)),
            )
        }

        try {
            if (body != null) {
                conn.outputStream.use { it.write(body) }
            }
            val code = conn.responseCode
            val text = if (code in 200..299) {
                conn.inputStream.bufferedReader().use { it.readText() }
            } else {
                conn.errorStream?.bufferedReader()?.use { it.readText() } ?: ""
            }
            val serverTime = conn.getHeaderField(PrhSigning.HEADER_SERVER_TIME)?.toLongOrNull()
            return Response(code, text, serverTime)
        } finally {
            conn.disconnect()
        }
    }

    /** Unsigned or device-signed JSON call used by register and login. */
    private fun doJson(method: String, path: String, body: ByteArray?, signing: SigningKey?): JSONObject {
        val resp = exec(method, path, body, signing, readTimeoutMs = 15_000)
        if (resp.code !in 200..299) {
            throw HttpException(resp.code, "HTTP ${resp.code}: ${resp.body}")
        }
        return if (resp.body.isNotEmpty()) JSONObject(resp.body) else JSONObject()
    }
}
