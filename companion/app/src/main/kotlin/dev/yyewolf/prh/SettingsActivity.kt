package dev.yyewolf.prh

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import android.text.InputType
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.widget.Button
import android.widget.CheckBox
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.content.ContextCompat
import androidx.core.view.setPadding
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanIntentResult
import com.journeyapps.barcodescanner.ScanOptions
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.util.Date

/**
 * Pairing and status.
 *
 * Registration lives here rather than on the watch: typing an IP address with
 * watch buttons is not a design. The VSCode extension shows a QR encoding
 * `prh://<host>:<port>?k=<one-time pairing key>&f=<tls pin>`, which arrives as
 * a VIEW intent. That is the only way to pair: the passphrase fallback that
 * ran over plaintext HTTP has been removed, so there is no code path that
 * sends a secret without the pinned-TLS protection.
 */
class SettingsActivity : ComponentActivity() {

    private lateinit var hostField: EditText
    private lateinit var portField: EditText
    private lateinit var statusText: TextView
    private lateinit var pairButton: Button
    private lateinit var scanButton: Button
    private lateinit var sessionsButton: Button
    private lateinit var heartbeatText: TextView
    private lateinit var watchCheck: CheckBox
    private lateinit var notifCheck: CheckBox
    private val scope = CoroutineScope(Dispatchers.Main)

    /** Ticks the heartbeat line while this screen is on top. */
    private var heartbeatJob: Job? = null

    /** The user's 12/24h preference, resolved once. */
    private val timeFormat by lazy { android.text.format.DateFormat.getTimeFormat(this) }

    /**
     * Launches the in-app QR scanner. The decoded text — a `prh://` deep
     * link — is treated exactly like a VIEW intent: parsed into the same
     * deep-link fields `doPair` consumes. Decoding a non-`prh` string is
     * harmless: `parseDeepLink` ignores it.
     */
    private val scanLauncher = registerForActivityResult(ScanContract()) { result: ScanIntentResult ->
        val contents = result.contents
        if (contents.isNullOrEmpty()) return@registerForActivityResult
        // A VIEW intent's `.data` is a Uri; the scanner hands back raw text, so
        // wrap it. Everything downstream reads scheme/host/query, which Uri
        // parses from a string the same way Intent would.
        applyScannedCode(Uri.parse(contents))
    }

