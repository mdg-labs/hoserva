package acme

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	eventCertificateRenewalFailed = "certificate_renewal_failed"
	renewalFailedTitle            = "Certificate renewal failed"
)

// Setup is the user-supplied Let's Encrypt DNS-01 configuration.
type Setup struct {
	Domain               string
	Provider             string
	CloudflareAPIToken   string
	RFC2136Nameserver    string
	RFC2136TSIGKeyName   string
	RFC2136TSIGSecret    string
	RFC2136TSIGAlgorithm string
}

// Status is the non-secret view GET /settings/network returns.
type Status struct {
	Configured bool
	Enabled    bool
	Domain     string
	Provider   string
	HasSecret  bool
	LastError  string
}

// Service stores ACME config (D4), issues via Client, and installs the
// resulting certificate only after a successful issue.
type Service struct {
	Store     *Store
	Cipher    SecretCipher
	Client    Client
	Installer Installer
	Publisher Publisher
	Now       func() time.Time

	// DirectoryURL overrides Let's Encrypt production for tests.
	DirectoryURL string
	// CloudflareAPIBase overrides api.cloudflare.com for tests.
	CloudflareAPIBase string
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	if s == nil || s.Store == nil {
		return Status{}, nil
	}
	cfg, err := s.Store.Get(ctx)
	if err != nil {
		return Status{}, err
	}
	if cfg == nil {
		return Status{}, nil
	}
	return Status{
		Configured: true,
		Enabled:    cfg.Enabled,
		Domain:     cfg.Domain,
		Provider:   cfg.Provider,
		HasSecret:  len(cfg.DNSSecret) > 0,
		LastError:  cfg.LastError,
	}, nil
}

func (s *Service) Configure(ctx context.Context, setup Setup) error {
	if s == nil || s.Store == nil || s.Cipher == nil {
		return fmt.Errorf("acme: service is not configured")
	}
	domain, err := normalizeDomain(setup.Domain)
	if err != nil {
		return err
	}
	provider := strings.ToLower(strings.TrimSpace(setup.Provider))
	if provider != ProviderCloudflare && provider != ProviderRFC2136 {
		return fmt.Errorf("acme: provider must be cloudflare or rfc2136")
	}
	existing, err := s.Store.Get(ctx)
	if err != nil {
		return err
	}

	secret := setup.CloudflareAPIToken
	settings := ProviderSettings{}
	if provider == ProviderRFC2136 {
		secret = setup.RFC2136TSIGSecret
		if setup.RFC2136Nameserver == "" || setup.RFC2136TSIGKeyName == "" {
			return fmt.Errorf("acme: RFC 2136 nameserver and TSIG key name are required")
		}
		settings.Nameserver = setup.RFC2136Nameserver
		settings.TSIGKeyName = setup.RFC2136TSIGKeyName
		settings.TSIGAlgorithm = setup.RFC2136TSIGAlgorithm
		if settings.TSIGAlgorithm == "" {
			settings.TSIGAlgorithm = "hmac-sha256"
		}
	} else if secret == "" && existing != nil && existing.Provider == ProviderCloudflare {
		secret = ""
	}

	var dnsCipher []byte
	if secret != "" {
		dnsCipher, err = s.Cipher.Encrypt([]byte(secret))
		if err != nil {
			return fmt.Errorf("acme: encrypting DNS credential: %w", err)
		}
	} else if existing != nil && len(existing.DNSSecret) > 0 && existing.Provider == provider {
		dnsCipher = existing.DNSSecret
	} else {
		return fmt.Errorf("acme: a DNS credential is required")
	}

	accountCipher := []byte(nil)
	if existing != nil && len(existing.AccountKey) > 0 {
		accountCipher = existing.AccountKey
	} else {
		_, pemBytes, err := accountKeyFromPEM(nil)
		if err != nil {
			return err
		}
		accountCipher, err = s.Cipher.Encrypt(pemBytes)
		if err != nil {
			return fmt.Errorf("acme: encrypting ACME account key: %w", err)
		}
	}

	providerJSON, err := encodeProvider(settings)
	if err != nil {
		return err
	}
	return s.Store.Upsert(ctx, &Config{
		Domain:       domain,
		Provider:     provider,
		ProviderJSON: providerJSON,
		DNSSecret:    dnsCipher,
		AccountKey:   accountCipher,
		Enabled:      true,
		LastError:    "",
	})
}

