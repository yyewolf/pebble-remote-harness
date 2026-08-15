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
import android.view.View
import android.view.ViewGroup
import android.widget.Button
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
    private lateinit var statusPill: TextView
    private lateinit var deliverySection: LinearLayout
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
        matchSystemBarsToTheme()

        parseDeepLink(intent)

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(20), dp(12), dp(20), dp(40))
        }

        root.addView(titleView(getString(R.string.app_name)), matchWrap())
        root.addSpaced(captionView(getString(R.string.app_tagline)), 4)

        // Connection first, pairing last. Pairing is a one-time act and the
        // thing you actually come back for is "is it working, and what is
        // waiting" — so the screen leads with that. When nothing is paired the
        // delivery section hides itself, which floats pairing back up to
        // directly under the status it explains.
        root.addSpaced(sectionLabel(getString(R.string.connection_heading)), 28)
        root.addSpaced(buildStatusCard(), 8)

        deliverySection = buildDeliverySection()
        root.addSpaced(deliverySection, 0)
        root.addSpaced(sectionLabel(getString(R.string.pairing_heading)), 28)
        root.addSpaced(buildPairingCard(), 8)

        val scroll = ScrollView(this).apply {
            clipToPadding = false
            addView(root, matchWrap())
        }
        setContentView(scroll)
        scroll.padForSystemBars()

        prefill()
    }

    /** Status, health, and the way through to the sessions. */
    private fun buildStatusCard(): LinearLayout {
        statusPill = pill(getString(R.string.not_paired), textSecondary, surfaceAltColor).apply {
            layoutParams = LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT,
                ViewGroup.LayoutParams.WRAP_CONTENT,
            )
        }
        heartbeatText = captionView().apply { textSize = 13f }
        statusText = captionView().apply {
            setTextColor(textSecondary)
            visibility = View.GONE
        }
        sessionsButton = primaryButton(getString(R.string.sessions_button)).apply {
            visibility = View.GONE
            setOnClickListener {
                startActivity(Intent(this@SettingsActivity, SessionsActivity::class.java))
            }
        }

        return card().apply {
            addView(statusPill)
            addSpaced(heartbeatText, 10)
            addSpaced(statusText, 6)
            addSpaced(sessionsButton, 16)
        }
    }

    private fun buildDeliverySection(): LinearLayout {
        val watchRow = switchRow(
            getString(R.string.watch_notifications),
            getString(R.string.watch_notifications_hint),
            PrhPrefs.isWatchEnabled(this),
        ) { checked ->
            PrhPrefs.setWatchEnabled(this, checked)
            warnIfNothingAlerts()
        }
        val notifRow = switchRow(
            getString(R.string.phone_notifications),
            getString(R.string.phone_notifications_hint),
            PrhPrefs.isPhoneNotificationsEnabled(this),
        ) { checked ->
            PrhPrefs.setPhoneNotificationsEnabled(this, checked)
            // Turning this on is worthless while POST_NOTIFICATIONS is denied —
            // notify() is dropped without an error — so the ask happens at the
            // moment the intent is expressed.
            if (checked) requestNotificationPermission()
            warnIfNothingAlerts()
        }

        return LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            visibility = View.GONE
            addSpaced(sectionLabel(getString(R.string.delivery_heading)), 28)
            addSpaced(
                card().apply {
                    addView(watchRow, matchWrap())
                    addView(divider())
                    addView(notifRow, matchWrap())
                },
                8,
            )
        }
    }

    private fun buildPairingCard(): LinearLayout {
        hostField = styledField("192.168.1.10")
        portField = styledField("8477").apply {
            inputType = InputType.TYPE_CLASS_NUMBER
            setText("8477")
        }
        // Scan is the primary action because it is the only one that can
        // start a pairing: `doPair` needs a one-time key, and the only way to
        // get one is off the QR. Pair on its own is the second tap.
        scanButton = primaryButton(getString(R.string.scan_button))
        scanButton.setOnClickListener { onScanClicked() }
        pairButton = secondaryButton("Pair")
        pairButton.setOnClickListener { doPair() }

        return card().apply {
            addView(sectionLabel(getString(R.string.host_label)), matchWrap())
            addSpaced(hostField, 6)
            addSpaced(sectionLabel(getString(R.string.port_label)), 16)
            addSpaced(portField, 6)
            addSpaced(scanButton, 20)
            addSpaced(pairButton, 8)
        }
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
            heartbeatText.setTextColor(textFaint)
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
                !running || !hb.ok -> badColor
                stale -> warnColor
                else -> goodColor
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
        // Read back out of prefs rather than off the two switches: the write
        // has already happened, so this cannot disagree with what the service
        // will do.
        if (!PrhPrefs.isWatchEnabled(this) && !PrhPrefs.isPhoneNotificationsEnabled(this)) {
            Toast.makeText(
                this,
                "Nothing will alert you now — prompts only appear inside the app",
                Toast.LENGTH_LONG,
            ).show()
        }
    }

    // -- status ---------------------------------------------------------------

    /** The badge at the top of the connection card. */
    private fun setStatusPill(label: String, paired: Boolean) {
        statusPill.text = label.uppercase()
        statusPill.setTextColor(if (paired) goodColor else textSecondary)
        statusPill.background = roundedRect(
            if (paired) goodSoft else surfaceAltColor,
            radiusDp = 20,
        )
    }

    /** The line under the badge: progress, or why pairing failed. Hidden when
     *  there is nothing to say, so the card does not keep a stale error. */
    private fun setDetail(message: String?, isError: Boolean = false) {
        if (message.isNullOrBlank()) {
            statusText.visibility = View.GONE
            return
        }
        statusText.visibility = View.VISIBLE
        statusText.text = message
        statusText.setTextColor(if (isError) badColor else textSecondary)
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

        val paired = PrhPrefs.isPaired(this)
        setStatusPill(
            getString(if (paired) R.string.paired else R.string.not_paired),
            paired,
        )
        deliverySection.visibility = if (paired) View.VISIBLE else View.GONE

        if (paired) {
            setDetail(null)
            sessionsButton.visibility = View.VISIBLE
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
            setDetail("Scanned code ready — tap Pair")
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
        setDetail("Pairing…")

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
                    setStatusPill(getString(R.string.paired), paired = true)
                    setDetail(null)
                    sessionsButton.visibility = View.VISIBLE
                    deliverySection.visibility = View.VISIBLE
                    pairButton.isEnabled = true
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
                    setDetail(
                        "prh's TLS key is not the one this code vouches for. " +
                            "Start pairing again in the editor and scan the new code.",
                        isError = true,
                    )
                    pairButton.isEnabled = true
                }
            } catch (e: PrhClient.PairingClosed) {
                withContext(Dispatchers.Main) {
                    setDetail(
                        "Pairing is not open. Run \"Pebble Harness: Pair\" in the editor, then scan again.",
                        isError = true,
                    )
                    pairButton.isEnabled = true
                }
            } catch (e: Exception) {
                withContext(Dispatchers.Main) {
                    setDetail("Pairing failed: ${e.message}", isError = true)
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
    }
}
