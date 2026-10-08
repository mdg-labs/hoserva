package main

import (
	"log"
	"net"
	"sync"
	"sync/atomic"

	"github.com/mdg-labs/hoserva/internal/auth"
)

// sourceFilteringListener wraps a TCP listener and closes, without
// answering, any accepted connection whose source address the LAN-only
// filter (Q10, internal/auth.AllowedSource) doesn't allow — applied here,
// at accept time, on TCP only; the Unix socket is already local (doc 01
// §7). allowAll is the one explicit, warned config toggle Q10 names, and
// can be flipped live from /settings/network. Turning it off also closes
// every connection already handed out whose source the filter now
// refuses, so a peer admitted while the toggle was on does not outlive it.
//
// mu orders Accept's check-and-track against SetAllowAll's sweep, so a
// connection cannot be admitted under the old setting after the sweep has
// already looked for it.
type sourceFilteringListener struct {
	net.Listener
	allowAll atomic.Bool

	mu    sync.Mutex
	conns map[*filteredConn]struct{}
}

func newSourceFilteringListener(inner net.Listener, allowAll bool) *sourceFilteringListener {
	l := &sourceFilteringListener{Listener: inner, conns: map[*filteredConn]struct{}{}}
	l.SetAllowAll(allowAll)
	return l
}

func (l *sourceFilteringListener) SetAllowAll(allowAll bool) {
	if allowAll {
		log.Println("hoservad: WARNING: the LAN-only source filter is disabled — every source address is accepted on the TCP listener (Q10)")
	}
	l.mu.Lock()
	l.allowAll.Store(allowAll)
	var refused []*filteredConn
	if !allowAll {
		for c := range l.conns {
			if !auth.AllowedSource(c.ip, false) {
				refused = append(refused, c)
				delete(l.conns, c)
			}
		}
	}
	l.mu.Unlock()

	for _, c := range refused {
		_ = c.Conn.Close()
	}
	if len(refused) > 0 {
		log.Printf("hoservad: the LAN-only source filter is back on — closed %d open connection(s) from other addresses (Q10)", len(refused))
	}
}

func (l *sourceFilteringListener) AllowAll() bool {
	return l.allowAll.Load()
}

func (l *sourceFilteringListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		host := connIP(conn)
		l.mu.Lock()
		if host == nil || !auth.AllowedSource(host, l.allowAll.Load()) {
			l.mu.Unlock()
			_ = conn.Close()
			continue
		}
		fc := &filteredConn{Conn: conn, ip: host, owner: l}
		l.conns[fc] = struct{}{}
		l.mu.Unlock()
		return fc, nil
	}
}

func (l *sourceFilteringListener) tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

// filteredConn leaves the listener's set when it is closed, by either
// side or by a toggle sweep, so the set holds live connections only.
type filteredConn struct {
	net.Conn
	ip    net.IP
	owner *sourceFilteringListener
}

func (c *filteredConn) Close() error {
	c.owner.mu.Lock()
	delete(c.owner.conns, c)
	c.owner.mu.Unlock()
	return c.Conn.Close()
}

func connIP(conn net.Conn) net.IP {
	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	return addr.IP
}
