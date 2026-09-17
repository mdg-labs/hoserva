package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// errTLSKeyReadable mirrors internal/auth's own ErrKeyReadable for the
// machine key: this key signs the TLS listener's certificate, and
// deserves the same "root, 0600" enforcement, not just documentation of
// it — checked here on every load, not only right after this file
// generates one itself.
var errTLSKeyReadable = fmt.Errorf("TLS key file must not be group- or world-readable (mode 0600)")

func checkTLSKeyPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat TLS key at %s: %w", path, err)
	}
	if info.Mode().Perm()&(fs.ModePerm&0o077) != 0 {
		return fmt.Errorf("%s: %w", path, errTLSKeyReadable)
	}
	return nil
}

// certLifetime is generous on purpose: this is a self-signed certificate
// (Q9) a browser will always warn about anyway, so there is no rotation
// story to build for it yet — regenerating it (deleting the two files
// under the state directory) is the escape hatch until one exists.
const certLifetime = 10 * 365 * 24 * time.Hour

// loadOrGenerateTLSCertificate reads certPath/keyPath, generating a fresh
// self-signed ECDSA certificate the first time either is missing (Q9:
// "a self-signed certificate on first start"), and reusing it on every
// later start. keyPath is written 0600, exactly like the machine key —
// nothing about how it's protected differs just because it signs a
// certificate instead of encrypting a database column.
func loadOrGenerateTLSCertificate(certPath, keyPath string) (tls.Certificate, error) {
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			if err := checkTLSKeyPermissions(keyPath); err != nil {
				return tls.Certificate{}, err
			}
			cert, err := tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				return tls.Certificate{}, fmt.Errorf("loading existing TLS certificate: %w", err)
			}
			return cert, nil
		}
	}

	certPEM, keyPEM, err := generateSelfSignedCertificate()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating self-signed certificate: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("creating certificate directory: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("writing certificate: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("writing certificate key: %w", err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parsing freshly generated certificate: %w", err)
	}
	return cert, nil
}

func generateSelfSignedCertificate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating ECDSA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generating serial number: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "hoserva"},
		NotBefore:    now.Add(-time.Hour), // tolerate modest clock skew
		NotAfter:     now.Add(certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
		DNSNames:     []string{"hoserva", "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshalling key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM, nil
}
