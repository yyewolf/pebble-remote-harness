package dev.yyewolf.prh

import android.content.Intent
import android.os.Bundle
import android.util.Log
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.widget.BaseAdapter
import android.widget.LinearLayout
import android.widget.ListView
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
 * The session list — "what's going on everywhere".
 *
 * One prh serves every VSCode window, and every window's sessions land here.
 * A session with [SessionSummary.hasPrompt] true has a permission or question
 * prompt pending: tapping it opens the conversation so you can read what the
 * agent is asking for before approving — which is the whole point of a phone
 * view when the watch's 200px screen cannot show enough context.
 *
 * The list refreshes on a timer while it is on screen and stops the moment it
 * is not, so a backgrounded app is not quietly polling. prh returns the list
 * already ordered: pending prompts first, then most recent.
 */
class SessionsActivity : ComponentActivity() {

    private val scope = MainScope()
    private var refreshJob: Job? = null
    private lateinit var adapter: SessionAdapter
    private lateinit var emptyText: TextView
    private lateinit var list: ListView
    private var client: PrhClient? = null

    /** Whether a load has ever succeeded, so "no sessions" is not shown over a failure. */
    private var loaded = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        client = buildClient()
        if (client == null) {
            Toast.makeText(this, "Not paired. Pair in settings first.", Toast.LENGTH_LONG).show()
            startActivity(Intent(this, SettingsActivity::class.java))
            finish()
            return
        }

