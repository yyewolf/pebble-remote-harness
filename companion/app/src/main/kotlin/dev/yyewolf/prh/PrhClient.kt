package dev.yyewolf.prh

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.coroutines.delay
import org.json.JSONObject
import org.json.JSONArray
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import javax.net.ssl.HttpsURLConnection

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
    /**
     * base64url SHA-256 of prh's public key, from the pairing QR.
     *
     * Required for every https base URL. Its absence is not a soft failure to
     * shrug at: without it there is nothing to distinguish prh from anything
     * else answering on that address.
     */
    private val tlsPin: String? = null,
) {

    class CursorTooOld : Exception("cursor fell out of the server's ring buffer")

    /**
     * The device is not registered with this prh at all, so no amount of
     * retrying will help — the user has to re-pair by scanning a fresh code.
     * Worth distinguishing from a transient failure so the UI can say something
     * useful instead of retrying forever.
     */
    class NotPaired(message: String) : IOException(message)

    /**
     * prh is reachable and the code may well be right, but enrolment is not
     * armed. Distinct from a bad key so the UI can say "start pairing in the
     * editor" rather than "wrong code".
     */
    class PairingClosed(message: String) : IOException(message)

    /**
     * prh has no record of that session: it was deleted, or prh restarted and
     * lost its in-memory registry. Distinct from [NotPaired] — the device's
     * credentials are fine — so the conversation view can close itself without
     * telling the user to re-pair.
     */
    class UnknownSession(message: String) : IOException(message)

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
     * Enrols using the one-time pairing key from the QR — the only path.
     *
     * The key is proved by signing rather than sent, and the device secret
     * comes back sealed under it, so the enrolment exchange carries nothing an
     * eavesdropper can use.
     *
     * Returns the device ID and the base64url device secret.
     */
    suspend fun registerWithPairingKey(
        pairingKey: ByteArray,
        deviceName: String,
    ): Pair<String, String> = withContext(Dispatchers.IO) {
        val body = JSONObject().apply {
            put("device_name", deviceName)
            put("platform", "android")
        }.toString().toByteArray()

        val resp = try {
            doJson("POST", "/v1/register", body, SigningKey(PrhSigning.PAIRING_KEY_ID, pairingKey))
        } catch (e: HttpException) {
            // 403 is the window, not the key: prh is up and the code is fine,
            // but nobody armed enrolment or it has already been used.
            if (e.code == 403) {
                throw PairingClosed("pairing is not open on prh; start pairing from the editor")
            }
            throw e
        }

        val id = resp.optString("device_id", "")
        if (id.isEmpty()) throw IOException("register: no device id in response")

        val secret = PrhSigning.unwrapDeviceSecret(
            pairingKey = pairingKey,
            wrapSalt = resp.getString("wrap_salt"),
            wrapNonce = resp.getString("wrap_nonce"),
            wrapSecret = resp.getString("wrap_secret"),
            deviceId = id,
        )
        id to PrhSigning.encode(secret)
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
                        msgRole = o.optString("msg_role", ""),
                        msgPartID = o.optString("msg_part_id", ""),
                        msgText = o.optString("msg_text", ""),
                        msgKind = o.optString("msg_kind", ""),
                        msgTime = o.optLong("msg_time", 0),
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

    /**
     * Lists every known session. GET /v1/sessions.
     *
     * Built by prh from events, not by calling Kilo, so this needs no
     * credentials beyond the device's signing key. The [hasPrompt] flag is
     * what the session list badges — it is set when a perm/ques envelope for
     * that session is still pending.
     */
    suspend fun sessions(): List<SessionSummary> = withContext(Dispatchers.IO) {
        val resp = authed("GET", "/v1/sessions", null)
        if (resp.code !in 200..299) {
            throw HttpException(resp.code, "sessions failed: ${resp.body}")
        }
        val json = JSONObject(resp.body.ifEmpty { "{}" })
        val arr = json.optJSONArray("sessions") ?: JSONArray()
        (0 until arr.length()).map { i ->
            val o = arr.getJSONObject(i)
            SessionSummary(
                id = o.getString("id"),
                project = o.optString("project", ""),
                title = o.optString("title", ""),
                dir = o.optString("dir", ""),
                status = o.optString("status", "unknown"),
                updated = o.optLong("updated", 0),
                hasPrompt = o.optBoolean("has_prompt", false),
                promptId = o.optString("prompt_id", ""),
                promptType = o.optString("prompt_type", ""),
            )
        }
    }

    /**
     * Long-polls a session's conversation. GET /v1/sessions/{id}/conversation.
     *
     * Returns the per-session cursor and the messages newer than it. The
     * cursor is the seq of the last message the caller has seen, so a
     * follow-up call fetches only what arrived since.
     */
    suspend fun conversation(
        sessionId: String,
        cursor: Long = 0,
        waitSeconds: Int = 55,
    ): Pair<Long, List<Envelope>> = withContext(Dispatchers.IO) {
        val path = "/v1/sessions/$sessionId/conversation?cursor=$cursor&wait=$waitSeconds"
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
                        msgRole = o.optString("msg_role", ""),
                        msgPartID = o.optString("msg_part_id", ""),
                        msgText = o.optString("msg_text", ""),
                        msgKind = o.optString("msg_kind", ""),
                        msgTime = o.optLong("msg_time", 0),
                    )
                }
                newCursor to events
            }
            404 -> throw UnknownSession("session gone or prh restarted")
            else -> throw IOException("conversation failed: ${resp.code}")
        }
    }

    /**
     * Sends text into a session — the "reply in sessions" affordance.
     * POST /v1/sessions/{id}/prompt.
     *
     * Routes to a kind:"prompt" decision the plugin applies via Kilo's
     * prompt_async. prh never holds credentials, so it cannot send the text
     * itself.
     */
    suspend fun sessionPrompt(sessionId: String, text: String): Unit = withContext(Dispatchers.IO) {
        val body = JSONObject().apply {
            put("text", text)
        }.toString().toByteArray()

        val resp = authed("POST", "/v1/sessions/$sessionId/prompt", body)
        if (resp.code == 404) throw UnknownSession("session gone or prh restarted")
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
        // execSigned has already dealt with skew, so a 401 surviving it means
        // the session itself is gone.
        val first = execSigned(method, path, body, currentKey(), readTimeoutMs)
        if (first.code != 401) return first

        login()
        return execSigned(method, path, body, currentKey(), readTimeoutMs)
    }

    /**
     * A signed request that corrects for clock skew and retries once.
     *
     * Every signed call goes through here, enrolment and login included. That
     * matters more than it looks: a signature carries the sender's clock, and a
     * phone whose clock is off by more than the leeway fails *every* request
     * with a 401 that looks exactly like a bad credential. The very first
     * request a phone makes — registration — is the most likely to hit it,
     * because the app has had no prior response to learn an offset from.
     *
     * A skew rejection carries the server's clock in X-Prh-Time, so adopt it
     * and sign again. Only one retry: if a correctly-dated signature is still
     * refused, the problem is the key, not the clock.
     *
     * Trusting the server's clock is safe. It only shifts what *we* sign;
     * prh still judges against its own clock, so a bogus value would make our
     * requests fail rather than let stale ones through — and over pinned TLS
     * the header comes from prh anyway.
     */
    private fun execSigned(
        method: String,
        path: String,
        body: ByteArray?,
        signing: SigningKey?,
        readTimeoutMs: Int = 15_000,
    ): Response {
        val first = exec(method, path, body, signing, readTimeoutMs)
        if (first.code != 401 || signing == null) return first

        // No X-Prh-Time means this was not a skew rejection; nothing to fix.
        val serverTime = first.serverTime ?: return first
        clockOffsetSec = serverTime - System.currentTimeMillis() / 1000

        return exec(method, path, body, signing, readTimeoutMs)
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
        val url = URL(baseUrl.trimEnd('/') + path)
        val conn = url.openConnection() as HttpURLConnection

        if (conn is HttpsURLConnection) {
            val pin = tlsPin
                ?: throw IOException("no TLS pin for $baseUrl; pair again to obtain one")
            conn.sslSocketFactory = PrhTls.socketFactory(pin)
            conn.hostnameVerifier = PrhTls.hostnameVerifier
        }

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
        val resp = execSigned(method, path, body, signing, readTimeoutMs = 15_000)
        if (resp.code !in 200..299) {
            throw HttpException(resp.code, "HTTP ${resp.code}: ${resp.body}")
        }
        return if (resp.body.isNotEmpty()) JSONObject(resp.body) else JSONObject()
    }
}
