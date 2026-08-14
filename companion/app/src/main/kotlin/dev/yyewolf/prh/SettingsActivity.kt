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

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        handleDeepLink(intent)

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(48)
            gravity = Gravity.CENTER_HORIZONTAL
        }

        hostField = EditText(this).apply {
            hint = "Host (e.g. 192.168.1.10)"
            inputType = InputType.TYPE_CLASS_TEXT
        }
        portField = EditText(this).apply {
            hint = "Port"
            inputType = InputType.TYPE_CLASS_NUMBER
            setText("8477")
        }
        passwordField = EditText(this).apply {
            hint = "Password"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
        }
        pairButton = Button(this).apply { text = "Pair" }
        statusText = TextView(this).apply { text = "Not paired" }

        root.addView(hostField)
        root.addView(portField)
        root.addView(passwordField)
        root.addView(pairButton)
        root.addView(statusText)
        setContentView(root)

        prefill()

        pairButton.setOnClickListener { doPair() }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleDeepLink(intent)
        prefill()
    }

    // -- deep link ---------------------------------------------------------

    private var deepLinkPw: String? = null

    private fun handleDeepLink(intent: Intent?) {
        val data = intent?.data ?: return
        if (data.scheme != "prh") return
        val host = data.host ?: return
        val port = data.port
        deepLinkPw = data.getQueryParameter("pw")

        hostField.setText(host)
        if (port > 0) portField.setText(port.toString())
    }

    private fun prefill() {
        PrhPrefs.getBaseUrl(this)?.let { url ->
            val parsed = Uri.parse(url)
            hostField.setText(parsed.host)
            parsed.port.takeIf { it > 0 }?.let { portField.setText(it.toString()) }
        }
        if (PrhPrefs.isPaired(this)) {
            statusText.text = "Paired"
        }
        if (deepLinkPw != null) {
            passwordField.setText(deepLinkPw)
        }
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
