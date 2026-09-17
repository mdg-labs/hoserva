package auth

import (
	"net"
	"testing"
)

// One allowed and one just-outside address per range, plus IPv4-mapped
// IPv6 forms — as CLAUDE.md's dispatch note requires.
func TestAllowedSource(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"loopback v4-mapped v6", "::ffff:127.0.0.1", true},

		{"rfc1918 10/8 in", "10.1.2.3", true},
		{"rfc1918 10/8 out", "9.255.255.255", false},
		{"rfc1918 172.16/12 in", "172.16.0.1", true},
		{"rfc1918 172.16/12 out", "172.32.0.1", false},
		{"rfc1918 192.168/16 in", "192.168.1.1", true},
		{"rfc1918 192.168/16 out", "192.169.0.1", false},

		{"link-local v4 in", "169.254.1.1", true},
		{"link-local v4 out", "169.253.255.255", false},
		{"link-local v6 in", "fe80::1", true},
		{"link-local v6 out", "fec0::1", false},

		{"ula in", "fc00::1", true},
		{"ula in high", "fdff::1", true},
		{"ula out", "fe00::1", false},

		{"cgnat in", "100.64.0.1", true},
		{"cgnat out low", "100.63.255.255", false},
		{"cgnat out high", "100.128.0.1", false},

		{"public v4", "8.8.8.8", false},
		{"public v6", "2001:4860:4860::8888", false},

		{"rfc1918 v4-mapped v6 in", "::ffff:10.1.2.3", true},
		{"public v4-mapped v6 out", "::ffff:8.8.8.8", false},

		// NAT64 (RFC 6052's well-known prefix 64:ff9b::/96) and 6to4
		// (2002::/16) both embed an IPv4 address inside an IPv6 one, but
		// — unlike the IPv4-mapped ::ffff:0:0/96 cases above, which
		// AllowedSource unwraps via ip.To4() — neither is ever unwrapped
		// here: AllowedSource treats them as ordinary IPv6 addresses, so
		// even a NAT64/6to4 address that embeds a private IPv4 one is
		// still judged solely by whether the IPv6 address itself falls in
		// sourceRanges (it doesn't, for either prefix), never by peeking
		// inside it. That is the correct, conservative behaviour — an
		// embedded private address is not a claim this filter can trust
		// without also trusting the NAT64/6to4 gateway that produced it —
		// and these cases pin it against a future change accidentally
		// starting to unwrap them.
		{"nat64, embeds a private v4 address", "64:ff9b::0a01:0203", false},
		{"nat64, embeds a public v4 address", "64:ff9b::0808:0808", false},
		{"6to4, embeds a private v4 address", "2002:0a01:0203::1", false},
		{"6to4, embeds a public v4 address", "2002:0808:0808::1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip := net.ParseIP(c.ip)
			if ip == nil {
				t.Fatalf("invalid test IP %q", c.ip)
			}
			if got := AllowedSource(ip, false); got != c.want {
				t.Errorf("AllowedSource(%s, false) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
}

func TestAllowedSourceAllowAll(t *testing.T) {
	if !AllowedSource(net.ParseIP("8.8.8.8"), true) {
		t.Error("allowAll=true must accept every source")
	}
}

func TestAllowedSourceNilIP(t *testing.T) {
	if AllowedSource(nil, false) {
		t.Error("a nil IP must never be allowed")
	}
}
