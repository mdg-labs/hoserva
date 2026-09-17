package auth

import "net"

// AddressSubject returns Login's SubjectAddress rate-limiter key for a
// source address (doc 01 §7's "per source address" budget). addr is a
// bare IP with no port (main.go's withSourceAddrMiddleware already
// strips it).
//
// Every loopback address — the whole 127.0.0.0/8 range, ::1, and an
// IPv4-mapped ::ffff:127.x.x.x — shares one fixed subject, never its own:
// a review finding's reproduction filled the address table (and locked
// most of it) from 127.0.0.0/8 alone, using tens of thousands of distinct
// loopback addresses no LAN client could ever present, which then refused
// a fresh, real address once the table had no unlocked entry left to
// evict. A single local caller gets one budget, exactly like a single
// remote one does.
//
// An IPv4 address — including an IPv4-mapped IPv6 one, ::ffff:a.b.c.d,
// which is the same address as a.b.c.d — otherwise keys on itself. An
// IPv6 address keys on its /64: the smallest block a single LAN, VPN or
// ISP link typically hands out to one host, so a caller that rotates
// through addresses of its own /64 (trivial over IPv6, and free — every
// one of them still reaches this box under Q10's source filter) can't
// spin up a fresh, unlocked address subject on every single request.
// This is part of the fix for the review finding that an address-rotating
// flood could otherwise fill and evict its way through the limiter's
// address table at no per-request cost.
//
// addr failing to parse is not expected from withSourceAddrMiddleware's
// own output, but is handled rather than assumed: it still gets *a*
// bucket (its own literal string), rather than silently bypassing the
// limiter for that request.
func AddressSubject(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil {
		return "addr:raw:" + addr
	}
	if ip.IsLoopback() {
		return "addr:loopback"
	}
	if v4 := ip.To4(); v4 != nil {
		return "addr:v4:" + v4.String()
	}
	prefix := ip.Mask(net.CIDRMask(64, 128))
	return "addr:v6:" + prefix.String()
}
