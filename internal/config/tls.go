package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// FederationTLS returns the certificate and key the server-server listener should serve,
// creating a self-signed pair in the data directory when none was configured.
//
// Federation is HTTPS by specification, so a homeserver cannot skip this the way an internal HTTP
// service can. A self-signed certificate is not a claim that peers should trust it blindly — they
// will not, and the handshake will fail, which is the correct outcome for an operator who has not
// installed a trusted certificate yet. It is what lets the listener start at all, so that a server
// configured to federate reports a certificate problem over the wire instead of refusing to boot.
//
// A deployment behind a private certificate authority passes -tls-cert/-tls-key instead and this
// code never runs.
func (c *Config) FederationTLS() (certFile, keyFile string, err error) {
	if c.TLSCertFile != "" || c.TLSKeyFile != "" {
		if c.TLSCertFile == "" || c.TLSKeyFile == "" {
			return "", "", fmt.Errorf("both -tls-cert and -tls-key must be given to serve federation with a chosen certificate")
		}
		return c.TLSCertFile, c.TLSKeyFile, nil
	}

	dir := c.DataDir
	if dir == "" {
		return "", "", fmt.Errorf("a data directory is required to generate a federation certificate")
	}
	certFile = filepath.Join(dir, "federation.crt")
	keyFile = filepath.Join(dir, "federation.key")
	if fileExists(certFile) && fileExists(keyFile) {
		return certFile, keyFile, nil
	}
	if err := generateSelfSigned(string(c.ServerName), dir, certFile, keyFile); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// generateSelfSigned writes a fresh certificate valid for one year and naming serverName.
func generateSelfSigned(serverName, dir, certFile, keyFile string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating a federation key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("choosing a certificate serial number: %w", err)
	}
	notBefore := time.Now().Add(-time.Hour) // skew allowance: a peer with a slow clock still accepts it
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: serverName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		// The subject alternative name, not only the common name: verifiers stopped
		// accepting a bare common name years ago, and a certificate that does not name the
		// server produces a handshake failure that looks exactly like a federation bug.
		DNSNames: []string{serverName},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("creating the federation certificate: %w", err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("encoding the federation key: %w", err)
	}
	// Written through a temporary file so a crash cannot leave a half-written certificate
	// that the next start accepts because both files exist.
	if err := writePrivate(keyFile, "EC PRIVATE KEY", keyBytes, 0o600); err != nil {
		return err
	}
	if err := writePrivate(certFile, "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	return nil
}

// The payload is DER and the file has to be PEM: crypto/tls reads a key with tls.X509KeyPair, which
// says "failed to find any PEM data in key input" against a binary key — a failure that looks like a
// corrupt certificate and is really an encoding bug, which is how this one survived until someone
// started a container that had no certificate to find.
func writePrivate(path, blockType string, payload []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: payload})
	if err := os.WriteFile(tmp, encoded, mode); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("installing %s: %w", path, err)
	}
	return nil
}
