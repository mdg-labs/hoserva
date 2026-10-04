package share

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"

	"github.com/mdg-labs/hoserva/internal/config"
)

// ShareGrants is what the permission tables hold for one share, with
// group members already resolved to usernames (Hoserva groups are
// database-only and have no Unix group, Q27).
type ShareGrants struct {
	// Users maps a username to its own grant: none, read-only or
	// read-write.
	Users map[string]string
	// Groups is every group grant of the share.
	Groups []GroupGrant
}

// GroupGrant is one group's grant and the usernames that belong to it.
type GroupGrant struct {
	Access  string
	Members []string
}

// AccessReader reads every share's grants, keyed by share name. The
// permission tables belong to the auth store, which satisfies it.
type AccessReader interface {
	ShareGrants(ctx context.Context) (map[string]ShareGrants, error)
}

const (
	accessReadOnly  = "read-only"
	accessReadWrite = "read-write"
)

func accessRank(access string) int {
	switch access {
	case accessReadOnly:
		return 1
	case accessReadWrite:
		return 2
	default:
		return 0
	}
}

// effectiveAccess resolves grants to the accounts that may read a share
// and the ones that may write it. A user's own grant wins over any group
// grant; between groups the widest grant wins; a user with no grant at all
// has no access.
func effectiveAccess(g ShareGrants) (valid, write []string) {
	level := map[string]int{}
	for _, grp := range g.Groups {
		for _, member := range grp.Members {
			if r := accessRank(grp.Access); r > level[member] {
				level[member] = r
			} else if _, ok := level[member]; !ok {
				level[member] = r
			}
		}
	}
	for user, access := range g.Users {
		level[user] = accessRank(access)
	}
	for user, r := range level {
		if r == 0 {
			continue
		}
		if !config.SambaUserListable(user) {
			log.Printf("share: %q cannot be written into smb.conf, so it has no SMB access", user)
			continue
		}
		valid = append(valid, user)
		if r == 2 {
			write = append(write, user)
		}
	}
	sort.Strings(valid)
	sort.Strings(write)
	return valid, write
}

// RefreshAccess regenerates smb.conf from the stored grants, after a
// change to who may reach which share. A smb.conf the user keeps unmanaged
// or has not imported is left as it is (Q76), as ApplyTopology leaves it.
// The grants are already stored when this runs, so a failure here leaves
// the previous file in place and the next regeneration applies them.
func (s *Service) RefreshAccess(ctx context.Context) error {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	_, smb, _, err := s.shareFiles(ctx)
	if errors.Is(err, ErrNoArray) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.Gen.WriteSamba(ctx, smb, applyCommand, 1, s.now()); err != nil && !shareFileWriteSkippable(err) {
		return fmt.Errorf("share: regenerating smb.conf: %w", err)
	}
	return nil
}
