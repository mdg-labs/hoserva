package auth

import (
	"context"
	"fmt"
	"os/user"
)

type peerCredentialContextKey struct{}

// WithPeerCredential attaches cred to ctx for downstream handlers (Q78).
func WithPeerCredential(ctx context.Context, cred PeerCredential) context.Context {
	return context.WithValue(ctx, peerCredentialContextKey{}, cred)
}

// PeerCredentialFromContext returns the Unix socket peer credential when
// present — set by the daemon's ConnContext hook (doc 01 §5, Q44).
func PeerCredentialFromContext(ctx context.Context) (PeerCredential, bool) {
	cred, ok := ctx.Value(peerCredentialContextKey{}).(PeerCredential)
	return cred, ok
}

// PeerCredential is a Unix socket connection's kernel-verified identity
// (SO_PEERCRED, doc 01 §5) — the socket carries no bearer or cookie
// credential of its own; the kernel already vouches for the caller.
type PeerCredential struct {
	UID uint32
	GID uint32 // the connecting process's primary group.
}

// GroupLookup resolves whether a peer credential is authorized to use the
// Unix socket: uid 0, or a member — primary or supplementary — of the
// named group (Q44). It sits behind an interface, with FakeGroupLookup
// alongside it, so tests exercise root, group member and stranger without
// real OS groups (CLAUDE.md).
type GroupLookup interface {
	IsMember(cred PeerCredential, group string) (bool, error)
}

// OSGroupLookup checks the real system's groups (NSS/`/etc/group` via Go's
// os/user). A group that doesn't exist is not an error — dev runs never
// create the `hoserva` group, and that must degrade to "root only",
// logged once by the caller, not fail the whole daemon.
type OSGroupLookup struct{}

var _ GroupLookup = OSGroupLookup{}

// ErrGroupNotFound is returned when group does not exist on this system —
// distinguishable from "found but not a member" so a caller can log the
// degraded-to-root-only case once, instead of on every request.
var ErrGroupNotFound = fmt.Errorf("group not found")

func (OSGroupLookup) IsMember(cred PeerCredential, group string) (bool, error) {
	if cred.UID == 0 {
		return true, nil
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		if _, ok := err.(user.UnknownGroupError); ok {
			return false, ErrGroupNotFound
		}
		return false, fmt.Errorf("looking up group %q: %w", group, err)
	}
	if fmt.Sprint(cred.GID) == g.Gid {
		return true, nil
	}
	u, err := user.LookupId(fmt.Sprint(cred.UID))
	if err != nil {
		// A uid with no passwd entry can't be a supplementary member of
		// anything either — not an error, just not a member.
		return false, nil
	}
	gids, err := u.GroupIds()
	if err != nil {
		return false, nil
	}
	for _, gid := range gids {
		if gid == g.Gid {
			return true, nil
		}
	}
	return false, nil
}

// FakeGroupLookup is a scriptable GroupLookup for tests: it treats gids
// listed in Members as belonging to Group, and never touches the real
// system (CLAUDE.md's "scriptable fake" for every system-touching
// interface).
type FakeGroupLookup struct {
	Group   string
	Members map[uint32]bool
	Exists  bool
}

var _ GroupLookup = &FakeGroupLookup{}

func (f *FakeGroupLookup) IsMember(cred PeerCredential, group string) (bool, error) {
	if cred.UID == 0 {
		return true, nil
	}
	if group != f.Group {
		return false, ErrGroupNotFound
	}
	if !f.Exists {
		return false, ErrGroupNotFound
	}
	return f.Members[cred.GID], nil
}
