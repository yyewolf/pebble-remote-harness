package dev.yyewolf.prh

import android.content.Context
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey

/**
 * Persistent storage for the companion.
 *
 * The device secret lives in [EncryptedSharedPreferences]: it is the credential
 * that lets this phone approve shell commands, and it is the one thing that
 * must survive both a phone reboot and a prh restart. Remembering it is what
 * keeps pairing a one-time act: a scanned pairing key is spent at enrolment
 * and never needed again.
 *
 * The session key is deliberately *not* stored. It is short-lived, cheap to
 * re-obtain, and worthless once prh restarts, so writing it to disk would add
 * a persistent copy of a credential for no benefit.
 *
 * Nor is the pairing passphrase stored. Storing it would mean holding a secret
 * that can enrol *new* devices, in order to solve a problem the device secret
 * already solves.
 *
 * The cursor is plain prefs — not sensitive, and read on every poll.
 */
object PrhPrefs {

    private const val FILE_SECURE = "prh_secure"
    private const val FILE_PLAIN = "prh_prefs"

    private const val KEY_DEVICE_SECRET = "device_secret"
    private const val KEY_BASE_URL = "base_url"
    private const val KEY_DEVICE_ID = "device_id"
    private const val KEY_CURSOR = "cursor"
    private const val KEY_TLS_PIN = "tls_pin"

    private fun securePrefs(context: Context) = EncryptedSharedPreferences.create(
        context,
        FILE_SECURE,
        MasterKey.Builder(context).setKeyScheme(MasterKey.KeyScheme.AES256_GCM).build(),
        EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
        EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
    )

    private fun plainPrefs(context: Context) =
        context.getSharedPreferences(FILE_PLAIN, Context.MODE_PRIVATE)

    // -- device credentials --------------------------------------------------

    /** base64url, as returned by POST /v1/register. Never transmitted. */
    fun getDeviceSecret(context: Context): String? =
        securePrefs(context).getString(KEY_DEVICE_SECRET, null)

    fun getDeviceId(context: Context): String? =
        securePrefs(context).getString(KEY_DEVICE_ID, null)

    /** Stores both halves of the pairing, or clears both. */
    fun setCredentials(context: Context, deviceId: String?, deviceSecret: String?) {
        securePrefs(context).edit().apply {
            if (deviceId == null) remove(KEY_DEVICE_ID) else putString(KEY_DEVICE_ID, deviceId)
            if (deviceSecret == null) remove(KEY_DEVICE_SECRET) else putString(KEY_DEVICE_SECRET, deviceSecret)
        }.apply()
    }

    fun getDeviceName(context: Context): String =
        android.os.Build.MODEL

    // -- server URL --------------------------------------------------------

    fun getBaseUrl(context: Context): String? =
        plainPrefs(context).getString(KEY_BASE_URL, null)

    fun setBaseUrl(context: Context, url: String) {
        plainPrefs(context).edit().putString(KEY_BASE_URL, url).apply()
    }

    // -- TLS pin -------------------------------------------------------------

    /**
     * base64url SHA-256 of prh's public key, learned from the pairing QR.
     *
     * Not a secret — every TLS client is handed the certificate anyway — but
     * it is integrity-critical: whoever can change this value chooses which
     * server the phone trusts. It lives in the encrypted store for that
     * reason, not for confidentiality.
     */
    fun getTlsPin(context: Context): String? =
        securePrefs(context).getString(KEY_TLS_PIN, null)

    fun setTlsPin(context: Context, pin: String?) {
        securePrefs(context).edit().apply {
            if (pin == null) remove(KEY_TLS_PIN) else putString(KEY_TLS_PIN, pin)
        }.apply()
    }

    // -- cursor -------------------------------------------------------------

    fun getCursor(context: Context): Long =
        plainPrefs(context).getLong(KEY_CURSOR, 0L)

    fun setCursor(context: Context, cursor: Long) {
        plainPrefs(context).edit().putLong(KEY_CURSOR, cursor).apply()
    }

    // -- paired? -----------------------------------------------------------

    /**
     * Paired means we can obtain a session without the user: a device ID, its
     * secret, and somewhere to send them.
     */
    fun isPaired(context: Context): Boolean =
        getDeviceId(context) != null &&
            getDeviceSecret(context) != null &&
            getBaseUrl(context) != null &&
            // A base URL without a pin cannot connect: the companion only
            // talks pinned https, so a pairing missing the pin is not one.
            getTlsPin(context) != null
}
