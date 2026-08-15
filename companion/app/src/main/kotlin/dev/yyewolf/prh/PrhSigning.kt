package dev.yyewolf.prh

import android.util.Base64
import java.security.MessageDigest
import java.security.SecureRandom
import javax.crypto.Cipher
import javax.crypto.Mac
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

/**
 * The client half of the Hop 1 signing scheme. See docs/protocol.md.
 *
 * Nothing here is novel; it is HMAC-SHA256 over a canonical string, plus
 * HKDF-SHA256 and AES-256-GCM to unwrap a session key. All of it is in the
 * Android platform library, so the companion needs no crypto dependency —
 * which matters, because a dependency here would be a dependency in the
 * process that holds the key to approving shell commands.
 *
 * Every constant and every line of the canonical string must match
 * api/internal/auth exactly. A mismatch shows up as a uniform 401 with no
 * indication of which field disagreed, so change both sides together.
 */
object PrhSigning {

    const val SCHEME = "PRH1"
    const val HEADER_KEY = "X-Prh-Key"
    const val HEADER_DATE = "X-Prh-Date"
    const val HEADER_NONCE = "X-Prh-Nonce"
    const val HEADER_SIG = "X-Prh-Sig"
    const val HEADER_SERVER_TIME = "X-Prh-Time"

    private const val HKDF_INFO = "prh-session-wrap-v1"
    private const val PAIRING_INFO = "prh-pairing-wrap-v1"

    /** Literal key ID used when signing an enrolment with the pairing key. */
    const val PAIRING_KEY_ID = "pair"
    private const val NONCE_BYTES = 16
    private const val GCM_TAG_BITS = 128

    private val random = SecureRandom()

    /** base64url without padding, matching Go's RawURLEncoding. */
    private const val B64 = Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP

    fun encode(raw: ByteArray): String = Base64.encodeToString(raw, B64)

    fun decode(s: String): ByteArray = Base64.decode(s, B64)

    fun newNonce(): String {
        val raw = ByteArray(NONCE_BYTES)
        random.nextBytes(raw)
        return encode(raw)
    }

    /**
     * Builds the string to sign.
     *
     * [path] must be the path *and query* exactly as sent — the server
     * canonicalises its own RequestURI, so a dropped `?cursor=` is a 401.
     */
    fun canonical(method: String, path: String, date: String, nonce: String, body: ByteArray?): String {
        val digest = MessageDigest.getInstance("SHA-256").digest(body ?: ByteArray(0))
        return listOf(
            SCHEME,
            method,
            path,
            date,
            nonce,
            digest.joinToString("") { "%02x".format(it) },
        ).joinToString("\n")
    }

    fun sign(key: ByteArray, canonical: String): String {
        val mac = Mac.getInstance("HmacSHA256")
        mac.init(SecretKeySpec(key, "HmacSHA256"))
        return encode(mac.doFinal(canonical.toByteArray(Charsets.UTF_8)))
    }

    /**
     * Recovers the session key from a login response.
     *
     * The key ID is the AEAD additional data, so a response whose key ID was
     * swapped in flight fails to open rather than binding a good key to the
     * wrong session.
     */
    fun unwrapSessionKey(
        deviceSecret: ByteArray,
        wrapSalt: String,
        wrapNonce: String,
        wrappedKey: String,
        keyId: String,
    ): ByteArray = unwrap(deviceSecret, wrapSalt, wrapNonce, wrappedKey, keyId, HKDF_INFO)

    /**
     * Recovers the device secret from a sealed enrolment response.
     *
     * This is what keeps the one dangerous exchange safe. The pairing key came
     * off the screen via the camera, a channel nothing on the network can
     * touch, so an attacker who captured the whole enrolment holds a signature
     * and a sealed blob and can do nothing with either.
     *
     * The device ID is the additional data: a response whose ID was swapped in
     * flight fails to open rather than binding a good secret to an identity
     * prh never issued.
     */
    fun unwrapDeviceSecret(
        pairingKey: ByteArray,
        wrapSalt: String,
        wrapNonce: String,
        wrapSecret: String,
        deviceId: String,
    ): ByteArray = unwrap(pairingKey, wrapSalt, wrapNonce, wrapSecret, deviceId, PAIRING_INFO)

    /**
     * Shared AES-256-GCM open under an HKDF-derived key.
     *
     * [info] is what keeps the session wrap and the pairing wrap apart. Passing
     * the wrong one yields a key that simply never opens anything, so a mix-up
     * fails loudly rather than crossing the two purposes.
     */
    private fun unwrap(
        secret: ByteArray,
        wrapSalt: String,
        wrapNonce: String,
        sealed: String,
        aad: String,
        info: String,
    ): ByteArray {
        val wrapKey = hkdf(secret, decode(wrapSalt), info.toByteArray(Charsets.UTF_8), 32)
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(
            Cipher.DECRYPT_MODE,
            SecretKeySpec(wrapKey, "AES"),
            GCMParameterSpec(GCM_TAG_BITS, decode(wrapNonce)),
        )
        cipher.updateAAD(aad.toByteArray(Charsets.UTF_8))
        return cipher.doFinal(decode(sealed))
    }

    /**
     * HKDF-SHA256, extract-then-expand, per RFC 5869.
     *
     * Hand-rolled because Android has no HKDF in the platform library before
     * API 35 and pulling in a crypto library for thirty lines is a worse
     * trade. It is only ever called with [length] <= 32, so the expand loop
     * runs once, but it is written generally rather than hardcoded to one
     * block — a truncated second block would be silently wrong.
     */
    private fun hkdf(secret: ByteArray, salt: ByteArray, info: ByteArray, length: Int): ByteArray {
        val mac = Mac.getInstance("HmacSHA256")

        // Extract: the salt is the key here, not the secret. Reversing these
        // produces plausible-looking bytes that will never match the server.
        mac.init(SecretKeySpec(salt, "HmacSHA256"))
        val prk = mac.doFinal(secret)

        // Expand.
        mac.init(SecretKeySpec(prk, "HmacSHA256"))
        val out = ByteArray(length)
        var block = ByteArray(0)
        var pos = 0
        var counter = 1
        while (pos < length) {
            mac.update(block)
            mac.update(info)
            mac.update(counter.toByte())
            block = mac.doFinal()
            val n = minOf(block.size, length - pos)
            block.copyInto(out, pos, 0, n)
            pos += n
            counter++
        }
        return out
    }
}
