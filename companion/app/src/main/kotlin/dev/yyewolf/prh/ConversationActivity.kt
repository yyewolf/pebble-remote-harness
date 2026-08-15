package dev.yyewolf.prh

import android.content.Intent
import android.os.Bundle
import android.text.TextUtils
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import androidx.activity.ComponentActivity
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.MainScope
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

/**
 * The per-session conversation view.
 *
 * This is the surface the user story is about: the agent asks to run
 * `rm ../fds`, the watch shows only "bash / rm ../fds" with no context, and
 * the user opens this view to read the conversation and decide. Here they see
 * what the agent has been doing, and can:
 *
 *  - approve / always-allow / reject a pending prompt inline (the buttons the
 *    watch would show, but with the conversation above them)
 *  - type a reply that starts a new turn in the session (POST
 *    /v1/sessions/{id}/prompt)
 *
 * The conversation long-polls prh while the view is open, so it stays live as
 * the agent works. Pending prompts arrive through that same stream — prh puts
 * perm/ques envelopes in the session's conversation precisely so this view has
 * something to render the buttons from — and a `gone` envelope under the same
 * ID retracts them when the prompt is answered anywhere else.
 *
 * None of this crosses Bluetooth: conversation is phone-only.
 */
class ConversationActivity : ComponentActivity() {

    private val scope = MainScope()
    private var client: PrhClient? = null
    private var pollJob: Job? = null

    private var sessionId: String = ""
    private var sessionTitle: String = ""

    private lateinit var scroll: ScrollView
    private lateinit var header: TextView
    private lateinit var promptPanel: LinearLayout
    private lateinit var messagesList: LinearLayout
    private lateinit var replyField: EditText
    private lateinit var sendButton: Button
    private var conversationCursor: Long = 0

    /**
     * Envelope ID -> the views rendering it, so an update replaces its text in
     * place rather than appending a duplicate. prh keys conversation entries by
     * ID (the part ID for a message), and re-sends an entry whenever it grows,
     * so without this the view would fill with partials of the same sentence.
     */
    private val entryViews = mutableMapOf<String, MessageRow>()

    /** One rendered message: the row in the list, and the two views to update. */
    private class MessageRow(val root: View, val role: TextView, val body: TextView)

    /** The prompt currently rendered in the panel, if any. */
    private var shownPromptId: String = ""

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        matchSystemBarsToTheme()

        sessionId = intent.getStringExtra(EXTRA_SESSION_ID) ?: ""
        if (sessionId.isEmpty()) {
            finish()
            return
        }
        sessionTitle = intent.getStringExtra(EXTRA_SESSION_TITLE) ?: ""

        client = buildClient()
        if (client == null) {
            Toast.makeText(this, "Not paired.", Toast.LENGTH_SHORT).show()
            finish()
            return
        }

