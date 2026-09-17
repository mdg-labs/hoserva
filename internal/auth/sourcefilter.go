package auth

import "net"

// sourceRanges are Q10's default-allowed source networks: loopback,
// RFC 1918, IPv4 link-local, IPv6 link-local, IPv6 ULA and CGNAT
// 100.64.0.0/10 (Tailscale). Loopback itself is checked with net.IP's own
// IsLoopback rather than listed here, since it covers both ::1 and the
// whole 127.0.0.0/8 range without a CIDR literal for each.
var sourceRanges = mustParseCIDRs(
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
	"169.254.0.0/16",
	"fe80::/10",
	"fc00::/7",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("auth: invalid CIDR literal " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// AllowedSource reports whether ip is permitted by the default LAN-only
// source filter (Q10, doc 01 §7): loopback, RFC 1918, link-local, IPv6
// ULA and CGNAT 100.64.0.0/10, in either their IPv4 or IPv4-mapped-IPv6
// form. allowAll bypasses the filter entirely — the one explicit, warned
// config toggle Q10 names; callers log a warning when it is set, not this
// function.
//
// There is no support for trusting a reverse proxy's X-Forwarded-For here
// (or anywhere in this issue): this filter runs at TCP accept time,
// against the real peer address, before any HTTP request — let alone a
// header — has been read.
func AllowedSource(ip net.IP, allowAll bool) bool {
	if allowAll {
		return true
	}
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range sourceRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
