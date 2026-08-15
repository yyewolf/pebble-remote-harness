// Package tlsconf generates and loads the self-signed certificate that
// secures Hop 1.
//
// There is no CA and no chain of trust here, deliberately. The companion pins
// the certificate's public key, learning the expected value out of band from
// the pairing QR — a channel nothing on the network can reach. That makes the
// usual PKI machinery irrelevant: no CA to trust, no hostname to match, no
// expiry to renew around.
//
// Two consequences follow from pinning the *key* rather than the certificate,
// and both are the reason it is done that way:
//
//   - the certificate can be regenerated (new SANs, longer validity) without
//     breaking a single paired phone, as long as the key is kept
//   - the daemon's IP can change with DHCP and nothing cares, because nobody
//     is verifying a hostname against it
package tlsconf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// P-256 rather than Ed25519.
//
// Ed25519 makes a smaller certificate, which looks attractive until you check
// the other end: the companion targets minSdk 26, and Android's TLS stack
// cannot verify Ed25519 certificates that far back. A handshake that fails on
// the user's phone is a worse outcome than a certificate a hundred bytes
// larger. The size argument is void anyway — what travels in the QR is a
// 32-byte hash, identical for either algorithm.
//
// P-256 is supported by every Android version this app runs on.
const certValidity = 10 * 365 * 24 * time.Hour

// EnsureCert loads the certificate at certPath, generating one if it is
// missing, and returns it with its pin.
//
// Regenerating is safe for paired devices only while the key survives, so the
// key file is never overwritten once it exists: if the certificate is missing
// but the key is present, a new certificate is issued around the same key and
// the pin does not move.
func EnsureCert(certPath, keyPath string) (tls.Certificate, string, error) {
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return loadCert(certPath, keyPath)
		}
	}

	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writeCert(certPath, key); err != nil {
		return tls.Certificate{}, "", err
	}
	return loadCert(certPath, keyPath)
}

func loadCert(certPath, keyPath string) (tls.Certificate, string, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("tlsconf: loading key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("tlsconf: parsing certificate: %w", err)
	}
	pair.Leaf = leaf
	return pair, Pin(leaf), nil
}

// Pin is the value the companion compares against.
//
// SHA-256 over the DER SubjectPublicKeyInfo — the exact bytes Android's
// PublicKey.getEncoded() returns, which is what makes the two sides
// comparable without either having to parse the other's certificate format.
// Hashing the whole certificate instead would tie the pin to the expiry date
// and force every phone to re-pair on renewal.
func Pin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func loadOrCreateKey(keyPath string) (*ecdsa.PrivateKey, error) {
	if buf, err := os.ReadFile(keyPath); err == nil {
		block, _ := pem.Decode(buf)
		if block == nil {
			return nil, fmt.Errorf("tlsconf: %s is not PEM", keyPath)
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("tlsconf: parsing %s: %w", keyPath, err)
		}
		return key, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tlsconf: generating key: %w", err)
	}

	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("tlsconf: marshalling key: %w", err)
	}
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func writeCert(certPath string, key *ecdsa.PrivateKey) error {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("tlsconf: generating serial: %w", err)
	}

	host, _ := os.Hostname()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "prh", Organization: []string{"Pebble Remote Harness"}},
		NotBefore:    time.Now().Add(-time.Hour), // tolerate a skewed clock at first start
		// Long-lived on purpose. Expiry is a CA-world mechanism for bounding
		// the damage of a key you cannot revoke; here the user revokes by
		// re-pairing, and a certificate that expires would strand every phone
		// on a day nobody chose.
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames(host),
		IPAddresses:           localIPs(),
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("tlsconf: creating certificate: %w", err)
	}
	return writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func dnsNames(host string) []string {
	names := []string{"localhost", "prh"}
	if host != "" && host != "localhost" {
		names = append(names, host)
	}
	return names
}

// localIPs collects every address the phone might reach us on.
//
// Cosmetic for the companion, which pins the key and does not check names at
// all — but it keeps the certificate usable by ordinary clients (curl, a
// browser) that do, and costs nothing. It is explicitly *not* load-bearing:
// the address list goes stale the moment DHCP changes its mind, and the design
// does not care.
func localIPs() []net.IP {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		ips = append(ips, ipnet.IP)
	}
	return ips
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("tlsconf: creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("tlsconf: writing %s: %w", path, err)
	}
	return nil
}
