package dev.yyewolf.prh

import android.content.Context
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey

/**
 * Persistent storage for the companion.
 *
 * The device token lives in [EncryptedSharedPreferences] because it is a
 * bearer credential: anyone with it can approve prompts. The cursor is
 * plain prefs — it is not sensitive and encrypted prefs would make its
 * read on every poll unnecessarily slow.
 */
object PrhPrefs {

    private const val FILE_SECURE = "prh_secure"
    private const val FILE_PLAIN = "prh_prefs"

    private const val KEY_TOKEN = "token"
    private const val KEY_BASE_URL = "base_url"
    private const val KEY_DEVICE_ID = "device_id"
    private const val KEY_DEVICE_NAME = "device_name"
    private const val KEY_CURSOR = "cursor"

    private fun securePrefs(context: Context) = EncryptedSharedPreferences.create(
        context,
        FILE_SECURE,
        MasterKey.Builder(context).setKeyScheme(MasterKey.KeyScheme.AES256_GCM).build(),
        EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
        EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
    )

    private fun plainPrefs(context: Context) =
        context.getSharedPreferences(FILE_PLAIN, Context.MODE_PRIVATE)

    // -- token -------------------------------------------------------------

    fun getToken(context: Context): String? =
        securePrefs(context).getString(KEY_TOKEN, null)

    fun setToken(context: Context, token: String?) {
        securePrefs(context).edit().apply {
            if (token == null) remove(KEY_TOKEN) else putString(KEY_TOKEN, token)
        }.apply()
    }

    // -- device info -------------------------------------------------------

    fun getDeviceId(context: Context): String? =
        securePrefs(context).getString(KEY_DEVICE_ID, null)

    fun setDeviceId(context: Context, id: String) {
        securePrefs(context).edit().putString(KEY_DEVICE_ID, id).apply()
    }

    fun getDeviceName(context: Context): String =
        android.os.Build.MODEL

    // -- server URL --------------------------------------------------------

    fun getBaseUrl(context: Context): String? =
        plainPrefs(context).getString(KEY_BASE_URL, null)

    fun setBaseUrl(context: Context, url: String) {
        plainPrefs(context).edit().putString(KEY_BASE_URL, url).apply()
    }

    // -- cursor -------------------------------------------------------------

    fun getCursor(context: Context): Long =
        plainPrefs(context).getLong(KEY_CURSOR, 0L)

    fun setCursor(context: Context, cursor: Long) {
        plainPrefs(context).edit().putLong(KEY_CURSOR, cursor).apply()
    }

    // -- paired? -----------------------------------------------------------

    fun isPaired(context: Context): Boolean =
        getToken(context) != null && getBaseUrl(context) != null
}
