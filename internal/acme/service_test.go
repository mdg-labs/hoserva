package acme

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "acme.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

func TestIssueFailureDoesNotInstallCertificate(t *testing.T) {
	db := newTestDB(t)
	st := NewStore(db)
	installer := &RecordingInstaller{View: CertView{Kind: KindSelfSigned, NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour)}}
	pub := &FakePublisher{}
	client := &FakeClient{IssueFn: func(context.Context, IssueRequest) (*Certificate, error) {
		return nil, errors.New("directory unreachable")
	}}
	svc := &Service{Store: st, Cipher: FakeCipher{}, Client: client, Installer: installer, Publisher: pub}
	if err := svc.Configure(context.Background(), Setup{
		Domain:             "nas.example.com",
		Provider:           ProviderCloudflare,
		CloudflareAPIToken: "token",
	}); err != nil {
		t.Fatal(err)
	}

	err := svc.Issue(context.Background(), true)
	if err == nil {
		t.Fatal("expected issue to fail")
	}
	if installer.Installs != 0 {
		t.Fatalf("Install called %d times on a failed renew; the existing cert must stay", installer.Installs)
	}
	if installer.View.Kind != KindSelfSigned {
		t.Fatalf("kind = %s, want self_signed still served", installer.View.Kind)
	}
	if len(pub.Events) != 1 || pub.Events[0][0] != eventCertificateRenewalFailed {
		t.Fatalf("notify events = %#v, want certificate_renewal_failed", pub.Events)
	}
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.LastError == "" {
		t.Fatal("lastError should record the failure")
	}
}

func TestIssueSuccessInstallsLetsEncryptCert(t *testing.T) {
	db := newTestDB(t)
	st := NewStore(db)
	installer := &RecordingInstaller{View: CertView{Kind: KindSelfSigned, NotAfter: time.Now().Add(time.Hour)}}
	client := &FakeClient{IssueFn: func(context.Context, IssueRequest) (*Certificate, error) {
		return &Certificate{
			CertPEM:  []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"),
			KeyPEM:   []byte("-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----"),
			NotAfter: time.Now().Add(90 * 24 * time.Hour),
			Domain:   "nas.example.com",
		}, nil
	}}
	svc := &Service{Store: st, Cipher: FakeCipher{}, Client: client, Installer: installer}
	if err := svc.Configure(context.Background(), Setup{
		Domain:             "nas.example.com",
		Provider:           ProviderCloudflare,
		CloudflareAPIToken: "token",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Issue(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if installer.Installs != 1 {
		t.Fatalf("Installs = %d, want 1", installer.Installs)
	}
	if installer.View.Kind != KindLetsEncrypt {
		t.Fatalf("kind = %s, want lets_encrypt", installer.View.Kind)
	}
}

func TestRenewalDueOnlyForArmedLetsEncrypt(t *testing.T) {
	db := newTestDB(t)
	st := NewStore(db)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	installer := &RecordingInstaller{View: CertView{
		Kind:     KindLetsEncrypt,
		NotAfter: now.Add(10 * 24 * time.Hour),
		Domain:   "nas.example.com",
	}}
	svc := &Service{Store: st, Cipher: FakeCipher{}, Installer: installer, Now: func() time.Time { return now }}
	if err := svc.Configure(context.Background(), Setup{
		Domain:             "nas.example.com",
		Provider:           ProviderCloudflare,
		CloudflareAPIToken: "token",
	}); err != nil {
		t.Fatal(err)
	}
	due, err := svc.RenewalDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("expected renewal due with 10 days remaining")
	}
	if err := svc.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	due, err = svc.RenewalDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Fatal("disabled renewal must not be due")
	}
}

func TestConfigureEncryptsSecrets(t *testing.T) {
	db := newTestDB(t)
	st := NewStore(db)
	svc := &Service{Store: st, Cipher: FakeCipher{}}
	if err := svc.Configure(context.Background(), Setup{
		Domain:             "nas.example.com",
		Provider:           ProviderCloudflare,
		CloudflareAPIToken: "super-secret-token",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := st.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.DNSSecret) == "super-secret-token" {
		t.Fatal("DNS secret stored in plaintext")
	}
	plain, err := FakeCipher{}.Decrypt(cfg.DNSSecret)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "super-secret-token" {
		t.Fatalf("decrypted = %q", plain)
	}
	if len(cfg.AccountKey) == 0 {
		t.Fatal("account key missing")
	}
}