        buildUi()
        startPolling()
    }

    /**
     * A prompt notification tapped while this view is already open.
     *
     * The notification uses CLEAR_TOP/SINGLE_TOP, so a second prompt for the
     * session on screen lands here rather than stacking a duplicate activity.
     * If it names a *different* session, the view has to be rebound to it —
     * otherwise tapping a notification for another project silently leaves you
     * reading the previous session's conversation.
     */
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        val incoming = intent.getStringExtra(EXTRA_SESSION_ID) ?: return
        if (incoming == sessionId) return

        setIntent(intent)
        sessionId = incoming
        sessionTitle = intent.getStringExtra(EXTRA_SESSION_TITLE) ?: ""
        conversationCursor = 0
        shownPromptId = ""
        entryViews.clear()
        messagesList.removeAllViews()
        promptPanel.removeAllViews()
        promptPanel.visibility = View.GONE
        header.text = sessionTitle.ifBlank { sessionId }
        startPolling()
    }

    override fun onDestroy() {
        // Cancelling the scope, not just the poll job: a reply or a send that
        // is still in flight holds a reference to this activity through its
        // Toast, and would leak it for the length of the request.
        scope.cancel()
        super.onDestroy()
    }

    // -- layout ---------------------------------------------------------------

    /**
     * Header, scrolling transcript, pinned composer.
     *
     * The composer sits outside the ScrollView rather than at the end of it.
     * Inside, it scrolled away with the history: on a long conversation you
     * had to scroll to the bottom to find the reply box, and every streamed
     * message moved it. Pinned, it is where a chat composer is expected to be,
     * and the transcript gets the rest.
     */
    private fun buildUi() {
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
        }

        root.addView(buildHeader(), matchWrap())

        messagesList = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            layoutParams = matchWrap()
        }
        promptPanel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            visibility = View.GONE
            layoutParams = matchWrap()
        }

        // The prompt goes *below* the conversation, not above it. You read what
        // the agent has been doing and then decide, so the decision belongs at
        // the end of that reading — and the view auto-scrolls to the bottom,
        // which put the buttons off-screen when they sat above the history.
        val content = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(16), dp(4), dp(16), dp(16))
            addView(messagesList)
            addView(promptPanel)
        }
        scroll = ScrollView(this).apply {
            clipToPadding = false
            layoutParams = matchRest()
            addView(content, matchWrap())
        }
        root.addView(scroll)
        root.addView(buildComposer(), matchWrap())

        setContentView(root)
        // The IME counts here: the composer is pinned to the bottom, so without
        // the keyboard inset it would sit underneath the keyboard it opened.
        root.padForSystemBars(includeIme = true)
    }

    private fun buildHeader(): LinearLayout {
        val back = TextView(this).apply {
            text = "‹"
            textSize = 30f
            setTextColor(textSecondary)
            gravity = Gravity.CENTER
            setPadding(dp(4), 0, dp(14), dp(4))
            background = withRipple(roundedRect(surfaceColor, radiusDp = 24))
            setOnClickListener { finish() }
        }
        header = bodyView(sessionTitle.ifBlank { sessionId }).apply {
            textSize = 18f
            setSingleLine(true)
            ellipsize = TextUtils.TruncateAt.MIDDLE
            layoutParams = LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f)
        }
        return LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setPadding(dp(12), dp(8), dp(16), dp(8))
            addView(back)
            addView(header)
        }
    }

    private fun buildComposer(): LinearLayout {
        replyField = styledField(getString(R.string.reply_hint), multiline = true).apply {
            maxLines = 4
            layoutParams = LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f)
        }
        sendButton = primaryButton(getString(R.string.send)).apply {
            setPadding(dp(18), dp(12), dp(18), dp(12))
            layoutParams = LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT,
                ViewGroup.LayoutParams.WRAP_CONTENT,
            ).apply { leftMargin = dp(8) }
        }
        sendButton.setOnClickListener { sendPrompt() }

        val row = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.BOTTOM
            setPadding(dp(12), dp(10), dp(12), dp(10))
            addView(replyField)
            addView(sendButton)
        }
        // A hairline above the whole bar, so the composer reads as a separate
        // surface from the transcript scrolling under it.
        return LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setBackgroundColor(surfaceColor)
            addView(divider())
            addView(row, matchWrap())
        }
    }

    private fun buildClient(): PrhClient? {
        val baseUrl = PrhPrefs.getBaseUrl(this) ?: return null
        val deviceId = PrhPrefs.getDeviceId(this) ?: return null
        val deviceSecret = PrhPrefs.getDeviceSecret(this) ?: return null
        val tlsPin = PrhPrefs.getTlsPin(this) ?: return null
        return PrhClient(baseUrl, deviceId, deviceSecret, tlsPin)
    }

    /**
     * Long-polls the conversation until the activity is destroyed.
     *
     * One job owns the whole loop, including its own retry. The earlier
     * version re-entered itself from the failure path, which reassigned the
     * job field from inside the job it was replacing: two loops could end up
     * polling the same session, each doubling on the next failure.
     *
     * Backoff is capped rather than fixed, so a prh that is down for a while
     * does not get a request every five seconds for as long as the screen is
     * open.
     */
    private fun startPolling() {
        val c = client ?: return
        pollJob?.cancel()
        pollJob = scope.launch {
            var backoffMs = 1_000L
            while (isActive) {
                try {
                    val (newCursor, events) = c.conversation(sessionId, conversationCursor)
                    conversationCursor = newCursor
                    render(events)
                    backoffMs = 1_000L
                } catch (e: CancellationException) {
                    throw e
                } catch (e: PrhClient.UnknownSession) {
                    // The session was deleted, or prh restarted and lost it.
                    // Nothing to poll for any more.
                    Toast.makeText(this@ConversationActivity, "Session gone", Toast.LENGTH_SHORT).show()
                    finish()
                    return@launch
                } catch (e: Exception) {
                    delay(backoffMs)
                    backoffMs = (backoffMs * 2).coerceAtMost(30_000L)
                }
            }
        }
    }

    /**
     * Renders conversation entries. Each is keyed by envelope ID: a message
     * part that grew replaces its own view, and a prompt that was answered
     * arrives as a `gone` under the same ID and takes its buttons away.
     */
    private fun render(events: List<Envelope>) {
        if (events.isEmpty()) return
        val atBottom = isScrolledToBottom()

        for (env in events) {
            when {
                env.type.needsReply -> showPrompt(env)
                env.type == EventType.GONE -> dismissPrompt(env.id)
                env.type == EventType.MSG -> renderMessage(env)
                else -> Unit // idle/err/note are the watch's business, not this view's
            }
        }

        // Only follow the tail if the user was already there. Yanking them back
        // down mid-scroll while the agent streams makes the view unreadable.
        if (atBottom) scroll.post { scroll.fullScroll(View.FOCUS_DOWN) }
    }

    private fun isScrolledToBottom(): Boolean {
        val content = scroll.getChildAt(0) ?: return true
        return scroll.scrollY + scroll.height >= content.height - dp(48)
    }

    private fun renderMessage(env: Envelope) {
        // prh sends an empty part when the agent retracts one.
        if (env.msgText.isBlank() && env.body.isBlank()) {
            entryViews.remove(env.id)?.let { messagesList.removeView(it.root) }
            return
        }
        val existing = entryViews[env.id]
        if (existing != null) {
            existing.role.text = roleLabel(env)
            existing.body.text = bodyOf(env)
            return
        }
        val row = makeMessageView(env)
        entryViews[env.id] = row
        messagesList.addView(row.root, matchWrap())
    }

    private fun bodyOf(env: Envelope): String = env.msgText.ifBlank { env.body }

    /**
     * One message as a bubble.
     *
     * Three shapes, because three kinds of thing are being shown and a flat
     * list of paragraphs made them indistinguishable: what you said (accent,
     * right), what the agent said (surface, left), and what it was doing —
     * thinking and tool calls — which is context rather than conversation and
     * is rendered quieter and monospaced so it can be skimmed past.
     */
    private fun makeMessageView(env: Envelope): MessageRow {
        val mine = env.msgRole == "user" && env.msgKind.isBlank()
        val chatter = env.msgKind == "reasoning" || env.msgKind == "tool" ||
            env.msgKind == "step-start"

        val role = captionView(roleLabel(env)).apply {
            textSize = 11f
            setTextColor(roleColor(env))
            letterSpacing = 0.06f
        }
        val body = if (chatter) monoView(bodyOf(env)) else bodyView(bodyOf(env))
        if (chatter) body.setTextColor(textSecondary)
        if (mine) body.setTextColor(textPrimary)

        val bubble = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(12), dp(9), dp(12), dp(10))
            background = when {
                mine -> roundedRect(accentSoft, strokeColor = accentSoft)
                chatter -> roundedRect(surfaceAltColor, radiusDp = 10)
                else -> roundedRect(surfaceColor, strokeColor = borderColor)
            }
            addView(role, matchWrap())
            addView(body, matchWrap())
        }

        // Bubbles stop short of the far edge so the alignment reads as
        // alignment rather than as full-width blocks.
        val row = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = if (mine) Gravity.END else Gravity.START
            setPadding(if (mine) dp(36) else 0, dp(4), if (mine) 0 else dp(36), dp(4))
            addView(bubble, matchWrap())
        }
        return MessageRow(row, role, body)
    }

    private fun roleLabel(env: Envelope): String = when (env.msgKind) {
        "reasoning" -> "thinking"
        "tool" -> "tool"
        "step-start" -> "step"
        "file" -> "file"
        "patch" -> "patch"
        else -> env.msgRole.ifBlank { "assistant" }
    }

    private fun roleColor(env: Envelope): Int = when {
        env.msgKind == "reasoning" -> textFaint
        env.msgKind == "tool" -> accentColor
        env.msgRole == "user" -> accentColor
        else -> textSecondary
    }

    // -- the pending prompt ------------------------------------------------

    /**
     * The decision, as a card at the end of the transcript.
     *
     * Deliberately the loudest thing on the screen — accent border, the body
     * in monospace because it is usually a literal command, and the three
     * answers coloured by what they do. This is the reason the screen exists.
     */
    private fun showPrompt(env: Envelope) {
        shownPromptId = env.id

        val card = card().apply {
            background = roundedRect(surfaceColor, strokeColor = accentColor, strokeDp = 2)
            layoutParams = matchWrap()
        }
        if (env.title.isNotBlank()) {
            card.addView(
                bodyView(env.title).apply {
                    textSize = 16f
                    setTextColor(accentColor)
                },
                matchWrap(),
            )
        }
        card.addSpaced(
            monoView(env.body).apply {
                background = roundedRect(surfaceAltColor, radiusDp = 8)
                setPadding(dp(10), dp(8), dp(10), dp(8))
            },
            if (env.title.isNotBlank()) 10 else 0,
        )

        env.choices.forEachIndexed { index, choice ->
            val button = when {
                env.type == EventType.QUES -> secondaryButton(choice)
                choice.startsWith("Always") -> secondaryButton(choice)
                choice.equals("Reject", ignoreCase = true) -> negativeButton(choice)
                else -> positiveButton(choice)
            }
            button.setOnClickListener { answerPrompt(env, choice, index) }
            card.addSpaced(button, if (index == 0) 14 else 8)
        }

        promptPanel.removeAllViews()
        promptPanel.addView(card)
        promptPanel.setPadding(0, dp(8), 0, 0)
        promptPanel.visibility = View.VISIBLE
    }

    /** Hides the panel if the retraction is for the prompt on screen. */
    private fun dismissPrompt(envId: String) {
        if (shownPromptId.isEmpty() || shownPromptId != envId) return
        shownPromptId = ""
        promptPanel.removeAllViews()
        promptPanel.visibility = View.GONE
    }

    /**
     * Maps a choice to the reply prh expects.
     *
     * A permission's choices are the fixed set the hub builds — Approve,
     * `Always: <pattern>`, Reject — so they map onto the actions by label. A
     * question's are Kilo's own option strings, which carry no such meaning:
     * those go back as a choice *index*, which is the only thing an arbitrary
     * option list can be answered with.
     */
    private fun replyFor(env: Envelope, choice: String, index: Int): Reply = when {
        env.type == EventType.QUES -> Reply(env.id, ReplyAction.CHOICE, choice = index)
        choice.startsWith("Always") -> Reply(env.id, ReplyAction.ALWAYS, choice = index)
        choice.equals("Reject", ignoreCase = true) -> Reply(env.id, ReplyAction.REJECT, choice = index)
        else -> Reply(env.id, ReplyAction.ONCE, choice = index)
    }

    private fun answerPrompt(env: Envelope, choice: String, index: Int) {
        val c = client ?: return
        // Take the buttons away immediately. They are the one control here
        // where a double tap has consequences, and the round trip is long
        // enough on a phone to invite one.
        setPromptEnabled(false)
        scope.launch {
            try {
                c.reply(replyFor(env, choice, index))
                dismissPrompt(env.id)
                Toast.makeText(this@ConversationActivity, "Sent", Toast.LENGTH_SHORT).show()
            } catch (e: CancellationException) {
                throw e
            } catch (e: PrhClient.HttpException) {
                // 409 means it was already answered — at the desk, or on the
                // watch. The prompt is settled either way, so clear it.
                if (e.code == 409) {
                    dismissPrompt(env.id)
                    Toast.makeText(this@ConversationActivity, "Already answered", Toast.LENGTH_SHORT).show()
                } else {
                    setPromptEnabled(true)
                    Toast.makeText(this@ConversationActivity, "Failed: ${e.message}", Toast.LENGTH_SHORT).show()
                }
            } catch (e: Exception) {
                setPromptEnabled(true)
                Toast.makeText(this@ConversationActivity, "Failed: ${e.message}", Toast.LENGTH_SHORT).show()
            }
        }
    }

    /** The buttons live inside the prompt card, which is the panel's only child. */
    private fun setPromptEnabled(enabled: Boolean) {
        val card = promptPanel.getChildAt(0) as? LinearLayout ?: return
        for (i in 0 until card.childCount) {
            (card.getChildAt(i) as? Button)?.isEnabled = enabled
        }
    }

    // -- replying into the session -----------------------------------------

    private fun sendPrompt() {
        val c = client ?: return
        val text = replyField.text.toString().trim()
        if (text.isEmpty()) return

        // Clear and disable on the way out, restore only on failure: the text
        // is gone from the field the moment it is accepted, so a second tap
        // cannot send it twice.
        replyField.setText("")
        sendButton.isEnabled = false
        scope.launch {
            try {
                c.sessionPrompt(sessionId, text)
                Toast.makeText(this@ConversationActivity, "Sent", Toast.LENGTH_SHORT).show()
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                // Give the text back rather than losing what they typed.
                replyField.setText(text)
                Toast.makeText(this@ConversationActivity, "Failed: ${e.message}", Toast.LENGTH_SHORT).show()
            } finally {
                sendButton.isEnabled = true
            }
        }
    }

    companion object {
        const val EXTRA_SESSION_ID = "session_id"
        const val EXTRA_SESSION_TITLE = "session_title"
    }
}
