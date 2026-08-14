package dev.yyewolf.prh

import android.app.Activity
import android.os.Bundle

/**
 * Pairing and status.
 *
 * Registration lives here rather than on the watch: typing an IP address with
 * watch buttons is not a design. The VSCode extension shows a QR encoding
 * `prh://<host>:<port>?pw=<password>`, which arrives as a VIEW intent.
 */
class SettingsActivity : Activity() {

    /**
     * TODO: implement.
     *  - fields for host, port, password, prefilled from a prh:// deep link
     *  - "Pair" calls PrhClient.register and stores the token in
     *    EncryptedSharedPreferences
     *  - show daemon reachability (GET /v1/health) and watch connection state
     *  - prompt for the battery-optimisation exemption; without it the poll
     *    dies in Doze and the whole app is pointless
     *  - start PrhService once paired
     */
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
    }
}
