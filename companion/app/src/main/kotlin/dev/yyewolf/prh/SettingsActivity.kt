package dev.yyewolf.prh

import android.app.Activity
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import android.text.InputType
import android.view.Gravity
import android.view.View
import android.view.WindowManager
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import androidx.core.view.setPadding
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/**
 * Pairing and status.
 *
 * Registration lives here rather than on the watch: typing an IP address with
 * watch buttons is not a design. The VSCode extension shows a QR encoding
 * `prh://<host>:<port>?k=<one-time pairing key>`, which arrives as a VIEW
 * intent. The older `?pw=<passphrase>` form still works and is the fallback
 * for when a code cannot be scanned, but it sends both the passphrase and the
 * device secret in the clear — see docs/protocol.md.
 */
class SettingsActivity : Activity() {

    private lateinit var hostField: EditText
    private lateinit var portField: EditText
    private lateinit var passwordField: EditText
    private lateinit var statusText: TextView
    private lateinit var pairButton: Button
    private val scope = CoroutineScope(Dispatchers.Main)

    private var deepLinkHost: String? = null
    private var deepLinkPort: Int = -1
    private var deepLinkPw: String? = null

    /**
     * The one-time pairing key from the QR, held only for this pairing attempt
     * and never persisted. It is spent as soon as a device enrols.
     */
    private var deepLinkKey: String? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        parseDeepLink(intent)

        val scroll = ScrollView(this).apply {
            fitsSystemWindows = true
            clipToPadding = false
        }

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(48, 24, 48, 48)
            gravity = Gravity.CENTER_HORIZONTAL
        }

        val hostLabel = TextView(this).apply {
            text = "Host"
            textSize = 16f
            setPadding(0, 0, 0, 8)
        }
        hostField = EditText(this).apply {
            hint = "e.g. 192.168.1.10"
            inputType = InputType.TYPE_CLASS_TEXT
            maxLines = 1
            setPadding(24, 16, 24, 16)
        }
        val portLabel = TextView(this).apply {
            text = "Port"
            textSize = 16f
            setPadding(0, 16, 0, 8)
        }
        portField = EditText(this).apply {
            hint = "8477"
            inputType = InputType.TYPE_CLASS_NUMBER
            setText("8477")
            maxLines = 1
            setPadding(24, 16, 24, 16)
        }
        val pwLabel = TextView(this).apply {
            text = "Password (only if you cannot scan a code)"
            textSize = 16f
            setPadding(0, 16, 0, 8)
        }
        passwordField = EditText(this).apply {
            hint = "Pairing password"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
            maxLines = 1
            setPadding(24, 16, 24, 16)
        }
        pairButton = Button(this).apply { text = "Pair" }
        statusText = TextView(this).apply {
            text = "Not paired"
            setPadding(0, 16, 0, 0)
        }

        root.addView(hostLabel)
        root.addView(hostField)
        root.addView(portLabel)
        root.addView(portField)
        root.addView(pwLabel)
        root.addView(passwordField)
        root.addView(pairButton)
        root.addView(statusText)
        scroll.addView(root)
        setContentView(scroll)

        prefill()
        pairButton.setOnClickListener { doPair() }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        parseDeepLink(intent)
        prefill()
    }

    // -- deep link ---------------------------------------------------------

    private fun parseDeepLink(intent: Intent?) {
        val data = intent?.data ?: return
        if (data.scheme != "prh") return
        deepLinkHost = data.host
        deepLinkPort = data.port
        // `k` is the one-time pairing key from the QR; `pw` is the passphrase
        // fallback for when a code cannot be scanned. Prefer `k`.
        deepLinkKey = data.getQueryParameter("k")
        deepLinkPw = data.getQueryParameter("pw")
    }

    private fun prefill() {
        deepLinkHost?.let { hostField.setText(it) }
        if (deepLinkPort > 0) portField.setText(deepLinkPort.toString())

        if (deepLinkHost == null) {
            PrhPrefs.getBaseUrl(this)?.let { url ->
                val parsed = Uri.parse(url)
                hostField.setText(parsed.host)
                parsed.port.takeIf { it > 0 }?.let { portField.setText(it.toString()) }
            }
        }

        if (PrhPrefs.isPaired(this)) {
            statusText.text = "Paired"
        }
        // The pairing key is never shown in the password box: it is a
        // high-entropy one-time secret that arrived out of band, and putting
        // it in an editable text field invites it into clipboards and
        // screenshots. It is held in memory for this one pairing attempt.
        if (deepLinkKey != null) {
            passwordField.isEnabled = false
            passwordField.setText("")
            passwordField.hint = "Not needed — pairing code scanned"
            statusText.text = "Ready to pair with the scanned code"
        } else {
            deepLinkPw?.let { passwordField.setText(it) }
        }
    }

    // -- pairing -----------------------------------------------------------

    private fun doPair() {
        val host = hostField.text.toString().trim()
        val port = portField.text.toString().trim()
        val pw = passwordField.text.toString()
        val key = deepLinkKey

        if (host.isEmpty()) {
            Toast.makeText(this, "Host required", Toast.LENGTH_SHORT).show()
            return
        }
        if (key == null && pw.isEmpty()) {
            Toast.makeText(this, "Scan a pairing code, or enter the passphrase", Toast.LENGTH_SHORT).show()
            return
        }

        val baseUrl = "http://$host:$port"
        pairButton.isEnabled = false
        statusText.text = "Pairing…"

        scope.launch {
            try {
                val client = PrhClient(baseUrl)
                val deviceName = PrhPrefs.getDeviceName(this@SettingsActivity)

                // Enrolment is the one exchange whose compromise hands over
                // everything, so take the sealed path whenever a code was
                // scanned: the key is proved by signing rather than sent, and
                // the device secret comes back encrypted under it.
                //
                // What comes back either way is the device secret, which is
                // what everything afterwards signs with — so this screen is not
                // needed again unless the device is revoked or prh loses its
                // devices file.
                val (deviceId, deviceSecret) = if (key != null) {
                    client.registerWithPairingKey(PrhSigning.decode(key), deviceName)
                } else {
                    client.registerWithPassphrase(pw, deviceName)
                }

                PrhPrefs.setBaseUrl(this@SettingsActivity, baseUrl)
                PrhPrefs.setCredentials(this@SettingsActivity, deviceId, deviceSecret)

                // One window enrols one device, so the scanned code is spent.
                // Dropping it stops a stale key sitting in memory and stops a
                // second tap on Pair from failing confusingly.
                deepLinkKey = null

                withContext(Dispatchers.Main) {
                    statusText.text = "Paired"
                    Toast.makeText(this@SettingsActivity, "Paired", Toast.LENGTH_SHORT).show()
                    requestBatteryExemption()
                    PrhService.start(this@SettingsActivity)
                }
            } catch (e: PrhClient.PairingClosed) {
                withContext(Dispatchers.Main) {
                    statusText.text = "Pairing is not open. Run \"Pebble Harness: Pair\" in the editor, then scan again."
                    pairButton.isEnabled = true
                }
            } catch (e: Exception) {
                withContext(Dispatchers.Main) {
                    statusText.text = "Pairing failed: ${e.message}"
                    pairButton.isEnabled = true
                }
            }
        }
    }

    private fun requestBatteryExemption() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            val pm = getSystemService(POWER_SERVICE) as PowerManager
            if (!pm.isIgnoringBatteryOptimizations(packageName)) {
                val intent = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS).apply {
                    data = Uri.parse("package:$packageName")
                }
                startActivity(intent)
            }
        }
    }
}
