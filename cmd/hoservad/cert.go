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
			if err == nil {
				return cert, nil
			}
			// A crash between the key rename and the certificate rename
			// leaves both paths present but not a pair. The previous
			// pair, if installTLSCertificate snapshotted one, is still
			// a recoverable unit.
			if recovered, recErr := recoverTLSCertificatePair(certPath, keyPath); recErr == nil {
				return recovered, nil
			}
			return tls.Certificate{}, fmt.Errorf("loading existing TLS certificate: %w", err)
		}
	}
	if recovered, recErr := recoverTLSCertificatePair(certPath, keyPath); recErr == nil {
		return recovered, nil
	}

	certPEM, keyPEM, err := generateSelfSignedCertificate()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating self-signed certificate: %w", err)
	}
	if _, err := writeTLSCertificatePair(certPath, keyPath, certPEM, keyPEM); err != nil {
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

// tlsInstallProgress records which final paths writeTLSCertificatePair
// has already renamed over. A failure before either rename leaves both
// false, so a caller must not rewrite the existing pair.
type tlsInstallProgress struct {
	keyInstalled  bool
	certInstalled bool
}

// writeTLSCertificatePair persists certPEM/keyPEM at certPath/keyPath.
// Each file is fully written and fsynced in a hidden temp name first;
// only after both are ready are they renamed into place and the parent
// directory fsynced, so a crash mid-write cannot leave a zero-byte or
// truncated file at the final path. The two renames are not one
// operation: progress reports how far the install got so a caller can
// put back only the paths that changed.
func writeTLSCertificatePair(certPath, keyPath string, certPEM, keyPEM []byte) (tlsInstallProgress, error) {
	var progress tlsInstallProgress
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return progress, fmt.Errorf("validating TLS certificate pair: %w", err)
	}
	dir := filepath.Dir(certPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return progress, fmt.Errorf("creating certificate directory: %w", err)
	}

	tmpCertPath, err := writeTLSTempFile(dir, "hoserva.crt", certPEM, 0o644)
	if err != nil {
		return progress, fmt.Errorf("writing certificate: %w", err)
	}
	tmpKeyPath, err := writeTLSTempFile(dir, "hoserva.key", keyPEM, 0o600)
	if err != nil {
		_ = os.Remove(tmpCertPath)
		return progress, fmt.Errorf("writing certificate key: %w", err)
	}

	if err := os.Rename(tmpKeyPath, keyPath); err != nil {
		_ = os.Remove(tmpCertPath)
		_ = os.Remove(tmpKeyPath)
		return progress, fmt.Errorf("installing certificate key: %w", err)
	}
	progress.keyInstalled = true
	if err := os.Rename(tmpCertPath, certPath); err != nil {
		_ = os.Remove(tmpCertPath)
		return progress, fmt.Errorf("installing certificate: %w", err)
	}
	progress.certInstalled = true
	if err := fsyncDir(dir); err != nil {
		return progress, err
	}
	if err := checkTLSKeyPermissions(keyPath); err != nil {
		return progress, err
	}
	return progress, nil
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

// installTLSCertificate replaces certPath/keyPath with certPEM/keyPEM.
// Invalid PEM is rejected before any file is touched. A pair that
// already loads is snapshotted to sibling .bak files first, so a crash
// between the key rename and the certificate rename can be repaired by
// loadOrGenerateTLSCertificate. A failure before either rename leaves
// the existing files untouched — restoring them with os.WriteFile would
// truncate a valid pair.
func installTLSCertificate(certPath, keyPath string, certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("installing TLS certificate: %w", err)
	}
	if err := snapshotTLSPair(certPath, keyPath); err != nil {
		return err
	}

	progress, err := writeTLSCertificatePair(certPath, keyPath, certPEM, keyPEM)
	if err != nil {
		if rbErr := restoreInstalledTLSFiles(certPath, keyPath, progress); rbErr != nil {
			return fmt.Errorf("%w (restoring previous certificate: %v)", err, rbErr)
		}
		return err
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		progress.keyInstalled = true
		progress.certInstalled = true
		if rbErr := restoreInstalledTLSFiles(certPath, keyPath, progress); rbErr != nil {
			return fmt.Errorf("verifying installed TLS certificate: %w (restoring previous: %v)", err, rbErr)
		}
		return fmt.Errorf("verifying installed TLS certificate: %w", err)
	}
	return nil
}

func snapshotTLSPair(certPath, keyPath string) error {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading TLS key: %w", err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil
	}
	if err := writeTLSFileDurable(certPath+".bak", certPEM, 0o644); err != nil {
		return fmt.Errorf("snapshotting TLS certificate: %w", err)
	}
	if err := writeTLSFileDurable(keyPath+".bak", keyPEM, 0o600); err != nil {
		return fmt.Errorf("snapshotting TLS key: %w", err)
	}
	return fsyncDir(filepath.Dir(certPath))
}

func restoreInstalledTLSFiles(certPath, keyPath string, progress tlsInstallProgress) error {
	if !progress.keyInstalled && !progress.certInstalled {
		return nil
	}
	if progress.keyInstalled {
		keyPEM, err := os.ReadFile(keyPath + ".bak")
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("reading TLS key backup: %w", err)
		}
		if err == nil {
			if err := writeTLSFileDurable(keyPath, keyPEM, 0o600); err != nil {
				return fmt.Errorf("restoring TLS key: %w", err)
			}
		}
	}
	if progress.certInstalled {
		certPEM, err := os.ReadFile(certPath + ".bak")
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("reading TLS certificate backup: %w", err)
		}
		if err == nil {
			if err := writeTLSFileDurable(certPath, certPEM, 0o644); err != nil {
				return fmt.Errorf("restoring TLS certificate: %w", err)
			}
		}
	}
	return fsyncDir(filepath.Dir(certPath))
}

// recoverTLSCertificatePair installs the snapshotted pair over the live
// paths when that snapshot itself loads. A missing snapshot is an error
// so the caller can fall through to generating a certificate only when
// no previous pair exists.
func recoverTLSCertificatePair(certPath, keyPath string) (tls.Certificate, error) {
	bakCert, bakKey := certPath+".bak", keyPath+".bak"
	if err := checkTLSKeyPermissions(bakKey); err != nil {
		return tls.Certificate{}, err
	}
	certPEM, err := os.ReadFile(bakCert)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := os.ReadFile(bakKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeTLSFileDurable(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeTLSFileDurable(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := fsyncDir(filepath.Dir(certPath)); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

func writeTLSFileDurable(path string, data []byte, perm os.FileMode) error {
	tmp, err := writeTLSTempFile(filepath.Dir(path), filepath.Base(path), data, perm)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
