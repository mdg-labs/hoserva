package acme

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const defaultTSIGAlgorithm = dns.HmacSHA256

// RFC2136Solver presents DNS-01 TXT records via RFC 2136 dynamic updates.
type RFC2136Solver struct {
	Nameserver    string
	TSIGKeyName   string
	TSIGSecret    string
	TSIGAlgorithm string
	Zone          string
	Exchanger     rfc2136Exchanger
}

type rfc2136Exchanger interface {
	ExchangeContext(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error)
}

func (s *RFC2136Solver) Present(ctx context.Context, fqdn, value string) error {
	return s.update(ctx, fqdn, value, true)
}

func (s *RFC2136Solver) CleanUp(ctx context.Context, fqdn, value string) error {
	return s.update(ctx, fqdn, value, false)
}

func (s *RFC2136Solver) update(ctx context.Context, fqdn, value string, insert bool) error {
	if s.Nameserver == "" || s.TSIGKeyName == "" || s.TSIGSecret == "" {
		return fmt.Errorf("acme: RFC 2136 nameserver, TSIG key name and secret are required")
	}
	name := dns.Fqdn(fqdn)
	zone := s.Zone
	if zone == "" {
		zone = dns.Fqdn(strings.TrimPrefix(strings.TrimSuffix(fqdn, "."), "_acme-challenge."))
	} else {
		zone = dns.Fqdn(zone)
	}
	rr, err := dns.NewRR(fmt.Sprintf("%s 120 IN TXT %q", name, value))
	if err != nil {
		return fmt.Errorf("acme: building RFC 2136 TXT record: %w", err)
	}
	msg := new(dns.Msg)
	msg.SetUpdate(zone)
	if insert {
		msg.Insert([]dns.RR{rr})
	} else {
		msg.Remove([]dns.RR{rr})
	}
	algo := s.TSIGAlgorithm
	if algo == "" {
		algo = defaultTSIGAlgorithm
	}
	keyName := dns.Fqdn(s.TSIGKeyName)
	msg.SetTsig(keyName, algo, 300, time.Now().Unix())

	client := s.Exchanger
	if client == nil {
		c := new(dns.Client)
		c.TsigSecret = map[string]string{keyName: s.TSIGSecret}
		client = c
	}
	addr := s.Nameserver
	if !strings.Contains(addr, ":") {
		addr += ":53"
	}
	resp, _, err := client.ExchangeContext(ctx, msg, addr)
	if err != nil {
		return fmt.Errorf("acme: RFC 2136 update: %w", err)
	}
	if resp != nil && resp.Rcode != dns.RcodeSuccess {
		return fmt.Errorf("acme: RFC 2136 update rcode %s", dns.RcodeToString[resp.Rcode])
	}
	return nil
}
