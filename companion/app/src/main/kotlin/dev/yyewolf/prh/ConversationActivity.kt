package dev.yyewolf.prh

import android.content.Intent
import android.os.Bundle
import android.text.InputType
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
import androidx.core.view.setPadding
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
    private lateinit var promptText: TextView
    private lateinit var messagesList: LinearLayout
    private lateinit var replyField: EditText
    private lateinit var sendButton: Button
    private var conversationCursor: Long = 0

    /**
     * Envelope ID -> the view rendering it, so an update replaces its text in
     * place rather than appending a duplicate. prh keys conversation entries by
     * ID (the part ID for a message), and re-sends an entry whenever it grows,
     * so without this the view would fill with partials of the same sentence.
     */
    private val entryViews = mutableMapOf<String, View>()

    /** The prompt currently rendered in the panel, if any. */
    private var shownPromptId: String = ""

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

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

    private fun buildUi() {
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(20)
            layoutParams = matchWidth()
        }

        header = TextView(this).apply {
            text = sessionTitle.ifBlank { sessionId }
            textSize = 18f
            setPadding(0, 0, 0, 12)
            layoutParams = matchWidth()
        }

        promptPanel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(20)
            visibility = View.GONE
            layoutParams = matchWidth()
        }
        promptText = TextView(this).apply {
            textSize = 14f
            setPadding(0, 0, 0, 12)
        }

        messagesList = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            layoutParams = matchWidth()
        }

        val replyLabel = TextView(this).apply {
            text = "Reply"
            textSize = 14f
            setPadding(0, 16, 0, 6)
        }
        replyField = EditText(this).apply {
            hint = "Type a message to this session…"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_FLAG_MULTI_LINE
            maxLines = 3
            setPadding(20, 12, 20, 12)
            layoutParams = matchWidth()
        }
        sendButton = Button(this).apply {
            text = "Send"
            layoutParams = matchWidth()
        }
        sendButton.setOnClickListener { sendPrompt() }

        // The prompt goes *below* the conversation, not above it. You read what
        // the agent has been doing and then decide, so the decision belongs at
        // the end of that reading — and the view auto-scrolls to the bottom,
        // which put the buttons off-screen when they sat above the history.
        root.addView(header)
        root.addView(messagesList)
        root.addView(promptPanel)
        root.addView(replyLabel)
        root.addView(replyField)
        root.addView(sendButton)

        scroll = ScrollView(this).apply {
            addView(root)
        }
        setContentView(scroll)
        scroll.padForSystemBars()
    }

    private fun matchWidth() = LinearLayout.LayoutParams(
        ViewGroup.LayoutParams.MATCH_PARENT,
        ViewGroup.LayoutParams.WRAP_CONTENT,
    )

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
        return scroll.scrollY + scroll.height >= content.height - 48
    }

    private fun renderMessage(env: Envelope) {
        // prh sends an empty part when the agent retracts one.
        if (env.msgText.isBlank() && env.body.isBlank()) {
            entryViews.remove(env.id)?.let { messagesList.removeView(it) }
            return
        }
        val existing = entryViews[env.id]
        if (existing != null) {
            (existing as LinearLayout).let {
                (it.getChildAt(0) as TextView).text = roleLabel(env)
                (it.getChildAt(1) as TextView).text = bodyOf(env)
            }
            return
        }
        val view = makeMessageView(env)
        entryViews[env.id] = view
        messagesList.addView(view)
    }

    private fun bodyOf(env: Envelope): String = env.msgText.ifBlank { env.body }

    private fun makeMessageView(env: Envelope): View {
        val container = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(16, 12, 16, 12)
            layoutParams = matchWidth()
        }
        val role = TextView(this).apply {
            text = roleLabel(env)
            textSize = 12f
            setTextColor(roleColor(env))
            layoutParams = matchWidth()
        }
        val body = TextView(this).apply {
            text = bodyOf(env)
            textSize = 14f
            layoutParams = matchWidth()
        }
        container.addView(role)
        container.addView(body)
        return container
    }

    private fun roleLabel(env: Envelope): String = when (env.msgKind) {
        "reasoning" -> "thinking"
        "tool" -> "tool"
        "step-start" -> "step"
        "file" -> "file"
        "patch" -> "patch"
        else -> env.msgRole.ifBlank { "assistant" }
    }

    private fun roleColor(env: Envelope): Int = when (env.msgKind) {
        "reasoning" -> 0xFF888888.toInt()
        "tool" -> 0xFF0088CC.toInt()
        else -> if (env.msgRole == "user") 0xFF0066CC.toInt() else 0xFF222222.toInt()
    }

    // -- the pending prompt ------------------------------------------------

    private fun showPrompt(env: Envelope) {
        shownPromptId = env.id
        promptText.text = buildString {
            if (env.title.isNotBlank()) append(env.title).append('\n')
            append(env.body)
        }

        promptPanel.removeAllViews()
        promptPanel.addView(promptText)
        env.choices.forEachIndexed { index, choice ->
            val btn = Button(this).apply {
                text = choice
                layoutParams = matchWidth()
                setOnClickListener { answerPrompt(env, choice, index) }
            }
            promptPanel.addView(btn)
        }
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

    private fun setPromptEnabled(enabled: Boolean) {
        for (i in 0 until promptPanel.childCount) {
            (promptPanel.getChildAt(i) as? Button)?.isEnabled = enabled
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
