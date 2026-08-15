package dev.yyewolf.prh

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/**
 * Restarts the poll after a reboot. Without this, a phone that restarts
 * overnight silently stops delivering prompts until the app is opened by hand.
 */
class BootReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action == Intent.ACTION_BOOT_COMPLETED) {
            if (PrhPrefs.isPaired(context)) {
                PrhService.start(context)
            }
        }
    }
}
