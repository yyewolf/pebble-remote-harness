package dev.yyewolf.prh

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.core.app.NotificationCompat
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import java.io.IOException

/**
 * Foreground service holding the long-poll against prh.
 *
 * A foreground service with an ongoing notification is what keeps the socket
 * alive on API 26+. Without it — and without a battery-optimisation
 * exemption — Doze kills the poll and prompts arrive minutes late or never.
 */
class PrhService : Service() {

    companion object {
        const val CHANNEL_ONGOING = "prh.ongoing"
        const val CHANNEL_ALERTS = "prh.alerts"
        const val NOTIFICATION_ID = 1
        private const val TAG = "PrhService"

        /**
         * Well under the 12h session TTL, and frequent enough that a prh
         * restart is noticed before the next prompt rather than during it.
         * One tiny signed request every few minutes is cheaper than a missed
         * approval.
         */
        private const val HEARTBEAT_INTERVAL_MS = 4 * 60 * 1000L

        fun start(context: Context) {
            val intent = Intent(context, PrhService::class.java)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(intent)
            } else {
                context.startService(intent)
            }
        }

        fun stop(context: Context) {
            context.stopService(Intent(context, PrhService::class.java))
        }
    }

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private var pollJob: Job? = null
    private var heartbeatJob: Job? = null
    private lateinit var client: PrhClient
    private lateinit var bridge: PebbleBridge

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        createChannels()
        bridge = PebbleBridge(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForeground()
        startPollLoop()
        return START_STICKY
    }

    override fun onDestroy() {
        bridge.shutdown()
        scope.cancel()
        super.onDestroy()
    }

    // -- foreground notification -------------------------------------------

    private fun startForeground() {
        val notif = NotificationCompat.Builder(this, CHANNEL_ONGOING)
            .setContentTitle(getString(R.string.ongoing_text))
            .setSmallIcon(android.R.drawable.stat_sys_data_bluetooth)
            .setOngoing(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIFICATION_ID, notif, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        } else {
            startForeground(NOTIFICATION_ID, notif)
        }
    }

    private fun createChannels() {
        val nm = getSystemService(NOTIFICATION_SERVICE) as NotificationManager
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            nm.createNotificationChannel(NotificationChannel(
                CHANNEL_ONGOING, getString(R.string.ongoing_channel),
                NotificationManager.IMPORTANCE_LOW,
            ))
            nm.createNotificationChannel(NotificationChannel(
                CHANNEL_ALERTS, getString(R.string.alerts_channel),
                NotificationManager.IMPORTANCE_HIGH,
            ))
        }
    }

    // -- poll loop ----------------------------------------------------------

    private fun startPollLoop() {
        if (pollJob?.isActive == true) return

        val baseUrl = PrhPrefs.getBaseUrl(this)
        val deviceId = PrhPrefs.getDeviceId(this)
        val deviceSecret = PrhPrefs.getDeviceSecret(this)
        val tlsPin = PrhPrefs.getTlsPin(this)
        if (baseUrl == null || deviceId == null || deviceSecret == null) {
            Log.w(TAG, "not paired, skipping poll loop")
            return
        }
        Log.i(TAG, "starting poll loop against $baseUrl")
        // No session yet: the client establishes one on its first request and
        // re-establishes it whenever prh forgets, which needs nothing from
        // the user because the device secret is persisted.
        client = PrhClient(baseUrl, deviceId, deviceSecret, tlsPin)

        bridge.startRelay { reply ->
            Log.i(TAG, "reply from watch via PKJS: ${reply.eventId} ${reply.action}")
            scope.launch { forward(reply) }
        }

        pollJob = scope.launch { pollLoop() }
        heartbeatJob = scope.launch { heartbeatLoop() }
    }

    /**
     * Keeps the session alive and, more importantly, notices when it dies.
     *
     * The poll loop would eventually discover a dropped session on its own,
     * but only when it next returns — which on a quiet day is up to a minute
     * after prh restarted, and the discovery competes with a prompt that is
     * already blocking the agent. Finding out on a schedule instead means the
     * session is usually already re-established by the time it matters.
     *
     * [PrhClient.heartbeat] logs in again by itself on a 401, so a successful
     * call here means "session live" and a failure means the daemon is
     * unreachable or the pairing is gone.
     */
    private suspend fun heartbeatLoop() {
        while (true) {
            delay(HEARTBEAT_INTERVAL_MS)
            try {
                client.heartbeat()
            } catch (e: PrhClient.NotPaired) {
                // Nothing to retry: prh has forgotten this device entirely.
                Log.e(TAG, "pairing rejected by prh; re-pair from settings", e)
                return
            } catch (e: Exception) {
                Log.w(TAG, "heartbeat failed: ${e.message}")
            }
        }
    }

    private suspend fun pollLoop() {
        var cursor = PrhPrefs.getCursor(this)
        var backoff = 1000L
        Log.i(TAG, "poll loop started, cursor=$cursor")

        while (true) {
            try {
                val (newCursor, events) = client.poll(cursor)
                cursor = newCursor
                PrhPrefs.setCursor(this, cursor)
                backoff = 1000L

                if (events.isNotEmpty()) {
                    Log.i(TAG, "received ${events.size} events at cursor=$cursor")
                }

                for (env in events) {
                    Log.i(TAG, "delivering: type=${env.type} project=${env.project} title=${env.title}")
                    deliver(env)
                }
            } catch (e: PrhClient.CursorTooOld) {
                Log.w(TAG, "cursor too old, resetting")
                cursor = 0
                PrhPrefs.setCursor(this, cursor)
                backoff = 1000L
            } catch (e: PrhClient.NotPaired) {
                // prh does not know this device, so backing off and retrying
                // would spin forever. Must be caught before IOException — it
                // is one.
                Log.e(TAG, "pairing rejected by prh; re-pair from settings", e)
                return
            } catch (e: IOException) {
                Log.w(TAG, "poll error: ${e.message}")
                delay(backoff)
                backoff = minOf(backoff * 2, 60_000L)
            } catch (e: Exception) {
                Log.e(TAG, "unexpected poll error", e)
                delay(backoff)
                backoff = minOf(backoff * 2, 60_000L)
            }
        }
    }

    /**
     * Delivers one envelope.
     *
     * needsReply -> wakeWatchApp() then send(), *and* notify the phone
     * otherwise  -> send() only if the watchapp is already open
     *
     * A prompt always produces a phone notification, whether or not the watch
     * got it. The watch's 200px screen is why this app exists: "bash / rm
     * ../fds" is not enough to decide on, and the notification is the only
     * thing that says a decision is waiting and taps straight through to the
     * conversation behind it. Posting only when the watch was unreachable —
     * which is what this did — meant that in the normal case, watch connected,
     * the phone stayed completely silent and the pending prompt could only be
     * found by opening the app and going looking for it.
     *
     * msg envelopes never reach the watch — conversation is phone-only and
     * stays off Bluetooth. The conversation view fetches them on demand via
     * GET /v1/sessions/{id}/conversation, so there is nothing to do here.
     */
    private fun deliver(envelope: Envelope) {
        if (!envelope.type.crossesBluetooth) {
            return
        }

        val connected = bridge.isConnected()
        Log.i(TAG, "deliver: connected=$connected type=${envelope.type}")

        if (envelope.type.needsReply) {
            if (connected) {
                Log.i(TAG, "waking watch and sending envelope")
                bridge.wakeWatchApp()
                Thread.sleep(500)
                bridge.send(envelope)
            } else {
                Log.w(TAG, "watch unreachable, prompt is phone-only")
            }
            postAlert(envelope)
        } else if (envelope.type == EventType.GONE) {
            // Answered on the watch, or at the desk. Take the notification
            // down rather than leaving one that opens a settled prompt.
            cancelAlert(envelope.id)
            if (connected) bridge.send(envelope)
        } else {
            if (connected) {
                bridge.send(envelope)
            } else if (envelope.type == EventType.IDLE || envelope.type == EventType.ERR) {
                postAlert(envelope)
            }
        }
    }

    /**
     * Forwards a watch reply to prh.
     *
     * The watch does not retry, so this is the only thing standing between a
     * button press and a lost approval.
     */
    private suspend fun forward(reply: Reply) {
        try {
            client.reply(reply)
        } catch (e: Exception) {
            Log.e(TAG, "forward reply failed", e)
        }
    }

    // -- fallback notification ---------------------------------------------

    private fun postAlert(envelope: Envelope) {
        val nm = getSystemService(NOTIFICATION_SERVICE) as NotificationManager
        val builder = NotificationCompat.Builder(this, CHANNEL_ALERTS)
            .setContentTitle("[${envelope.project}] ${envelope.title}")
            .setContentText(envelope.body)
            // The body is a command or a question and is routinely longer than
            // one line. Collapsed it is elided, which is the opposite of what
            // this notification is for.
            .setStyle(NotificationCompat.BigTextStyle().bigText(envelope.body))
            .setSmallIcon(android.R.drawable.stat_notify_error)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_HIGH)

        // A prompt taps through to its conversation, so the decision is made
        // with the context above it. Without this the notification is a dead
        // end: it says something needs answering and gives no way to get there.
        if (envelope.type.needsReply && envelope.session.isNotEmpty()) {
            builder.setContentIntent(conversationIntent(envelope))
        }

        nm.notify(envelope.id.hashCode(), builder.build())
    }

    /**
     * Deep-links into the session's conversation.
     *
     * CLEAR_TOP/SINGLE_TOP so tapping a second prompt for a session already on
     * screen reuses that view instead of stacking another copy of it.
     */
    private fun conversationIntent(envelope: Envelope): PendingIntent {
        val intent = Intent(this, ConversationActivity::class.java).apply {
            putExtra(ConversationActivity.EXTRA_SESSION_ID, envelope.session)
            putExtra(ConversationActivity.EXTRA_SESSION_TITLE, envelope.project)
            flags = Intent.FLAG_ACTIVITY_NEW_TASK or
                Intent.FLAG_ACTIVITY_CLEAR_TOP or
                Intent.FLAG_ACTIVITY_SINGLE_TOP
        }
        return PendingIntent.getActivity(
            this,
            envelope.session.hashCode(),
            intent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
    }

    /** Takes down the notification for a prompt that has been settled. */
    private fun cancelAlert(envelopeId: String) {
        val nm = getSystemService(NOTIFICATION_SERVICE) as NotificationManager
        nm.cancel(envelopeId.hashCode())
    }
}
