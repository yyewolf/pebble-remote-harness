package dev.yyewolf.prh

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/**
 * Restarts the poll after a reboot. Without this, a phone that restarts
 * overnight silently stops delivering prompts until the app is opened by hand.
 */
class BootReceiver : BroadcastReceiver() {

    /** TODO: implement — start PrhService, but only if a device token exists. */
    override fun onReceive(context: Context, intent: Intent) {
    }
}