    /**
     * Requests CAMERA when the user taps Scan, then launches the scanner on a
     * grant. Marshmallow-and-up runtime permissions are the only target here
     * (minSdk 26), so there is no pre-M branch.
     */
    private val cameraPermission =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
            if (granted) launchScanner() else {
                Toast.makeText(this, "Camera permission needed to scan", Toast.LENGTH_SHORT).show()
            }
        }

    /**
     * Requests POST_NOTIFICATIONS. A denial is not fatal — prompts still reach
     * the watch — but it is worth saying what was lost, because the phone then
     * has no way to tell you a decision is waiting.
     */
    private val notificationPermissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
            if (!granted) {
                Toast.makeText(
                    this,
                    "Without notifications, prompts only reach the watch",
                    Toast.LENGTH_LONG,
                ).show()
            }
        }

    private var deepLinkHost: String? = null
    private var deepLinkPort: Int = -1

    /**
     * The one-time pairing key from the QR, held only for this pairing attempt
     * and never persisted. It is spent as soon as a device enrols.
     */
    private var deepLinkKey: String? = null

    /**
     * base64url SHA-256 of prh's TLS public key, from the QR's `f` parameter.
     *
     * Its presence is what selects https. Without it the phone has no way to
     * tell prh from anything else answering on that address, so pairing cannot
     * proceed — there is no plaintext fallback.
     */
    private var deepLinkPin: String? = null

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
        pairButton = Button(this).apply { text = "Pair" }
        scanButton = Button(this).apply { text = getString(R.string.scan_button) }
        statusText = TextView(this).apply {
            text = "Not paired"
            setPadding(0, 16, 0, 0)
        }
        sessionsButton = Button(this).apply {
            text = getString(R.string.sessions_button)
            visibility = View.GONE
            setOnClickListener {
                startActivity(Intent(this@SettingsActivity, SessionsActivity::class.java))
            }
        }

        heartbeatText = TextView(this).apply {
            textSize = 13f
            setPadding(0, 4, 0, 0)
        }

        // isChecked before the listener, deliberately: attaching first would
        // fire the callback for the value that was just read back out of prefs
        // and write it straight in again, which is harmless here but is how
        // toggle state ends up being "changed" by merely opening the screen.
        watchCheck = CheckBox(this).apply {
            text = getString(R.string.watch_notifications)
            isChecked = PrhPrefs.isWatchEnabled(this@SettingsActivity)
            setOnCheckedChangeListener { _, checked ->
                PrhPrefs.setWatchEnabled(this@SettingsActivity, checked)
                warnIfNothingAlerts()
            }
        }
        notifCheck = CheckBox(this).apply {
            text = getString(R.string.phone_notifications)
            isChecked = PrhPrefs.isPhoneNotificationsEnabled(this@SettingsActivity)
            setOnCheckedChangeListener { _, checked ->
                PrhPrefs.setPhoneNotificationsEnabled(this@SettingsActivity, checked)
                // Turning this on is worthless while POST_NOTIFICATIONS is
                // denied — notify() would be dropped without an error — so the
                // ask happens at the moment the intent is expressed.
                if (checked) requestNotificationPermission()
                warnIfNothingAlerts()
            }
        }

        root.addView(hostLabel)
        root.addView(hostField)
        root.addView(portLabel)
        root.addView(portField)
        root.addView(pairButton)
        root.addView(scanButton)
        root.addView(statusText)
        root.addView(sessionsButton)
        root.addView(heading(getString(R.string.connection_heading)))
        root.addView(heartbeatText.fullWidth())
        root.addView(heading(getString(R.string.delivery_heading)))
        root.addView(watchCheck.fullWidth())
        root.addView(notifCheck.fullWidth())
        scroll.addView(root)
        setContentView(scroll)
        scroll.padForSystemBars()

        prefill()
        pairButton.setOnClickListener { doPair() }
        scanButton.setOnClickListener { onScanClicked() }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        parseDeepLink(intent)
        prefill()
    }

    override fun onResume() {
        super.onResume()
        heartbeatJob?.cancel()
        heartbeatJob = scope.launch {
            while (isActive) {
                renderHeartbeat()
                // Every second, because the useful part is watching the age
                // reset: if it keeps climbing past a minute the poll is not
                // coming back, and that is the whole diagnosis.
                delay(1_000)
            }
        }
    }

    override fun onPause() {
        heartbeatJob?.cancel()
        heartbeatJob = null
        super.onPause()
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    // -- connection health --------------------------------------------------

    /**
     * Renders "is this working right now?" as one line.
     *
     * Two independent facts, because either alone lies. The service can be
     * running while every request fails, and the last contact can look recent
     * seconds after the process was killed. Together they say which.
     */
    private fun renderHeartbeat() {
        val running = PrhService.running
        val hb = PrhPrefs.getHeartbeat(this)
        if (hb == null) {
            heartbeatText.text = getString(R.string.heartbeat_never)
            heartbeatText.setTextColor(COLOR_MUTED)
            return
        }

        val age = System.currentTimeMillis() - hb.atMs
        val stale = age > STALE_AFTER_MS
        heartbeatText.text = buildString {
            append(if (running) "Service running" else "Service stopped")
            append(" · ")
            append(if (hb.ok) "last contact " else "last failure ")
            append(ago(age))
            append(" at ")
            append(timeFormat.format(Date(hb.atMs)))
            append(" (")
            append(hb.kind)
            append(")")
            if (hb.detail.isNotEmpty()) append("\n").append(hb.detail)
        }
        heartbeatText.setTextColor(
            when {
                !running || !hb.ok -> COLOR_BAD
                stale -> COLOR_WARN
                else -> COLOR_GOOD
            },
        )
    }

    private fun ago(ms: Long): String {
        val s = (ms / 1000).coerceAtLeast(0)
        return when {
            s < 60 -> "${s}s ago"
            s < 3_600 -> "${s / 60}m ago"
            s < 86_400 -> "${s / 3_600}h ago"
            else -> "${s / 86_400}d ago"
        }
    }

    /**
     * Both destinations off is legal — the conversation view still works and
     * still answers prompts — but it is almost never what someone meant to do,
     * so say it once rather than letting an agent block unnoticed.
     */
    private fun warnIfNothingAlerts() {
        if (!watchCheck.isChecked && !notifCheck.isChecked) {
            Toast.makeText(
                this,
                "Nothing will alert you now — prompts only appear inside the app",
                Toast.LENGTH_LONG,
            ).show()
        }
    }

    // -- small view helpers -------------------------------------------------

    private fun heading(label: String) = TextView(this).apply {
        text = label
        textSize = 13f
        setTextColor(COLOR_MUTED)
        setPadding(0, 32, 0, 4)
    }.fullWidth()

    /**
     * The root centres its children horizontally, which leaves a label or a
     * checkbox floating in the middle of the screen at its own measured width.
     * Full-width children left-align their content instead.
     */
    private fun <T : View> T.fullWidth(): T = apply {
        layoutParams = LinearLayout.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT,
            ViewGroup.LayoutParams.WRAP_CONTENT,
        )
    }

    // -- deep link ---------------------------------------------------------

    private fun parseDeepLink(intent: Intent?) {
        val data = intent?.data ?: return
        parsePrhUri(data)
    }

    /**
     * Pulls host/port/key/pin out of a `prh://` URI, whether it arrived as a
     * VIEW intent or was decoded by the in-app scanner. A non-`prh` URI is
     * ignored, so a misread QR is harmless.
     */
    private fun parsePrhUri(data: Uri) {
        if (data.scheme != "prh") return
        deepLinkHost = data.host
        deepLinkPort = data.port
        // `k` is the one-time pairing key from the QR; `f` is the TLS pin that
        // selects https. Both come from the scanned code, never typed.
        deepLinkKey = data.getQueryParameter("k")
        deepLinkPin = data.getQueryParameter("f")
    }

    /**
     * Feeds a scanned code into the same deep-link state a VIEW intent would
     * set, then refreshes the fields. The one-time key is now in memory and
     * `doPair` will spend it on the next tap.
     */
    private fun applyScannedCode(uri: Uri) {
        parsePrhUri(uri)
        prefill()
        if (deepLinkKey == null) {
            Toast.makeText(this, "That code is not a prh pairing link", Toast.LENGTH_SHORT).show()
        }
    }

    // -- scan ---------------------------------------------------------------

    private fun onScanClicked() {
        if (hasCameraPermission()) {
            launchScanner()
        } else {
            cameraPermission.launch(Manifest.permission.CAMERA)
        }
    }

    private fun hasCameraPermission(): Boolean =
        ContextCompat.checkSelfPermission(this, Manifest.permission.CAMERA) ==
            PackageManager.PERMISSION_GRANTED

    private fun launchScanner() {
        val options = ScanOptions().apply {
            // The pairing code is a dense base64url string; QR is the only
            // format the editor emits, so restricting to it stops a stray 1D
            // barcode (a receipt, a book) from being mistaken for a code.
            setDesiredBarcodeFormats(ScanOptions.QR_CODE)
            setPrompt(getString(R.string.scan_prompt))
            setBeepEnabled(false)
            setOrientationLocked(false)
        }
        scanLauncher.launch(options)
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
            sessionsButton.visibility = android.view.View.VISIBLE
            // Also asked here, not only on the pairing path: everyone who
            // paired before this was added is already installed and would
            // otherwise never be asked, leaving notifications permanently dead
            // with no indication why.
            requestNotificationPermission()

            // Start the poll service on every launch, not only on pairing.
            //
            // It was previously started in exactly two places: right after
            // pairing, and on ACTION_BOOT_COMPLETED. Anything that kills the
            // process in between — a reinstall, a force-stop, the user swiping
            // the app away, or the system reclaiming memory — left it dead
            // until the next reboot, with the app still showing "Paired" and
            // quietly receiving nothing. Starting it here is idempotent: if it
            // is already running this is just another onStartCommand.
            PrhService.start(this)
        }
        // The pairing key is never shown in a text field: it is a high-entropy
        // one-time secret that arrived out of band, and putting it in an
        // editable field invites it into clipboards and screenshots. It is
        // held in memory for this one pairing attempt.
        if (deepLinkKey != null) {
            statusText.text = "Ready to pair with the scanned code"
        }
    }

    // -- pairing -----------------------------------------------------------

    private fun doPair() {
        val host = hostField.text.toString().trim()
        val port = portField.text.toString().trim()
        val key = deepLinkKey

        if (host.isEmpty()) {
            Toast.makeText(this, "Host required", Toast.LENGTH_SHORT).show()
            return
        }
        if (key == null) {
            Toast.makeText(this, "Scan a pairing code to pair", Toast.LENGTH_SHORT).show()
            return
        }

        // The pin decides the scheme. There is no negotiation and no fallback:
        // trying https and dropping to http on failure is exactly the downgrade
        // an attacker would induce — and cleartext is disabled in the manifest.
        val pin = deepLinkPin ?: PrhPrefs.getTlsPin(this)
        if (pin == null) {
            Toast.makeText(this, "This code has no TLS pin. Pair from the editor again.", Toast.LENGTH_SHORT).show()
            return
        }
        val baseUrl = "https://$host:$port"

        pairButton.isEnabled = false
        statusText.text = "Pairing…"

        scope.launch {
            try {
                val client = PrhClient(baseUrl, tlsPin = pin)
                val deviceName = PrhPrefs.getDeviceName(this@SettingsActivity)

                // Enrolment is the one exchange whose compromise hands over
                // everything, so take the sealed path: the key is proved by
                // signing rather than sent, and the device secret comes back
                // encrypted under it.
                //
                // What comes back is the device secret, which is what
                // everything afterwards signs with — so this screen is not
                // needed again unless the device is revoked or prh loses its
                // devices file.
                val (deviceId, deviceSecret) =
                    client.registerWithPairingKey(PrhSigning.decode(key), deviceName)

                PrhPrefs.setBaseUrl(this@SettingsActivity, baseUrl)
                PrhPrefs.setCredentials(this@SettingsActivity, deviceId, deviceSecret)
                PrhPrefs.setTlsPin(this@SettingsActivity, pin)

                // One window enrols one device, so the scanned code is spent.
                // Dropping it stops a stale key sitting in memory and stops a
                // second tap on Pair from failing confusingly.
                deepLinkKey = null

                withContext(Dispatchers.Main) {
                    statusText.text = "Paired"
                    sessionsButton.visibility = android.view.View.VISIBLE
                    Toast.makeText(this@SettingsActivity, "Paired", Toast.LENGTH_SHORT).show()
                    requestNotificationPermission()
                    requestBatteryExemption()
                    PrhService.start(this@SettingsActivity)
                }
            } catch (e: javax.net.ssl.SSLHandshakeException) {
                // Almost always the pin: prh regenerated its key, or something
                // other than prh is answering on this address. Say which,
                // because "handshake failed" sends people to the wrong place.
                withContext(Dispatchers.Main) {
                    statusText.text = "prh's TLS key is not the one this code vouches for. " +
                        "Start pairing again in the editor and scan the new code."
                    pairButton.isEnabled = true
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

    /**
     * Asks for POST_NOTIFICATIONS.
     *
     * Declaring it in the manifest is not enough on API 33+: it is a runtime
     * permission, and until it is granted every `notify()` call is dropped on
     * the floor without an error. The manifest declared it and nothing ever
     * asked, so the app could not post a single notification — the prompt
     * alert, the "watch unreachable" fallback, and the foreground service's
     * own notification were all silently discarded.
     *
     * Asked here rather than at first prompt because a permission dialog that
     * appears while an agent is blocked waiting for an approval is the worst
     * possible moment for it.
     */
    private fun requestNotificationPermission() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
            == PackageManager.PERMISSION_GRANTED
        ) {
            return
        }
        notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
    }

    private companion object {
        /**
         * How old a successful contact has to get before it stops counting as
         * healthy. The long-poll uses a 55s wait, so a working connection
         * refreshes the timestamp at least that often; anything past 90s means
         * a poll did not come back when it should have.
         */
        const val STALE_AFTER_MS = 90_000L

        const val COLOR_GOOD = 0xFF2E7D32.toInt()
        const val COLOR_WARN = 0xFFEF6C00.toInt()
        const val COLOR_BAD = 0xFFC62828.toInt()
        const val COLOR_MUTED = 0xFF888888.toInt()
    }
}
