//go:build linux

package auth

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerCredentialOf reads conn's SO_PEERCRED (Linux-only — Debian is
// Hoserva's only supported base OS, doc 01 §1). It never sends or reads
// application data on conn; it inspects the already-accepted connection's
// own socket option.
func PeerCredentialOf(conn *net.UnixConn) (PeerCredential, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return PeerCredential{}, fmt.Errorf("getting raw connection: %w", err)
	}

	var cred PeerCredential
	var sysErr error
	if err := raw.Control(func(fd uintptr) {
		ucred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			sysErr = err
			return
		}
		cred = PeerCredential{UID: ucred.Uid, GID: ucred.Gid}
	}); err != nil {
		return PeerCredential{}, fmt.Errorf("reading SO_PEERCRED: %w", err)
	}
	if sysErr != nil {
		return PeerCredential{}, fmt.Errorf("SO_PEERCRED: %w", sysErr)
	}
	return cred, nil
}
