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

	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
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
	if err := writeTLSCertificatePair(certPath, keyPath, certPEM, keyPEM); err != nil {
		return tls.Certificate{}, err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parsing freshly generated certificate: %w", err)
	}
	return cert, nil
}

func tlsCertViewFromParsed(parsed *x509.Certificate) api.TLSCertView {
	view := api.TLSCertView{NotAfter: parsed.NotAfter}
	if parsed.Subject.CommonName == "hoserva" {
		view.Kind = acme.KindSelfSigned
		return view
	}
	view.Kind = acme.KindLetsEncrypt
	if len(parsed.DNSNames) > 0 {
		view.Domain = parsed.DNSNames[0]
	} else {
		view.Domain = parsed.Subject.CommonName
	}
	return view
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

// writeTLSCertificatePair persists certPEM/keyPEM at certPath/keyPath.
// Each file is fully written and fsynced in a hidden temp name first;
// only after both are ready are they renamed into place and the parent
// directory fsynced, so a crash mid-write cannot leave a zero-byte or
// truncated file at the final path.
func writeTLSCertificatePair(certPath, keyPath string, certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("validating TLS certificate pair: %w", err)
	}
	dir := filepath.Dir(certPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating certificate directory: %w", err)
	}

	tmpCertPath, err := writeTLSTempFile(dir, "hoserva.crt", certPEM, 0o644)
	if err != nil {
		return fmt.Errorf("writing certificate: %w", err)
	}
	tmpKeyPath, err := writeTLSTempFile(dir, "hoserva.key", keyPEM, 0o600)
	if err != nil {
		_ = os.Remove(tmpCertPath)
		return fmt.Errorf("writing certificate key: %w", err)
	}

	if err := os.Rename(tmpKeyPath, keyPath); err != nil {
		_ = os.Remove(tmpCertPath)
		_ = os.Remove(tmpKeyPath)
		return fmt.Errorf("installing certificate key: %w", err)
	}
	if err := os.Rename(tmpCertPath, certPath); err != nil {
		_ = os.Remove(tmpCertPath)
		return fmt.Errorf("installing certificate: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return err
	}
	if err := checkTLSKeyPermissions(keyPath); err != nil {
		return err
	}
	return nil
}

func writeTLSTempFile(dir, base string, data []byte, perm os.FileMode) (string, error) {
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

func fsyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %s for fsync: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing directory %s: %w", dir, err)
	}
	return nil
}

// installTLSCertificate atomically replaces certPath/keyPath. Invalid PEM
// is rejected before any file is touched, so a failed Let's Encrypt
// install cannot leave the listener without a certificate.
func installTLSCertificate(certPath, keyPath string, certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("installing TLS certificate: %w", err)
	}

	oldCert, _ := os.ReadFile(certPath)
	oldKey, _ := os.ReadFile(keyPath)

	restore := func() {
		if len(oldCert) > 0 {
			_ = os.WriteFile(certPath, oldCert, 0o644)
		}
		if len(oldKey) > 0 {
			_ = os.WriteFile(keyPath, oldKey, 0o600)
		}
	}

	if err := writeTLSCertificatePair(certPath, keyPath, certPEM, keyPEM); err != nil {
		restore()
		return err
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		restore()
		return fmt.Errorf("verifying installed TLS certificate: %w", err)
	}
	return nil
}
