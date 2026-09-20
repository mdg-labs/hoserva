package acme

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/acme"
)

// ProductionClient speaks real ACME DNS-01 via golang.org/x/crypto/acme.
// Tests set DirectoryURL (via IssueRequest) and HTTPClient to httptest;
// they never contact Let's Encrypt.
type ProductionClient struct {
	HTTPClient *http.Client
}

func (c *ProductionClient) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *ProductionClient) Issue(ctx context.Context, req IssueRequest) (*Certificate, error) {
	if req.Solver == nil {
		return nil, fmt.Errorf("acme: DNS-01 solver is required")
	}
	domain, err := normalizeDomain(req.Domain)
	if err != nil {
		return nil, err
	}
	dirURL := req.DirectoryURL
	if dirURL == "" {
		dirURL = ProductionDirectory
	}

	accountKey, accountPEM, err := accountKeyFromPEM(req.AccountKeyPEM)
	if err != nil {
		return nil, err
	}

	client := &acme.Client{
		Key:          accountKey,
		HTTPClient:   c.httpClient(),
		DirectoryURL: dirURL,
	}
	if _, err := client.Register(ctx, &acme.Account{}, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("acme: registering account: %w", err)
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return nil, fmt.Errorf("acme: creating order for %s: %w", domain, err)
	}
	fqdn := challengeFQDN(domain)
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return nil, fmt.Errorf("acme: fetching authorization: %w", err)
		}
		chal := dns01Challenge(authz)
		if chal == nil {
			return nil, fmt.Errorf("acme: authorization for %s offered no DNS-01 challenge; HTTP-01 and TLS-ALPN-01 are not used (Q9)", domain)
		}
		record, err := client.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return nil, fmt.Errorf("acme: DNS-01 record: %w", err)
		}
		if err := req.Solver.Present(ctx, fqdn, record); err != nil {
			return nil, fmt.Errorf("acme: presenting DNS-01 record: %w", err)
		}
		defer func(name, value string) {
			_ = req.Solver.CleanUp(context.Background(), name, value)
		}(fqdn, record)
		if _, err := client.Accept(ctx, chal); err != nil {
			return nil, fmt.Errorf("acme: accepting DNS-01 challenge: %w", err)
		}
		if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
			return nil, fmt.Errorf("acme: waiting for DNS-01 authorization: %w", err)
		}
	}

	if _, err := client.WaitOrder(ctx, order.URI); err != nil {
		return nil, fmt.Errorf("acme: waiting for order: %w", err)
	}

	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("acme: generating certificate key: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domain},
		DNSNames: []string{domain},
	}, certKey)
	if err != nil {
		return nil, fmt.Errorf("acme: creating CSR: %w", err)
	}
	// Re-fetch the order so FinalizeURL is current after WaitOrder.
	order, err = client.GetOrder(ctx, order.URI)
	if err != nil {
		return nil, fmt.Errorf("acme: fetching order: %w", err)
	}
	der, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("acme: finalizing certificate: %w", err)
	}
	certPEM, notAfter, err := encodeCertChain(der)
	if err != nil {
		return nil, err
	}
	keyPEM, err := marshalPKCS8PEM(certKey)
	if err != nil {
		return nil, err
	}
	return &Certificate{
		CertPEM:       certPEM,
		KeyPEM:        keyPEM,
		AccountKeyPEM: accountPEM,
		NotAfter:      notAfter,
		Domain:        domain,
	}, nil
}

func dns01Challenge(authz *acme.Authorization) *acme.Challenge {
	for _, ch := range authz.Challenges {
		if ch.Type == "dns-01" {
			return ch
		}
	}
	return nil
}

func accountKeyFromPEM(pemBytes []byte) (crypto.Signer, []byte, error) {
	if len(bytes.TrimSpace(pemBytes)) == 0 {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, fmt.Errorf("acme: generating account key: %w", err)
		}
		encoded, err := marshalPKCS8PEM(key)
		if err != nil {
			return nil, nil, err
		}
		return key, encoded, nil
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, nil, fmt.Errorf("acme: account key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		ecKey, ecErr := x509.ParseECPrivateKey(block.Bytes)
		if ecErr != nil {
			return nil, nil, fmt.Errorf("acme: parsing account key: %w", err)
		}
		parsed = ecKey
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("acme: account key is not a signer")
	}
	return signer, append([]byte(nil), pemBytes...), nil
}

func marshalPKCS8PEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("acme: marshalling key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func encodeCertChain(ders [][]byte) ([]byte, time.Time, error) {
	if len(ders) == 0 {
		return nil, time.Time{}, fmt.Errorf("acme: CA returned no certificates")
	}
	leaf, err := x509.ParseCertificate(ders[0])
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("acme: parsing issued certificate: %w", err)
	}
	var buf bytes.Buffer
	for _, der := range ders {
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			return nil, time.Time{}, fmt.Errorf("acme: encoding certificate PEM: %w", err)
		}
	}
	return buf.Bytes(), leaf.NotAfter, nil
}
