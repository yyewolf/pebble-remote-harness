package dev.yyewolf.prh

/**
 * Kotlin mirror of protocol.Envelope in api/internal/protocol.
 *
 * Fields are already truncated by prh — do not truncate again here, or the
 * watch and the phone will disagree about what was approved.
 *
 * The Msg* fields are populated only for type == MSG and never cross
 * Bluetooth. They carry what the agent is saying so the phone can show
 * context the 200px watch screen cannot.
 */
data class Envelope(
    val id: String,
    val seq: Long,
    val type: EventType,
    /**
     * Which VSCode window is asking. One prh serves them all, so approving
     * the right command in the wrong repository is a real hazard — the watch
     * must show this.
     */
    val project: String,
    val session: String,
    val title: String,
    val body: String,
    val choices: List<String> = emptyList(),
    val expires: Long = 0,
    // msg-only fields
    val msgRole: String = "",
    val msgPartID: String = "",
    val msgText: String = "",
    val msgKind: String = "",
    val msgTime: Long = 0,
)

enum class EventType(val wire: Int, val slug: String) {
    PERM(1, "perm"),
    QUES(2, "ques"),
    IDLE(3, "idle"),
    ERR(4, "err"),
    NOTE(5, "note"),

    /** Answered elsewhere (e.g. the VSCode UI); dismiss it on the watch. */
    GONE(6, "gone"),

    /** Conversation message part. Phone-only; the watch never sees it. */
    MSG(7, "msg");

    /** Whether the watch should be woken and shown answer affordances. */
    val needsReply: Boolean get() = this == PERM || this == QUES

    /**
     * Whether the companion forwards this to the watch over Bluetooth.
     * msg does not — conversation is phone-only.
     */
    val crossesBluetooth: Boolean
        get() = when (this) {
            PERM, QUES, IDLE, ERR, NOTE, GONE -> true
            MSG -> false
        }

    companion object {
        fun fromSlug(slug: String): EventType =
            entries.firstOrNull { it.slug == slug } ?: NOTE
    }
}

enum class ReplyAction(val wire: Int, val slug: String) {
    ONCE(1, "once"),
    ALWAYS(2, "always"),
    REJECT(3, "reject"),
    CHOICE(4, "choice"),
    TEXT(5, "text");

    companion object {
        fun fromWire(wire: Int): ReplyAction? = entries.firstOrNull { it.wire == wire }
    }
}

data class Reply(
    val eventId: String,
    val action: ReplyAction,
    val choice: Int = 0,
    val text: String = "",
)

/**
 * One session summary, from GET /v1/sessions. Built by prh from events, not by
 * calling Kilo, so prh needs no credentials.
 */
data class SessionSummary(
    val id: String,
    val project: String,
    val title: String,
    val dir: String,
    val status: String,        // idle | busy | retry | unknown
    val updated: Long,         // unix ms
    val hasPrompt: Boolean,
    val promptId: String,
    val promptType: String,    // perm | ques
)
