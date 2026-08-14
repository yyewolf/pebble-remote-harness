package dev.yyewolf.prh

import android.app.Service
import android.content.Intent
import android.os.IBinder

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
    }

    override fun onBind(intent: Intent?): IBinder? = null

    /**
     * TODO: implement.
     *  - startForeground with a low-importance ongoing notification
     *  - restore the persisted cursor, then loop PrhClient.poll
     *  - on timeout, reconnect immediately with the same cursor
     *  - on error, back off exponentially to a ~60s ceiling
     *  - on CursorTooOld, reset to the newest cursor and accept the gap
     *  - return START_STICKY so Android brings the service back
     */
    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        return START_STICKY
    }

    /**
     * Delivers one envelope.
     *
     * TODO: implement.
     *  - needsReply -> PebbleBridge.wakeWatchApp() then send()
     *  - otherwise  -> send() only if the watchapp is already open
     *  - if the watch is unreachable, post an alert notification instead. A
     *    prompt must never be silently dropped: the agent is blocked waiting
     *    for it.
     */
    private fun deliver(envelope: Envelope) {
    }

    /**
     * Forwards a watch reply to prh.
     *
     * TODO: implement with retry. The watch does not retry, so this is the
     * only thing standing between a button press and a lost approval.
     */
    private fun forward(reply: Reply) {
    }
}
