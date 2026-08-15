package dev.yyewolf.prh

import java.security.MessageDigest
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import javax.net.ssl.HostnameVerifier
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSession
import javax.net.ssl.SSLSocketFactory
import javax.net.ssl.X509TrustManager

/**
 * TLS for a server with no CA, identified by a key we were told about out of
 * band.
 *
 * prh's certificate is self-signed, so the platform trust store has nothing to
 * say about it and the usual chain validation would reject it outright. What
 * we have instead is better for this situation: the pairing QR carried a
 * SHA-256 of the server's public key, delivered screen-to-camera where nothing
 * on the network could touch it. So we ignore the chain entirely and check that
 * one thing.
 *
 * Two rules, and both are load-bearing:
 *
 *  1. **Pin the key, not the certificate.** The pin is over the DER
 *     SubjectPublicKeyInfo — exactly what [java.security.PublicKey.getEncoded]
 *     returns and exactly what prh hashes. Pinning the whole certificate would
 *     tie it to the expiry date and force every phone to re-pair on reissue.
 *
 *  2. **Hostname verification is off, and that is safe *only* because of 1.**
 *     Normally disabling it is how people accidentally accept any server. Here
 *     the certificate names whatever IPs prh saw when it generated them, and
 *     DHCP moves the daemon around; the name proves nothing either way. The pin
 *     is the identity. If you ever remove the pin check, you must put hostname
 *     verification back in the same commit.
 */
object PrhTls {

    /**
     * Builds a socket factory that trusts exactly one public key.
     *
     * @param pin base64url SHA-256 of the server's DER SubjectPublicKeyInfo,
     *   as carried in the pairing QR's `f` parameter.
     */
    fun socketFactory(pin: String): SSLSocketFactory {
        val expected = PrhSigning.decode(pin)
        require(expected.size == 32) { "TLS pin must be a 32-byte SHA-256" }

        val trust = object : X509TrustManager {
            override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
                val leaf = chain?.firstOrNull()
                    ?: throw CertificateException("prh presented no certificate")

                val actual = MessageDigest.getInstance("SHA-256")
                    .digest(leaf.publicKey.encoded)

                // isEqual is the constant-time comparison; contentEquals is not.
                // A timing oracle on a public value is not much of a prize, but
                // there is no reason to hand one over.
                if (!MessageDigest.isEqual(actual, expected)) {
                    throw CertificateException(
                        "prh's key does not match the pinned one. Either you are talking to " +
                            "the wrong machine, or prh's key was regenerated and this device " +
                            "must pair again.",
                    )
                }
            }

            // We never present a client certificate, and we are not a server.
            override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {
                throw CertificateException("prh clients do not authenticate with certificates")
            }

            // Deliberately empty: there are no CAs in this design. Returning
            // anything here would suggest a chain we do not have and do not use.
            override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
        }

        return SSLContext.getInstance("TLS").apply {
            init(null, arrayOf(trust), null)
        }.socketFactory
    }

    /**
     * Accepts any hostname — see rule 2 above. Safe only alongside the pin,
     * which is the actual identity check.
     */
    val hostnameVerifier = HostnameVerifier { _: String?, _: SSLSession? -> true }
}