        adapter = SessionAdapter()

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(24)
        }

        val title = TextView(this).apply {
            text = "Sessions"
            textSize = 20f
            setPadding(0, 0, 0, 16)
        }

        emptyText = TextView(this).apply {
            text = "No sessions yet. Start coding in VSCode."
            setPadding(0, 48, 0, 0)
            gravity = Gravity.CENTER
            visibility = View.GONE
        }

        list = ListView(this).apply {
            adapter = this@SessionsActivity.adapter
            // Height 0 with weight 1, *not* WRAP_CONTENT. A ListView measures
            // only a couple of children when asked to wrap inside a vertical
            // LinearLayout, so its height is derived from a sample of the rows
            // and changes as the data does — which is what made sessions look
            // like they were appearing and disappearing at random on every
            // refresh. Giving it the leftover space measures it once, properly.
            layoutParams = LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                0,
                1f,
            )
        }
        list.setOnItemClickListener { _, _, position, _ ->
            val session = adapter.getItem(position)
            startActivity(
                Intent(this@SessionsActivity, ConversationActivity::class.java).apply {
                    putExtra(ConversationActivity.EXTRA_SESSION_ID, session.id)
                    putExtra(ConversationActivity.EXTRA_SESSION_TITLE, session.displayTitle())
                },
            )
        }

        root.addView(title)
        root.addView(emptyText)
        root.addView(list)
        setContentView(root)
        root.padForSystemBars()
    }

    override fun onResume() {
        super.onResume()
        startRefreshing()
    }

    override fun onPause() {
        refreshJob?.cancel()
        refreshJob = null
        super.onPause()
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    private fun buildClient(): PrhClient? {
        val baseUrl = PrhPrefs.getBaseUrl(this) ?: return null
        val deviceId = PrhPrefs.getDeviceId(this) ?: return null
        val deviceSecret = PrhPrefs.getDeviceSecret(this) ?: return null
        val tlsPin = PrhPrefs.getTlsPin(this) ?: return null
        return PrhClient(baseUrl, deviceId, deviceSecret, tlsPin)
    }

    /**
     * Refreshes now, then on an interval for as long as the list is visible.
     *
     * A failure is reported once and then retried quietly: prh going away for
     * a moment should not produce a toast every few seconds.
     */
    private fun startRefreshing() {
        val c = client ?: return
        refreshJob?.cancel()
        refreshJob = scope.launch {
            var reportedFailure = false
            while (isActive) {
                try {
                    val sessions = c.sessions()
                    loaded = true
                    reportedFailure = false
                    adapter.update(sessions)
                    // Debug, not info: this fires every few seconds while the
                    // list is open. It earns its place because "what prh sent"
                    // versus "what the adapter holds" is the split that tells a
                    // data bug apart from a layout one.
                    Log.d(
                        TAG,
                        "refresh: received=${sessions.size} rendered=${adapter.count} " +
                            "ids=${sessions.map { it.id.takeLast(6) }}",
                    )
                    emptyText.visibility = if (sessions.isEmpty()) View.VISIBLE else View.GONE
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    if (!reportedFailure) {
                        reportedFailure = true
                        Toast.makeText(
                            this@SessionsActivity,
                            "Failed to load: ${e.message}",
                            Toast.LENGTH_SHORT,
                        ).show()
                    }
                    if (!loaded) emptyText.visibility = View.GONE
                }
                delay(REFRESH_INTERVAL_MS)
            }
        }
    }

    // -- adapter ----------------------------------------------------------

    private inner class SessionAdapter : BaseAdapter() {
        private var sessions: List<SessionSummary> = emptyList()

        fun update(s: List<SessionSummary>) {
            // prh already orders these — prompts first, then by recency — so
            // they are taken as given rather than re-sorted here, which would
            // let the two disagree.
            //
            // Redrawing only when something actually changed matters here: the
            // list reloads every few seconds, and notifyDataSetChanged on
            // identical data still tears down and rebuilds every row, which
            // shows up as a visible blink and drops any touch in progress.
            if (s == sessions) return
            sessions = s
            notifyDataSetChanged()
        }

        override fun getCount(): Int = sessions.size
        override fun getItem(position: Int): SessionSummary = sessions[position]

        override fun getItemId(position: Int): Long = position.toLong()

        // Deliberately NOT hasStableIds() = true.
        //
        // With stable IDs, AbsListView preserves the *identity* of the row at
        // the top across notifyDataSetChanged: it records the first visible
        // row's ID and, after the change, scrolls so that same row is first
        // again. prh puts the newest session first, so every new session is an
        // insert at index 0 — and the list would faithfully scroll down by one
        // to keep the old first row in place, parking the new session above
        // the viewport. The symptom is a list that is permanently one session
        // short, with empty space at the bottom and the missing row always the
        // newest one.
        //
        // Without stable IDs the list preserves the first visible *index*
        // instead, so an insert at 0 simply appears. setSelection(0) does not
        // fix the stable-ID case: handleDataChanged re-derives the position
        // from the recorded sync ID and overrides it.

        override fun getView(position: Int, convertView: View?, parent: ViewGroup?): View {
            // Recycle. Building a fresh view hierarchy on every bind — which is
            // what ignoring convertView does — makes the list rebuild itself
            // continuously while it refreshes.
            val row = convertView as? LinearLayout ?: newRow()
            val s = getItem(position)

            val titleRow = row.getChildAt(0) as LinearLayout
            (titleRow.getChildAt(0) as TextView).text = s.displayTitle()
            (titleRow.getChildAt(1) as TextView).apply {
                text = when {
                    !s.hasPrompt -> ""
                    s.promptType == EventType.QUES.slug -> "  ● asking"
                    else -> "  ● waiting"
                }
                visibility = if (s.hasPrompt) View.VISIBLE else View.GONE
            }
            (row.getChildAt(1) as TextView).text = s.statusLine()
            return row
        }

        /** One row's view hierarchy, bound later by [getView]. */
        private fun newRow(): LinearLayout {
            val container = LinearLayout(this@SessionsActivity).apply {
                orientation = LinearLayout.VERTICAL
                setPadding(20)
            }
            val titleRow = LinearLayout(this@SessionsActivity).apply {
                orientation = LinearLayout.HORIZONTAL
                gravity = Gravity.CENTER_VERTICAL
            }
            titleRow.addView(
                TextView(this@SessionsActivity).apply {
                    textSize = 16f
                    setSingleLine(true)
                },
            )
            titleRow.addView(
                TextView(this@SessionsActivity).apply {
                    setTextColor(0xFFFF8800.toInt())
                    textSize = 14f
                },
            )
            container.addView(titleRow)
            container.addView(
                TextView(this@SessionsActivity).apply {
                    textSize = 12f
                    setTextColor(0xFF888888.toInt())
                    setPadding(0, 6, 0, 0)
                },
            )
            return container
        }
    }

    companion object {
        private const val TAG = "PrhSessions"

        /**
         * How often the list reloads while on screen. GET /v1/sessions is a
         * cheap in-memory read on prh's side, but it is a signed request over
         * the LAN, so this is a few seconds rather than sub-second.
         */
        private const val REFRESH_INTERVAL_MS = 4_000L
    }
}

/** Display helpers shared by the list and the conversation header. */
fun SessionSummary.displayTitle(): String =
    if (title.isNotBlank()) title else if (dir.isNotBlank()) dir else id

fun SessionSummary.statusLine(): String {
    val parts = mutableListOf<String>()
    if (project.isNotBlank()) parts.add(project)
    if (dir.isNotBlank() && dir != project) parts.add(dir)
    parts.add(status)
    return parts.joinToString(" · ")
}
