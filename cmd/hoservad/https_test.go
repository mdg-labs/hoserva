package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/acme"
)

func TestHTTPSPersist_AtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h := newHTTPSControl(dir, "", "", nil, 8008, false, tls.Certificate{})
	if err := h.SetAllowAllSources(true); err != nil {
		t.Fatal(err)
	}
	if err := h.SetListenPort(8443); err != nil {
		t.Fatal(err)
	}
	got, ok := loadPersistedHTTPS(dir)
	if !ok {
		t.Fatal("expected persisted HTTPS settings")
	}
	if !got.AllowAllSources || got.ListenPort != 8443 {
		t.Fatalf("persisted = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "https-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("https-settings.json mode = %o, want 0600", perm)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "https-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("persisted file is not valid JSON: %s", raw)
	}
}

func TestHTTPSPersist_ReportsError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	h := newHTTPSControl(dir, "", "", nil, 8008, false, tls.Certificate{})
	if err := h.SetAllowAllSources(true); err == nil {
		t.Fatal("persist into a missing directory must fail")
	}
}

func newTestHTTPSControl(t *testing.T) (*httpsControl, string) {
	t.Helper()
	dir := t.TempDir()
	ln, ctrl, err := buildTCPListener(config{stateDir: dir, tcpAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ctrl, filepath.Join(dir, "tls")
}

func servedKey(t *testing.T, h *httpsControl) *ecdsa.PrivateKey {
	t.Helper()
	cert, err := h.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("served private key is %T, want *ecdsa.PrivateKey", cert.PrivateKey)
	}
	return key
}

func TestHTTPSControlRegenerate_MintsFreshKeyAndDropsBackups(t *testing.T) {
	h, tlsDir := newTestHTTPSControl(t)
	initial := servedKey(t, h)

	issuedCert, issuedKey, err := generateNamedCertificate("nas.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Install(issuedCert, issuedKey); err != nil {
		t.Fatal(err)
	}
	installed := servedKey(t, h)
	if installed.Equal(initial) {
		t.Fatal("install did not replace the served key")
	}

	view, err := h.Regenerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Kind != acme.KindSelfSigned {
		t.Fatalf("kind after regenerate = %q, want %q", view.Kind, acme.KindSelfSigned)
	}
	first := servedKey(t, h)
	if first.Equal(initial) || first.Equal(installed) {
		t.Fatal("regenerate served an earlier key pair instead of a fresh one")
	}
	assertNoTLSBackups(t, tlsDir)
	assertDiskMatchesServed(t, h, tlsDir)

	if _, err := h.Regenerate(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := servedKey(t, h)
	if second.Equal(first) || second.Equal(initial) || second.Equal(installed) {
		t.Fatal("a second regenerate did not yield another new key")
	}
	assertNoTLSBackups(t, tlsDir)
	assertDiskMatchesServed(t, h, tlsDir)
}

func TestHTTPSControlRegenerate_IgnoresBackupLeftByEarlierInstall(t *testing.T) {
	h, tlsDir := newTestHTTPSControl(t)
	initial := servedKey(t, h)
	certPath := filepath.Join(tlsDir, "hoserva.crt")
	keyPath := filepath.Join(tlsDir, "hoserva.key")

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath+".bak", certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".bak", keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Regenerate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if servedKey(t, h).Equal(initial) {
		t.Fatal("regenerate restored the key pair held in the backup files")
	}
	assertNoTLSBackups(t, tlsDir)
}

func TestHTTPSControlInstall_RemovesBackupSoMissingPairIsNotResurrected(t *testing.T) {
	h, tlsDir := newTestHTTPSControl(t)
	initial := servedKey(t, h)
	certPath := filepath.Join(tlsDir, "hoserva.crt")
	keyPath := filepath.Join(tlsDir, "hoserva.key")

	issuedCert, issuedKey, err := generateNamedCertificate("nas.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Install(issuedCert, issuedKey); err != nil {
		t.Fatal(err)
	}
	assertNoTLSBackups(t, tlsDir)

	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	cert, err := loadOrGenerateTLSCertificate(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("private key is %T", cert.PrivateKey)
	}
	if key.Equal(initial) {
		t.Fatal("a start with the live pair missing resurrected the key from before the install")
	}
}

func TestRemoveTLSBackups_ReportsWhatItCouldNotRemove(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "hoserva.crt")
	keyPath := filepath.Join(dir, "hoserva.key")
	if err := os.MkdirAll(filepath.Join(certPath+".bak", "stuck"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".bak", []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeTLSBackups(certPath, keyPath); !errors.Is(err, errTLSBackupLeft) {
		t.Fatalf("removeTLSBackups error = %v, want errTLSBackupLeft", err)
	}
	if _, err := os.Stat(keyPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("key backup should still be removed when the certificate's cannot be (stat error %v)", err)
	}
}

// failBackupCleanup makes removeTLSBackups fail the way a stuck delete or a
// failed directory sync would, and restores the real functions afterwards.
func failBackupCleanup(t *testing.T, failRemove, failSync bool) {
	t.Helper()
	origRemove, origSync := removeTLSBackupFile, syncTLSBackupDir
	t.Cleanup(func() { removeTLSBackupFile, syncTLSBackupDir = origRemove, origSync })
	if failRemove {
		removeTLSBackupFile = func(path string) error {
			return &os.PathError{Op: "remove", Path: path, Err: errors.New("input/output error")}
		}
	}
	if failSync {
		syncTLSBackupDir = func(dir string) error {
			return errors.New("syncing directory " + dir + ": input/output error")
		}
	}
}

func TestHTTPSControlInstall_ServesNewPairWhenBackupCleanupFails(t *testing.T) {
	for name, c := range map[string]struct{ remove, sync bool }{
		"removal fails":         {remove: true},
		"directory sync fails":  {sync: true},
		"removal and sync fail": {remove: true, sync: true},
	} {
		t.Run(name, func(t *testing.T) {
			h, tlsDir := newTestHTTPSControl(t)
			initial := servedKey(t, h)
			failBackupCleanup(t, c.remove, c.sync)

			issuedCert, issuedKey, err := generateNamedCertificate("nas.example.com")
			if err != nil {
				t.Fatal(err)
			}
			err = h.Install(issuedCert, issuedKey)
			if !errors.Is(err, errTLSBackupLeft) || !errors.Is(err, acme.ErrBackupLeft) {
				t.Fatalf("Install error = %v, want errTLSBackupLeft", err)
			}
			if servedKey(t, h).Equal(initial) {
				t.Fatal("the old key pair is still served although the new one is on disk")
			}
			assertDiskMatchesServed(t, h, tlsDir)
			view, err := h.Current()
			if err != nil {
				t.Fatal(err)
			}
			if view.Domain != "nas.example.com" {
				t.Fatalf("served certificate domain = %q, want the installed one", view.Domain)
			}
		})
	}
}

func TestHTTPSControlRegenerate_ServesNewPairWhenBackupCleanupFails(t *testing.T) {
	h, tlsDir := newTestHTTPSControl(t)
	initial := servedKey(t, h)
	failBackupCleanup(t, false, true)

	view, err := h.Regenerate(context.Background())
	if !errors.Is(err, acme.ErrBackupLeft) {
		t.Fatalf("Regenerate error = %v, want the backup-left error", err)
	}
	if view.Kind == "" {
		t.Fatal("Regenerate returned no certificate view for the pair it installed")
	}
	if servedKey(t, h).Equal(initial) {
		t.Fatal("the old key pair is still served although the new one is on disk")
	}
	assertDiskMatchesServed(t, h, tlsDir)
}

func TestRemoveTLSBackups_ReportsFailedDirectorySyncAsBackupLeft(t *testing.T) {
	dir := t.TempDir()
	failBackupCleanup(t, false, true)
	err := removeTLSBackups(filepath.Join(dir, "hoserva.crt"), filepath.Join(dir, "hoserva.key"))
	if !errors.Is(err, errTLSBackupLeft) {
		t.Fatalf("removeTLSBackups error = %v, want errTLSBackupLeft", err)
	}
}

func assertNoTLSBackups(t *testing.T, tlsDir string) {
	t.Helper()
	for _, name := range []string{"hoserva.crt.bak", "hoserva.key.bak"} {
		if _, err := os.Stat(filepath.Join(tlsDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (stat error %v)", name, err)
		}
	}
}

func assertDiskMatchesServed(t *testing.T, h *httpsControl, tlsDir string) {
	t.Helper()
	disk, err := tls.LoadX509KeyPair(filepath.Join(tlsDir, "hoserva.crt"), filepath.Join(tlsDir, "hoserva.key"))
	if err != nil {
		t.Fatalf("live pair does not load: %v", err)
	}
	diskKey, _ := disk.PrivateKey.(*ecdsa.PrivateKey)
	if diskKey == nil || !diskKey.Equal(servedKey(t, h)) {
		t.Fatal("the pair on disk is not the pair being served")
	}
}
