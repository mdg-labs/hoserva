package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
)

// hoservaGroup is Q44's fixed group name — not configurable: doc 01 §7
// documents it plainly as root-equivalent, exactly like the docker group,
// and letting it be renamed would make that documentation wrong for
// whoever renamed it.
const hoservaGroup = "hoserva"

// setupUnixListener creates the socket at path, removing a stale file
// left behind by an unclean shutdown first — this path is always inside
// this daemon's own state directory (production /run/hoserva/hoserva.sock,
// dev a workspace path), never a system path shared with anything else,
// so unlinking a genuinely stale one before binding is safe. The socket's
// own mode is set explicitly to 0600 (owner-only) the moment it's
// created, rather than left at whatever the process umask happens to
// produce (observed 0755 in practice — every account on the host could
// open it, though every request over it would still be refused
// downstream by unixSocketAuthMiddleware's own SO_PEERCRED check):
// applySocketGroupPermissions widens it to 0660 afterward only if the
// hoserva group actually exists.
func setupUnixListener(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating socket directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("setting Unix socket permissions at %s: %w", path, err)
	}
	return ln, nil
}

// unixSocketDialTimeout bounds how long removeStaleSocket waits to find
// out whether an existing socket file is actually answering.
const unixSocketDialTimeout = time.Second

// removeStaleSocket removes an existing socket file at path only after
// confirming nothing is actually listening on it: it dials path first — a
// second hoservad instance racing this one at first start (or any other
// process that happens to bind this exact path) must never have its live
// socket silently unlinked and replaced out from under it, the way an
// unconditional os.Remove used to. A successful dial means something is
// answering, so this refuses outright; any dial failure (connection
// refused, no such file, ...) means the file, if any, is a stale leftover
// from an unclean shutdown (SIGKILL, a crash) that never got to Close()
// its own listener, which unlinks the socket itself on a clean exit — the
// original case this always handled, now checked rather than assumed.
func removeStaleSocket(path string) error {
	conn, err := net.DialTimeout("unix", path, unixSocketDialTimeout)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("a Unix socket at %s is already accepting connections — refusing to take it over; stop the other process first", path)
	}
	if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
		return fmt.Errorf("removing stale socket at %s: %w", path, rmErr)
	}
	return nil
}

// applySocketGroupPermissions chowns the socket to hoservaGroup and chmods
// it 0660 (Q44) — production-only in practice, since it needs the group
// to exist, and degrades cleanly (one log line, no error) when it
// doesn't, which is always true in dev (CLAUDE.md: never create a system
// group from here) — setupUnixListener has already left the socket at
// 0600 in that case, so the log line below is accurate regardless of
// whether this daemon runs as root or as a non-root dev user.
func applySocketGroupPermissions(path string) {
	g, err := user.LookupGroup(hoservaGroup)
	if err != nil {
		log.Printf("hoservad: group %q does not exist — leaving the Unix socket owner-only (0600); root and this daemon's own user can still use it (Q44)", hoservaGroup)
		return
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		log.Printf("hoservad: could not parse gid for group %q: %v", hoservaGroup, err)
		return
	}
	if err := os.Chown(path, -1, gid); err != nil {
		log.Printf("hoservad: could not chown %s to group %q: %v", path, hoservaGroup, err)
		return
	}
	if err := os.Chmod(path, 0o660); err != nil {
		log.Printf("hoservad: could not chmod %s: %v", path, err)
	}
}

// unixConnContext is http.Server.ConnContext for the Unix listener: it
// reads SO_PEERCRED once per connection (not per request — the kernel's
// answer cannot change mid-connection) and attaches it to every request
// context on that connection.
func unixConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	cred, err := auth.PeerCredentialOf(uc)
	if err != nil {
		log.Printf("hoservad: reading the Unix socket peer's credentials: %v", err)
		return ctx
	}
	return auth.WithPeerCredential(ctx, cred)
}

// unixSocketAuthMiddleware refuses any request whose connection's peer
// credential is not uid 0, this daemon's own uid, or a member of
// hoservaGroup (Q44). The daemon's own uid is authorized in addition to
// what Q44 literally names so that a non-root dev daemon — the only kind
// CLAUDE.md permits outside packaging — remains usable by the account
// that started it: nothing here can create the hoserva group (that would
// touch the system), so without this, a dev daemon's own Unix socket
// would be unusable by anyone at all except a coincidental uid-0 shell.
//
// An authorized request is marked with a synthetic Authorization header
// (api.UnixSocketCredentialHeader) purely so ogen's generated per-scheme
// presence check calls api.TrustedSecurityHandler at all — see that
// header's own doc comment for why nothing about its value carries any
// meaning on its own.
func unixSocketAuthMiddleware(next http.Handler, lookup auth.GroupLookup, daemonUID uint32) http.Handler {
	var warnMissingGroupOnce sync.Once

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := auth.PeerCredentialFromContext(r.Context())
		if !ok {
			http.Error(w, "could not determine the caller's identity", http.StatusForbidden)
			return
		}

		authorized := cred.UID == 0 || cred.UID == daemonUID
		if !authorized {
			member, err := lookup.IsMember(cred, hoservaGroup)
			switch {
			case err == nil:
				authorized = member
			case errors.Is(err, auth.ErrGroupNotFound):
				warnMissingGroupOnce.Do(func() {
					log.Printf("hoservad: group %q does not exist — the Unix socket accepts only root and this daemon's own user until it is created (Q44)", hoservaGroup)
				})
			default:
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if !authorized {
			http.Error(w, "forbidden: connect as root, this daemon's own user, or a member of the hoserva group", http.StatusForbidden)
			return
		}

		r.Header.Set(api.UnixSocketCredentialHeader, api.UnixSocketCredentialValue)
		next.ServeHTTP(w, r)
	})
}
