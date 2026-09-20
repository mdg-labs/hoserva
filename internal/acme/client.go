// Package acme issues the daemon's TLS certificate via Let's Encrypt DNS-01
// (doc 01 §7, Q9). The production Client speaks real ACME; tests inject a
// scriptable fake or point DirectoryURL at an in-process httptest server.
// HTTP-01 and TLS-ALPN-01 are never used: ports 80 and 443 are never claimed.
package acme

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// ProductionDirectory is Let's Encrypt's ACME v2 directory.
	ProductionDirectory = "https://acme-v02.api.letsencrypt.org/directory"

	KindSelfSigned  = "self_signed"
	KindLetsEncrypt = "lets_encrypt"

	ProviderCloudflare = "cloudflare"
	ProviderRFC2136    = "rfc2136"

	// RenewLead is how far ahead of expiry unattended renewal starts.
	RenewLead = 30 * 24 * time.Hour
)

// Certificate is a PEM-encoded leaf (and issuer) plus the matching key
// and the ACME account key that issued it.
type Certificate struct {
	CertPEM       []byte
	KeyPEM        []byte
	AccountKeyPEM []byte
	NotAfter      time.Time
	Domain        string
}

// IssueRequest is what Client.Issue needs. DirectoryURL is Let's Encrypt
// production unless a test points it at httptest.
type IssueRequest struct {
	Domain        string
	AccountKeyPEM []byte
	DirectoryURL  string
	Solver        DNS01Solver
}

// Client speaks ACME DNS-01. The production implementation uses
// golang.org/x/crypto/acme; tests inject FakeClient.
type Client interface {
	Issue(ctx context.Context, req IssueRequest) (*Certificate, error)
}

// DNS01Solver presents and removes the DNS-01 TXT record. CleanUp is
// best-effort after Issue returns.
type DNS01Solver interface {
	Present(ctx context.Context, fqdn, value string) error
	CleanUp(ctx context.Context, fqdn, value string) error
}

// CertView is the listener's currently served certificate.
type CertView struct {
	Kind     string
	NotAfter time.Time
	Domain   string
}

// Installer swaps the listener's certificate files. Install must not
// generate a self-signed fallback: a failed call leaves the previous
// files in place.
type Installer interface {
	Current() (CertView, error)
	Install(certPEM, keyPEM []byte) error
}

// Publisher delivers a catalog notification (renewal failure).
type Publisher interface {
	Publish(ctx context.Context, event, title, message string) error
}

// SecretCipher encrypts ACME account keys and DNS credentials (Q28).
type SecretCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

func normalizeDomain(domain string) (string, error) {
	d := strings.TrimSpace(strings.ToLower(domain))
	d = strings.TrimSuffix(d, ".")
	if d == "" {
		return "", fmt.Errorf("acme: domain is required")
	}
	if strings.ContainsAny(d, " /:\\*") || strings.Contains(d, "..") {
		return "", fmt.Errorf("acme: %q is not a DNS-01 hostname", domain)
	}
	if strings.Count(d, ".") < 1 {
		return "", fmt.Errorf("acme: %q is not a DNS-01 hostname", domain)
	}
	return d, nil
}

func challengeFQDN(domain string) string {
	return "_acme-challenge." + strings.TrimSuffix(domain, ".")
}
