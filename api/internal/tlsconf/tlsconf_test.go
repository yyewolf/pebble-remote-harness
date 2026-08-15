package tlsconf

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func paths(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

func TestEnsureCertGeneratesAndReloads(t *testing.T) {
	certPath, keyPath := paths(t)

	first, pin1, err := EnsureCert(certPath, keyPath)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if pin1 == "" {
		t.Fatal("empty pin")
	}
	if _, ok := first.Leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("key is %T, want *ecdsa.PublicKey — Android cannot verify Ed25519 at minSdk 26", first.Leaf.PublicKey)
	}

	// A second call must reuse what is on disk, not mint a new identity.
	_, pin2, err := EnsureCert(certPath, keyPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if pin1 != pin2 {
		t.Fatal("pin changed on reload; every paired phone would have to re-pair")
	}
}

// The whole point of pinning the key rather than the certificate: prh can
// reissue the certificate and paired devices keep working.
func TestPinSurvivesCertificateReissue(t *testing.T) {
	certPath, keyPath := paths(t)

	_, before, err := EnsureCert(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// Lose the certificate but keep the key, which is what a reissue looks
	// like.
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}

	_, after, err := EnsureCert(certPath, keyPath)
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if before != after {
		t.Fatal("reissuing the certificate moved the pin")
	}
}

// The pin must be SHA-256 over the DER SubjectPublicKeyInfo, because that is
// exactly what Android's PublicKey.getEncoded() returns. If this drifts, the
// phone rejects every connection with no useful diagnostic.
func TestPinIsSha256OverSPKI(t *testing.T) {
	certPath, keyPath := paths(t)
	cert, pin, err := EnsureCert(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}

	spki, err := x509.MarshalPKIXPublicKey(cert.Leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)
	want := base64.RawURLEncoding.EncodeToString(sum[:])

	if pin != want {
		t.Fatalf("pin = %s, want %s (SHA-256 of the marshalled public key)", pin, want)
	}
}

func TestKeyFileIsNotWorldReadable(t *testing.T) {
	certPath, keyPath := paths(t)
	if _, _, err := EnsureCert(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("key mode = %o, want 600", mode)
	}
}

// End to end: a client that pins this key completes a handshake, and one that
// pins anything else does not.
func TestPinnedClientConnects(t *testing.T) {
	certPath, keyPath := paths(t)
	cert, pin, err := EnsureCert(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	// Mirrors what the companion's TrustManager does: ignore the chain, check
	// the pin, ignore the hostname.
	pinning := func(expected string) *http.Client {
		return &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // replaced by the pin check below
				VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					leaf, err := x509.ParseCertificate(rawCerts[0])
					if err != nil {
						return err
					}
					if Pin(leaf) != expected {
						return errPinMismatch
					}
					return nil
				},
			},
		}}
	}

	resp, err := pinning(pin).Get(srv.URL)
	if err != nil {
		t.Fatalf("correct pin was rejected: %v", err)
	}
	resp.Body.Close()

	if _, err := pinning("not-the-right-pin").Get(srv.URL); err == nil {
		t.Fatal("a wrong pin completed the handshake")
	}
}

var errPinMismatch = &pinError{}

type pinError struct{}

func (*pinError) Error() string { return "pin mismatch" }
