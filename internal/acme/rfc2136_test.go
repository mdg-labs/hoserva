package acme

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type fakeExchanger struct {
	msgs []*dns.Msg
}

func (f *fakeExchanger) ExchangeContext(_ context.Context, m *dns.Msg, _ string) (*dns.Msg, time.Duration, error) {
	f.msgs = append(f.msgs, m.Copy())
	resp := new(dns.Msg)
	resp.SetReply(m)
	resp.Rcode = dns.RcodeSuccess
	return resp, 0, nil
}

func TestRFC2136SolverSendsUpdate(t *testing.T) {
	ex := &fakeExchanger{}
	s := &RFC2136Solver{
		Nameserver:  "192.0.2.53:53",
		TSIGKeyName: "key.example.com.",
		TSIGSecret:  "c2VjcmV0",
		Zone:        "example.com",
		Exchanger:   ex,
	}
	if err := s.Present(context.Background(), "_acme-challenge.nas.example.com", "txt-value"); err != nil {
		t.Fatal(err)
	}
	if len(ex.msgs) != 1 {
		t.Fatalf("updates = %d, want 1", len(ex.msgs))
	}
	if len(ex.msgs[0].Ns) == 0 {
		t.Fatal("expected RFC 2136 insert")
	}
	if err := s.CleanUp(context.Background(), "_acme-challenge.nas.example.com", "txt-value"); err != nil {
		t.Fatal(err)
	}
	if len(ex.msgs) != 2 || len(ex.msgs[1].Ns) == 0 {
		t.Fatal("expected RFC 2136 remove")
	}
}
