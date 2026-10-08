package main

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c *addrConn) RemoteAddr() net.Addr { return c.remote }

type fakeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newFakeListener() *fakeListener {
	return &fakeListener{conns: make(chan net.Conn, 16), closed: make(chan struct{})}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8008}
}

// offer queues a connection from remote and returns the client's end of it.
func (l *fakeListener) offer(remote string) net.Conn {
	server, client := net.Pipe()
	l.conns <- &addrConn{Conn: server, remote: &net.TCPAddr{IP: net.ParseIP(remote), Port: 40000}}
	return client
}

func assertPeerClosed(t *testing.T, peer net.Conn) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read on the peer = %v, want io.EOF (connection closed)", err)
	}
}

func assertPeerOpen(t *testing.T, peer net.Conn) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, err := peer.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read on the peer = %v, want a timeout (connection still open)", err)
	}
}

func closeOnCleanup(t *testing.T, c net.Conn) {
	t.Helper()
	t.Cleanup(func() { _ = c.Close() })
}

func acceptOne(t *testing.T, l *sourceFilteringListener) net.Conn {
	t.Helper()
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestSourceFilteringListener_TurningAllowAllOffClosesNonLANConnections(t *testing.T) {
	inner := newFakeListener()
	l := newSourceFilteringListener(inner, true)

	publicPeer := inner.offer("203.0.113.5")
	lanPeer := inner.offer("192.168.1.10")
	publicConn := acceptOne(t, l)
	lanConn := acceptOne(t, l)
	closeOnCleanup(t, publicConn)
	closeOnCleanup(t, lanConn)
	if got := l.tracked(); got != 2 {
		t.Fatalf("tracked = %d, want 2", got)
	}

	l.SetAllowAll(false)

	assertPeerClosed(t, publicPeer)
	assertPeerOpen(t, lanPeer)
	if got := l.tracked(); got != 1 {
		t.Fatalf("tracked after toggle = %d, want 1 (the LAN connection)", got)
	}
	if _, err := publicConn.Write([]byte("x")); err == nil {
		t.Fatal("the server side of the closed connection still accepts writes")
	}
}

func TestSourceFilteringListener_OtherTransitionsCloseNothing(t *testing.T) {
	inner := newFakeListener()
	l := newSourceFilteringListener(inner, true)
	publicPeer := inner.offer("203.0.113.5")
	lanPeer := inner.offer("10.0.0.7")
	closeOnCleanup(t, acceptOne(t, l))
	closeOnCleanup(t, acceptOne(t, l))

	l.SetAllowAll(true)
	assertPeerOpen(t, publicPeer)
	assertPeerOpen(t, lanPeer)
	if got := l.tracked(); got != 2 {
		t.Fatalf("tracked after on->on = %d, want 2", got)
	}

	closedInner := newFakeListener()
	off := newSourceFilteringListener(closedInner, false)
	offPeer := closedInner.offer("172.16.0.9")
	closeOnCleanup(t, acceptOne(t, off))
	off.SetAllowAll(false)
	assertPeerOpen(t, offPeer)

	off.SetAllowAll(true)
	assertPeerOpen(t, offPeer)
	if got := off.tracked(); got != 1 {
		t.Fatalf("tracked after off->on = %d, want 1", got)
	}
}

func TestSourceFilteringListener_TracksOnlyLiveConnections(t *testing.T) {
	inner := newFakeListener()
	l := newSourceFilteringListener(inner, true)

	for i := 0; i < 20; i++ {
		peer := inner.offer("192.168.1.10")
		conn := acceptOne(t, l)
		if i%2 == 0 {
			_ = peer.Close()
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("server read after client close = %v, want io.EOF", err)
			}
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		_ = peer.Close()
	}
	if got := l.tracked(); got != 0 {
		t.Fatalf("tracked after every connection ended = %d, want 0", got)
	}
}

func TestSourceFilteringListener_RefusedAtAcceptIsClosedAndNotTracked(t *testing.T) {
	inner := newFakeListener()
	l := newSourceFilteringListener(inner, false)

	publicPeer := inner.offer("203.0.113.5")
	lanPeer := inner.offer("192.168.1.10")
	conn := acceptOne(t, l)
	closeOnCleanup(t, conn)

	if got := connIP(conn); !got.Equal(net.ParseIP("192.168.1.10")) {
		t.Fatalf("Accept returned the connection from %v, want the LAN one", got)
	}
	assertPeerClosed(t, publicPeer)
	assertPeerOpen(t, lanPeer)
	if got := l.tracked(); got != 1 {
		t.Fatalf("tracked = %d, want 1", got)
	}
}

func TestSourceFilteringListener_ConcurrentAcceptCloseAndToggle(t *testing.T) {
	inner := newFakeListener()
	l := newSourceFilteringListener(inner, true)

	var closers sync.WaitGroup
	acceptorDone := make(chan struct{})
	go func() {
		defer close(acceptorDone)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			closers.Add(1)
			go func() {
				defer closers.Done()
				_ = conn.Close()
			}()
		}
	}()

	var togglers sync.WaitGroup
	togglers.Add(1)
	go func() {
		defer togglers.Done()
		for i := 0; i < 200; i++ {
			l.SetAllowAll(i%2 == 1)
		}
	}()
	for i := 0; i < 200; i++ {
		remote := "203.0.113.5"
		if i%2 == 0 {
			remote = "192.168.1.10"
		}
		peer := inner.offer(remote)
		go func() { _, _ = io.Copy(io.Discard, peer) }()
	}
	togglers.Wait()
	deadline := time.Now().Add(30 * time.Second)
	for len(inner.conns) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the acceptor did not drain the queued connections")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = inner.Close()
	<-acceptorDone
	closers.Wait()
	if got := l.tracked(); got != 0 {
		t.Fatalf("tracked after every connection was closed = %d, want 0", got)
	}
}

// The toggle reaches the listener through httpsControl, the way
// applyNetworkSettings calls it.
func TestHTTPSControlSetAllowAllSources_ClosesNonLANConnections(t *testing.T) {
	inner := newFakeListener()
	l := newSourceFilteringListener(inner, true)
	h := newHTTPSControl(t.TempDir(), "", "", l, 8008, true, tls.Certificate{})

	publicPeer := inner.offer("198.51.100.20")
	lanPeer := inner.offer("100.64.1.2")
	closeOnCleanup(t, acceptOne(t, l))
	closeOnCleanup(t, acceptOne(t, l))

	if err := h.SetAllowAllSources(false); err != nil {
		t.Fatal(err)
	}
	assertPeerClosed(t, publicPeer)
	assertPeerOpen(t, lanPeer)
}

// buildTCPListener wires the filter under the TLS listener; a connection it
// hands out is tracked by that filter, and the toggle leaves a LAN one alone.
func TestBuildTCPListener_FilterTracksAcceptedConnections(t *testing.T) {
	ln, ctrl, err := buildTCPListener(config{stateDir: t.TempDir(), tcpAddr: "127.0.0.1:0", allowAllSources: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, c)
	deadline := time.Now().Add(5 * time.Second)
	for ctrl.filter.tracked() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("tracked = %d, want 1", ctrl.filter.tracked())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := ctrl.SetAllowAllSources(false); err != nil {
		t.Fatal(err)
	}
	if got := ctrl.filter.tracked(); got != 1 {
		t.Fatalf("tracked after toggle = %d, want the loopback connection kept", got)
	}
}
