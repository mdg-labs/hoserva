package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// plainHTTPPointer is the short response a plain-HTTP request on the
// TLS-only port gets (Q9) — a pointer to https://, not a redirect loop
// (nothing here can serve a Location header the client would need HTTPS
// already trusted to follow) and not a raw TLS handshake error.
const plainHTTPPointer = "This server only accepts HTTPS. Use https://%s/\n"

// atomicDuration is a time.Duration a test can shorten from a different
// goroutine than the one reading it, without racing: peekDeadline and
// drainDeadline below are read from handleConn's own per-connection
// goroutine (deliberately not the one calling Accept — see this file's
// own doc comment on tlsSniffingListener), so a plain package var a test
// mutates directly, the way it could when this sniffing ran synchronously
// inside Accept, is a genuine data race now that it doesn't.
type atomicDuration struct{ ns atomic.Int64 }

func newAtomicDuration(d time.Duration) *atomicDuration {
	ad := &atomicDuration{}
	ad.ns.Store(int64(d))
	return ad
}

func (a *atomicDuration) Load() time.Duration   { return time.Duration(a.ns.Load()) }
func (a *atomicDuration) Store(d time.Duration) { a.ns.Store(int64(d)) }

// peekDeadline bounds how long tlsSniffingListener waits for a new
// connection's first byte before giving up on it — otherwise a client
// that connects and never sends anything (deliberately or not) would tie
// up handleConn's own goroutine forever. Shortened by a test rather than
// actually waiting out the production value.
var peekDeadline = newAtomicDuration(10 * time.Second)

// tlsSniffingListener wraps a plain TCP listener and, for each accepted
// connection, looks at its first byte before deciding what to do with it:
// 0x16 is a TLS ClientHello's record type, so that connection is wrapped
// in tls.Server and returned to the caller (net/http) as normal; anything
// else is a plain-HTTP request, which gets plainHTTPPointer written
// directly and the connection closed — never reaching net/http's own
// request parsing, and never a TLS handshake that would otherwise just
// fail with an opaque protocol error (Q9).
//
// The sniff itself — and the plain-HTTP reply, which can wait up to
// peekDeadline+drainDeadline before the connection closes — never runs
// inside Accept: acceptLoop's own call to the inner listener's Accept
// never blocks on any one connection's behaviour, only on a new one
// arriving, and every accepted connection gets its own goroutine
// (handleConn) to sniff and answer in. Before this, both ran
// synchronously inside Accept itself, so one silent connection delayed
// every other client queued behind it in net/http's own Serve loop — up
// to peekDeadline's own 10 seconds, confirmed live at 9.70s.
type tlsSniffingListener struct {
	net.Listener
	tlsConfig *tls.Config

	connCh    chan net.Conn
	errCh     chan error
	done      chan struct{}
	closeOnce sync.Once
}

// newTLSSniffingListener wraps inner. tlsConfig.MinVersion is forced to
// TLS 1.2 regardless of what the caller set, matching this issue's own
// security-review requirement ("TLS 1.2 minimum").
func newTLSSniffingListener(inner net.Listener, tlsConfig *tls.Config) *tlsSniffingListener {
	cfg := tlsConfig.Clone()
	if cfg.MinVersion < tls.VersionTLS12 {
		cfg.MinVersion = tls.VersionTLS12
	}
	l := &tlsSniffingListener{
		Listener:  inner,
		tlsConfig: cfg,
		connCh:    make(chan net.Conn),
		errCh:     make(chan error, 1),
		done:      make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

// acceptTempDelayMax caps the backoff acceptLoop applies after a
// temporary Accept error (EMFILE, ENFILE, ECONNABORTED, ...), mirroring
// net/http.Server.Serve's own tempDelay loop (net/http/server.go) so both
// sides agree on what "temporary" means and on how long to back off: this
// loop retrying internally means net/http's own outer retry — the
// "retrying in 5ms" log line — never actually has to run, since Accept
// below only returns once there is a real connection or a permanent
// error.
const acceptTempDelayMax = time.Second

// acceptLoop calls the inner listener's own Accept in a tight loop — that
// call never blocks on any one connection's own behaviour — and hands
// each accepted connection to its own goroutine.
//
// A temporary error (fd exhaustion, an aborted connection, ...) is
// retried with the same exponential backoff net/http.Server.Serve uses,
// never ending the loop: ending it here used to be fatal in a way
// net/http itself could not recover from — net/http, seeing the same
// temporary error, retries by calling this listener's own Accept again,
// but with acceptLoop already gone, nothing was left to ever produce
// another value on connCh or errCh, so Accept (below) blocked forever.
// Reproduced live: with the daemon's RLIMIT_NOFILE cut to 48, 60 silent
// connections drove Accept to "too many open files"; once those
// connections closed and fds were free again, every further HTTPS request
// timed out — including 15s later — while the process stayed alive, and
// SIGTERM's Shutdown never returned since Serve was still stuck in
// Accept, needing a SIGKILL. A genuinely permanent error (the listener
// closed, in particular) still ends the loop and reports itself, exactly
// as before.
func (l *tlsSniffingListener) acceptLoop() {
	var tempDelay time.Duration
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			//nolint:staticcheck // net.Error.Temporary is deprecated upstream with no replacement for this; net/http.Server.Serve itself still calls it for the exact same reason (see acceptTempDelayMax's doc comment), and this loop needs to agree with it on what counts as retryable.
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					// Logged once per backoff burst, not once per retry
					// within it: this loop can retry every 5ms while a
					// burst lasts, and logging on every one of those
					// would itself be the kind of unbounded-rate work
					// this whole retry loop exists to survive. One line
					// per burst still makes fd exhaustion visible to the
					// operator, which it wasn't at all before this.
					log.Printf("hoservad: temporary accept error, retrying: %v", err)
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if tempDelay > acceptTempDelayMax {
					tempDelay = acceptTempDelayMax
				}
				select {
				case <-time.After(tempDelay):
				case <-l.done:
					return
				}
				continue
			}
			l.errCh <- err
			l.closeOnce.Do(func() { close(l.done) })
			return
		}
		tempDelay = 0
		go l.handleConn(conn)
	}
}

