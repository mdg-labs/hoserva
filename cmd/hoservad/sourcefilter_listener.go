package main

import (
	"log"
	"net"

	"github.com/mdg-labs/hoserva/internal/auth"
)

// sourceFilteringListener wraps a TCP listener and closes, without
// answering, any accepted connection whose source address the LAN-only
// filter (Q10, internal/auth.AllowedSource) doesn't allow — applied here,
// at accept time, on TCP only; the Unix socket is already local (doc 01
// §7). allowAll is the one explicit, warned config toggle Q10 names.
type sourceFilteringListener struct {
	net.Listener
	allowAll bool
}

func newSourceFilteringListener(inner net.Listener, allowAll bool) *sourceFilteringListener {
	if allowAll {
		log.Println("hoservad: WARNING: the LAN-only source filter is disabled — every source address is accepted on the TCP listener (Q10)")
	}
	return &sourceFilteringListener{Listener: inner, allowAll: allowAll}
}

func (l *sourceFilteringListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		host := connIP(conn)
		if host == nil || !auth.AllowedSource(host, l.allowAll) {
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
}

func connIP(conn net.Conn) net.IP {
	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	return addr.IP
}
