package acme

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestProductionClientIssuesDNS01AgainstHttptest(t *testing.T) {
	dir := startTestDirectory(t)
	solver := &FakeSolver{}
	client := &ProductionClient{}
	issued, err := client.Issue(context.Background(), IssueRequest{
		Domain:       "nas.example.com",
		DirectoryURL: dir.URL(),
		Solver:       solver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if issued.Domain != "nas.example.com" {
		t.Fatalf("domain = %s", issued.Domain)
	}
	if len(solver.Presented) == 0 {
		t.Fatal("expected DNS-01 Present")
	}
	if len(solver.Cleaned) == 0 {
		t.Fatal("expected DNS-01 CleanUp")
	}
	cert, err := tls.X509KeyPair(issued.CertPEM, issued.KeyPEM)
	if err != nil {
		t.Fatalf("issued pair: %v", err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.VerifyHostname("nas.example.com"); err != nil {
		t.Fatalf("SAN: %v", err)
	}
	block, _ := pem.Decode(issued.CertPEM)
	if block == nil {
		t.Fatal("expected PEM certificate")
	}
}

func TestProductionClientRefusesHTTP01OnlyDirectory(t *testing.T) {
	dir := startTestDirectory(t)
	dir.forceHTTP01 = true
	client := &ProductionClient{}
	_, err := client.Issue(context.Background(), IssueRequest{
		Domain:       "nas.example.com",
		DirectoryURL: dir.URL(),
		Solver:       &FakeSolver{},
	})
	if err == nil {
		t.Fatal("expected refusal when the CA offers no DNS-01 challenge")
	}
}
