package dev.yyewolf.prh

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
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

        val baseUrl = PrhPrefs.getBaseUrl(this) ?: return
        val token = PrhPrefs.getToken(this) ?: return
        client = PrhClient(baseUrl, token)

        bridge.onReply { reply ->
            scope.launch { forward(reply) }
        }

        pollJob = scope.launch { pollLoop() }
    }

    private suspend fun pollLoop() {
        var cursor = PrhPrefs.getCursor(this)
        var backoff = 1000L

        while (true) {
            try {
                val (newCursor, events) = client.poll(cursor)
                cursor = newCursor
                PrhPrefs.setCursor(this, cursor)
                backoff = 1000L

                for (env in events) {
                    deliver(env)
                }
            } catch (e: PrhClient.CursorTooOld) {
                Log.w(TAG, "cursor too old, resetting")
                cursor = 0
                PrhPrefs.setCursor(this, cursor)
                backoff = 1000L
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
     * needsReply -> wakeWatchApp() then send()
     * otherwise  -> send() only if the watchapp is already open
     * if the watch is unreachable, post an alert notification instead. A
     * prompt must never be silently dropped: the agent is blocked waiting
     * for it.
     */
    private fun deliver(envelope: Envelope) {
        if (envelope.type.needsReply) {
            if (bridge.isConnected()) {
                bridge.wakeWatchApp()
                Thread.sleep(500)
                bridge.send(envelope)
            } else {
                postAlert(envelope)
            }
        } else {
            if (bridge.isConnected()) {
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
        val notif = NotificationCompat.Builder(this, CHANNEL_ALERTS)
            .setContentTitle("[${envelope.project}] ${envelope.title}")
            .setContentText(envelope.body)
            .setSmallIcon(android.R.drawable.stat_notify_error)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .build()
        nm.notify(envelope.id.hashCode(), notif)
    }
}
