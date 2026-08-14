package dev.yyewolf.prh

/**
 * HTTP client for the prh daemon. See docs/protocol.md.
 *
 * Deliberately dependency-free in the scaffold: whether this ends up on
 * OkHttp or HttpURLConnection is an open choice, and the long-poll is the
 * only demanding part.
 */
class PrhClient(
    private val baseUrl: String,
    private var token: String? = null,
) {

    class CursorTooOld : Exception("cursor fell out of the server's ring buffer")

    /**
     * Trades the pairing password for a device token.
     *
     * TODO: implement POST /v1/register. Store the returned token in
     * EncryptedSharedPreferences, never in plain prefs.
     */
    suspend fun register(password: String, deviceName: String): String =
        throw NotImplementedError("register")

    /**
     * Long-polls GET /v1/poll, blocking up to [waitSeconds].
     *
     * TODO: implement.
     *  - an empty list with an unchanged cursor means timeout; poll again
     *    immediately with the same cursor
     *  - HTTP 410 means the cursor was evicted: throw [CursorTooOld] so the
     *    caller resets to the newest cursor and accepts the gap
     *  - the read timeout must exceed waitSeconds, or every poll looks like
     *    a network failure
     */
    suspend fun poll(cursor: Long, waitSeconds: Int = 55): Pair<Long, List<Envelope>> =
        throw NotImplementedError("poll")

    /**
     * Answers a prompt via POST /v1/reply.
     *
     * TODO: implement, and retry on network failure — this retry is what
     * actually guarantees delivery, since the watch does not retry. HTTP 409
     * means already-answered and must not be retried.
     */
    suspend fun reply(reply: Reply): Unit = throw NotImplementedError("reply")

    /** Sends dictated text as new work. POST /v1/prompt. */
    suspend fun prompt(session: String, text: String): Unit = throw NotImplementedError("prompt")

    /** Unauthenticated liveness probe, used by the settings screen. */
    suspend fun health(): Boolean = throw NotImplementedError("health")
}
