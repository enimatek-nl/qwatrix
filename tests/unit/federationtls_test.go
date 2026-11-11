package unit_test

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"github.com/enimatek-nl/qwatrix/internal/config"
)

// TestAFederationCertificateIsGeneratedWhereNoneExists covers the TLS pair the server mints for
// itself in an empty data directory.
//
// Federation is HTTPS, so this is not an optional nicety: a server that cannot produce a certificate
// either refuses to boot or serves federation in the clear. And the pair has to be *loadable*, not
// merely present — the first version of this wrote the key as raw DER into a file named like a PEM
// one, which crypto/tls reported as "failed to find any PEM data in key input" and which every
// container in the fleet would have hit on first start.
func TestAFederationCertificateIsGeneratedWhereNoneExists(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ServerName: "matrix.example", DataDir: dir}

	certFile, keyFile, err := cfg.FederationTLS()
	if err != nil {
		t.Fatalf("FederationTLS: %v", err)
	}
	for _, path := range []string{certFile, keyFile} {
		if filepath.Dir(path) != dir {
			t.Errorf("%s is outside the data directory %s", path, dir)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s exists but is empty", path)
		}
	}

	// The pair has to be usable by the TLS stack, which is the property that was missing.
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("the generated pair cannot be loaded by crypto/tls: %v", err)
	}
	if len(certificate.Certificate) == 0 {
		t.Fatal("the loaded certificate holds no ASN.1 data")
	}

	// A second call reuses what is on disk. Generating a new identity on every start would make
	// every peer's view of this server's key change every restart.
	again, _, err := cfg.FederationTLS()
	if err != nil {
		t.Fatalf("the second call: %v", err)
	}
	first, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("reading the certificate back: %v", err)
	}
	if again != certFile {
		t.Errorf("the second call returned %q, want the same file %q", again, certFile)
	}
	if len(first) == 0 {
		t.Error("the certificate file is empty after the second call")
	}
}

// TestAFederationCertificateNamesTheServerTheConfigAnswersFor keeps the subject alternative name in
// place. Verifiers stopped accepting a bare common name years ago, so a certificate that only sets
// the CN produces a handshake failure that looks exactly like a federation bug.
func TestAFederationCertificateNamesTheServerTheConfigAnswersFor(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ServerName: "matrix.example", DataDir: dir}
	certFile, _, err := cfg.FederationTLS()
	if err != nil {
		t.Fatalf("FederationTLS: %v", err)
	}
	pair, err := tls.LoadX509KeyPair(certFile, filepath.Join(dir, "federation.key"))
	if err != nil {
		t.Fatalf("loading the pair: %v", err)
	}
	if len(pair.Certificate) == 0 {
		t.Fatal("no certificate data")
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("parsing the generated certificate: %v", err)
	}
	if err := certificate.VerifyHostname("matrix.example"); err != nil {
		t.Errorf("the certificate does not name the server this config answers for: %v", err)
	}
	if certificate.NotAfter.Before(certificate.NotBefore) {
		t.Error("the certificate expires before it becomes valid")
	}
}

// TestAHalfGivenCertificatePairIsRefused: -tls-cert without -tls-key cannot mean anything sensible,
// and quietly falling back to a self-signed certificate would serve a different identity than the
// operator's CA-signed one.
func TestAHalfGivenCertificatePairIsRefused(t *testing.T) {
	cfg := &config.Config{
		ServerName:         "matrix.example",
		DataDir:            t.TempDir(),
		TLSCertFile:        "/somewhere/cert.pem",
		EnableRegistration: true,
		RegistrationSecret: "secret",
	}
	if _, _, err := cfg.FederationTLS(); err == nil {
		t.Fatal("a certificate without a key was accepted")
	}
}