func (s *Service) Disable(ctx context.Context) error {
	if s == nil || s.Store == nil {
		return fmt.Errorf("acme: service is not configured")
	}
	err := s.Store.SetEnabled(ctx, false, "")
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// Issue talks to ACME, then installs the certificate. A failure never
// calls Installer.Install and never generates a self-signed replacement.
func (s *Service) Issue(ctx context.Context, renew bool) error {
	if s == nil || s.Store == nil || s.Client == nil || s.Installer == nil || s.Cipher == nil {
		return fmt.Errorf("acme: service is not configured")
	}
	cfg, err := s.Store.Get(ctx)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("acme: Let's Encrypt is not configured")
	}
	dnsSecret, err := s.Cipher.Decrypt(cfg.DNSSecret)
	if err != nil {
		return s.fail(ctx, renew, fmt.Errorf("acme: decrypting DNS credential: %w", err))
	}
	accountPEM, err := s.Cipher.Decrypt(cfg.AccountKey)
	if err != nil {
		return s.fail(ctx, renew, fmt.Errorf("acme: decrypting ACME account key: %w", err))
	}
	solver, err := s.solver(cfg, string(dnsSecret))
	if err != nil {
		return s.fail(ctx, renew, err)
	}
	issued, err := s.Client.Issue(ctx, IssueRequest{
		Domain:        cfg.Domain,
		AccountKeyPEM: accountPEM,
		DirectoryURL:  s.DirectoryURL,
		Solver:        solver,
	})
	if err != nil {
		return s.fail(ctx, renew, err)
	}
	if err := s.Installer.Install(issued.CertPEM, issued.KeyPEM); err != nil {
		return s.fail(ctx, renew, fmt.Errorf("acme: installing issued certificate: %w", err))
	}
	if len(issued.AccountKeyPEM) > 0 {
		enc, encErr := s.Cipher.Encrypt(issued.AccountKeyPEM)
		if encErr == nil {
			_ = s.Store.SetAccountKey(ctx, enc)
		}
	}
	_ = s.Store.SetLastError(ctx, "")
	return nil
}

func (s *Service) solver(cfg *Config, secret string) (DNS01Solver, error) {
	switch cfg.Provider {
	case ProviderCloudflare:
		return &CloudflareSolver{APIToken: secret, APIBase: s.CloudflareAPIBase}, nil
	case ProviderRFC2136:
		p, err := decodeProvider(cfg.ProviderJSON)
		if err != nil {
			return nil, err
		}
		return &RFC2136Solver{
			Nameserver:    p.Nameserver,
			TSIGKeyName:   p.TSIGKeyName,
			TSIGSecret:    secret,
			TSIGAlgorithm: p.TSIGAlgorithm,
			Zone:          cfg.Domain,
		}, nil
	default:
		return nil, fmt.Errorf("acme: unknown DNS-01 provider %q", cfg.Provider)
	}
}

func (s *Service) fail(ctx context.Context, renew bool, err error) error {
	_ = s.Store.SetLastError(ctx, err.Error())
	if renew && s.Publisher != nil {
		msg := fmt.Sprintf("Let's Encrypt renewal failed. The existing certificate is still in use and was not replaced with a self-signed certificate. %s", err.Error())
		if pubErr := s.Publisher.Publish(ctx, eventCertificateRenewalFailed, renewalFailedTitle, msg); pubErr != nil {
			return fmt.Errorf("%w (also notifying: %v)", err, pubErr)
		}
	}
	return err
}

// RenewalDue reports whether unattended renewal should enqueue acme_issue.
func (s *Service) RenewalDue(ctx context.Context) (bool, error) {
	if s == nil || s.Store == nil || s.Installer == nil {
		return false, nil
	}
	cfg, err := s.Store.Get(ctx)
	if err != nil {
		return false, err
	}
	if cfg == nil || !cfg.Enabled {
		return false, nil
	}
	view, err := s.Installer.Current()
	if err != nil {
		return false, err
	}
	if view.Kind != KindLetsEncrypt {
		return false, nil
	}
	if cfg.LastError != "" && !cfg.UpdatedAt.IsZero() && s.now().Sub(cfg.UpdatedAt) < 24*time.Hour {
		return false, nil
	}
	until := view.NotAfter.Sub(s.now())
	return until <= RenewLead, nil
}

// Clear drops stored ACME config. Used when the operator regenerates a
// self-signed certificate so unattended renewal cannot overwrite it.
func (s *Service) Clear(ctx context.Context) error {
	if s == nil || s.Store == nil {
		return nil
	}
	return s.Store.Delete(ctx)
}
