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
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
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
 * `prh://<host>:<port>?pw=<password>`, which arrives as a VIEW intent.
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

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        parseDeepLink(intent)

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(48)
            gravity = Gravity.CENTER_HORIZONTAL
        }

        val hostLabel = TextView(this).apply { text = "Host" }
        hostField = EditText(this).apply {
            hint = "e.g. 192.168.1.10"
            inputType = InputType.TYPE_CLASS_TEXT
            maxLines = 1
        }
        val portLabel = TextView(this).apply { text = "Port" }
        portField = EditText(this).apply {
            hint = "8477"
            inputType = InputType.TYPE_CLASS_NUMBER
            setText("8477")
            maxLines = 1
        }
        val pwLabel = TextView(this).apply { text = "Password" }
        passwordField = EditText(this).apply {
            hint = "Pairing password"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
            maxLines = 1
        }
        pairButton = Button(this).apply { text = "Pair" }
        statusText = TextView(this).apply { text = "Not paired" }

        root.addView(hostLabel)
        root.addView(hostField)
        root.addView(portLabel)
        root.addView(portField)
        root.addView(pwLabel)
        root.addView(passwordField)
        root.addView(pairButton)
        root.addView(statusText)
        setContentView(root)

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
        deepLinkPw?.let { passwordField.setText(it) }
    }

    // -- pairing -----------------------------------------------------------

    private fun doPair() {
        val host = hostField.text.toString().trim()
        val port = portField.text.toString().trim()
        val pw = passwordField.text.toString()

        if (host.isEmpty() || pw.isEmpty()) {
            Toast.makeText(this, "Host and password required", Toast.LENGTH_SHORT).show()
            return
        }

        val baseUrl = "http://$host:$port"
        pairButton.isEnabled = false
        statusText.text = "Pairing…"

        scope.launch {
            try {
                val client = PrhClient(baseUrl)
                val deviceName = PrhPrefs.getDeviceName(this@SettingsActivity)
                val token = client.register(pw, deviceName)
                PrhPrefs.setBaseUrl(this@SettingsActivity, baseUrl)
                PrhPrefs.setToken(this@SettingsActivity, token)
                withContext(Dispatchers.Main) {
                    statusText.text = "Paired"
                    Toast.makeText(this@SettingsActivity, "Paired", Toast.LENGTH_SHORT).show()
                    requestBatteryExemption()
                    PrhService.start(this@SettingsActivity)
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