// Close stops acceptLoop and closes the inner listener — idempotent (a
// double Close, or a Close racing acceptLoop's own error path, never
// double-closes l.done) — and unblocks any Accept call already waiting,
// via l.done, immediately, rather than only once acceptLoop notices the
// inner Accept itself has failed.
func (l *tlsSniffingListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// handleConn does the actual sniffing (and, for a plain-HTTP request, the
// whole reply) off the accept path, in its own goroutine, so it can never
// delay any other connection. A successfully sniffed TLS connection is
// handed to Accept via connCh; if the listener has already stopped
// accepting (l.done closed) by the time this goroutine finishes, the
// connection is closed here instead of leaking a goroutine blocked
// forever on a send nobody will ever receive.
func (l *tlsSniffingListener) handleConn(conn net.Conn) {
	wrapped, first, ok := peekFirstByte(conn)
	if !ok {
		_ = conn.Close()
		return
	}
	if first != 0x16 {
		writePlainHTTPPointerAndClose(wrapped)
		return
	}
	tlsConn := tls.Server(wrapped, l.tlsConfig)
	select {
	case l.connCh <- tlsConn:
	case <-l.done:
		_ = tlsConn.Close()
	}
}

// Accept also watches l.done directly, not only l.errCh: once the
// listener has been closed (whether by Close or by acceptLoop's own
// permanent-error path), any further call — including one net/http makes
// after an error it treated as temporary — returns net.ErrClosed
// immediately instead of blocking forever waiting for a value neither
// channel will ever receive again.
func (l *tlsSniffingListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connCh:
		return conn, nil
	case err := <-l.errCh:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// prefixConn re-delivers a byte already Read from conn before conn's own
// unread bytes, so peeking at the connection's first byte doesn't consume
// it out from under tls.Server, which needs to see the ClientHello from
// its own first byte onward.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// peekFirstByte reads exactly one byte from conn, under peekDeadline, and
// wraps conn so that byte is still the first one any later Read sees.
func peekFirstByte(conn net.Conn) (net.Conn, byte, bool) {
	if err := conn.SetReadDeadline(time.Now().Add(peekDeadline.Load())); err != nil {
		return conn, 0, false
	}
	var buf [1]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil {
		return conn, 0, false
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return conn, 0, false
	}
	return &prefixConn{Conn: conn, prefix: buf[:]}, buf[0], true
}

// drainDeadline bounds how long writePlainHTTPPointerAndClose waits for
// the rest of the client's (never parsed) request to arrive before
// closing the connection — closing a TCP connection while its receive
// buffer still holds unread bytes gets a RST instead of a clean FIN on
// most stacks (confirmed on this one while writing this test), which a
// client sees as "connection reset", not the plain-text response this
// function just wrote.
var drainDeadline = newAtomicDuration(time.Second)

// writePlainHTTPPointerAndClose sends plainHTTPPointer without ever
// invoking net/http's own request parsing — deliberately not parsing the
// client's request line at all, since the whole point is to work even
// when what arrived isn't valid HTTP.
func writePlainHTTPPointerAndClose(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	host := "localhost:8008"
	if addr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		host = plainHTTPPointerHost(addr)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(peekDeadline.Load()))
	body := fmt.Sprintf(plainHTTPPointer, host)
	response := fmt.Sprintf(
		"HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(body), body,
	)
	w := bufio.NewWriter(conn)
	_, _ = w.WriteString(response)
	_ = w.Flush()

	_ = conn.SetReadDeadline(time.Now().Add(drainDeadline.Load()))
	_, _ = io.Copy(io.Discard, conn)
}

// plainHTTPPointerHost builds the "host:port" writePlainHTTPPointerAndClose
// points the client at from addr's own IP and port — net.JoinHostPort,
// not a bare Sprintf, since an IPv6 address needs brackets
// (`[::1]:8008`) to parse back as one "host:port" pair rather than three
// colon-separated fields.
func plainHTTPPointerHost(addr *net.TCPAddr) string {
	return net.JoinHostPort(hostForPointer(addr.IP), strconv.Itoa(addr.Port))
}

func hostForPointer(ip net.IP) string {
	if ip.IsUnspecified() || ip == nil {
		return "localhost"
	}
	return ip.String()
}
