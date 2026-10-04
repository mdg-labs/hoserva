package share

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// seedUndoTimeout bounds the undo of a failed seed, which runs without the
// request's or the job's own cancellation.
const seedUndoTimeout = 2 * time.Minute

var minFreeSpacePattern = regexp.MustCompile(`^[1-9][0-9]*[KMGT]?$`)

// SeedAccess is one imported account's access to a seeded share.
type SeedAccess struct {
	Username string
	Access   string
}

// SeedShare is a share the Unraid import creates. It is always stored
// array-only, because no cache exists before the point of no return;
// TargetCacheMode is the mode the import mapped, for the step that applies it.
type SeedShare struct {
	Name            string
	CreatePolicy    pool.CreatePolicy
	TargetCacheMode pool.CacheMode
	SMB             SMB
	MinFreeSpace    string
	Notes           []string
	Access          []SeedAccess
}

// SeedUser is an account the import creates. PasswordHash is a hash of a random
// value nobody knows: the account has no working credential until a password is
// set.
type SeedUser struct {
	Username     string
	PasswordHash string
}

// SeedInput is what SeedMigration creates.
type SeedInput struct {
	Shares []SeedShare
	Users  []SeedUser
}

// SeedResult names what SeedMigration created and what already existed and was
// left as it was.
type SeedResult struct {
	Shares         []string
	ExistingShares []string
	Users          []string
	ExistingUsers  []string
}

// SeedMigration creates the shares and share-only accounts of an Unraid import
// while the migration is pending, and writes nothing to an adopted disk: no
// directory is created, no mode or owner changed, whatever the share's cache
// mode. Branch directories already exist on the disks that hold the share, and
// their top-level treatment is a later step's. No mount is made for a share:
// /mnt/user/<share> is a directory of the read-only catch-all, and a share's
// directory may exist on no adopted disk (one that lived only on the cache), where
// a mount would have no branch. smb.conf exports every share read-only.
//
// Everything is validated before the first write. The rows are written in one
// transaction; then the pool's units, smb.conf and exports are generated from
// them. A failure after the rows were written removes them again and
// regenerates the files from what is left, and returns the error. A share or
// user that already exists is left as it is.
func (s *Service) SeedMigration(ctx context.Context, in SeedInput) (SeedResult, error) {
	defer s.postCommit(ctx)
	if s.Shares == nil {
		return SeedResult{}, fmt.Errorf("share: SeedMigration requires a ShareStore")
	}
	settings, _, err := s.array(ctx)
	if err != nil {
		return SeedResult{}, err
	}
	if !settings.MigrationPending {
		return SeedResult{}, fmt.Errorf("%w: seeding is only for the array of a pending Unraid migration", ErrInvalidInput)
	}
	users, shares, err := s.seedRows(in)
	if err != nil {
		return SeedResult{}, err
	}

	seeded, err := s.Shares.SeedMigration(ctx, users, shares)
	if err != nil {
		return SeedResult{}, err
	}
	res := SeedResult{Shares: seeded.Shares, ExistingShares: seeded.ExistingShares, ExistingUsers: seeded.ExistingUsers}
	for _, u := range seeded.Users {
		res.Users = append(res.Users, u.Username)
	}
	if len(seeded.Shares) == 0 {
		return res, nil
	}
	if written, err := s.applySeeded(ctx); err != nil {
		return SeedResult{}, s.undoSeed(ctx, seeded, written, err)
	}
	return res, nil
}

func (s *Service) seedRows(in SeedInput) ([]store.SeedUser, []store.SeedShare, error) {
	now := s.now().UTC()
	known := map[string]bool{}
	users := make([]store.SeedUser, 0, len(in.Users))
	for _, u := range in.Users {
		if err := ValidateAccountName(u.Username); err != nil {
			return nil, nil, fmt.Errorf("%w: seed user: %w", ErrInvalidInput, err)
		}
		if u.PasswordHash == "" || known[u.Username] {
			return nil, nil, fmt.Errorf("%w: seed user %q has no placeholder hash or is listed twice", ErrInvalidInput, u.Username)
		}
		known[u.Username] = true
		users = append(users, store.SeedUser{ID: uuid.NewString(), Username: u.Username, PasswordHash: u.PasswordHash, CreatedAt: now})
	}
	names := map[string]bool{}
	shares := make([]store.SeedShare, 0, len(in.Shares))
	for _, sh := range in.Shares {
		if err := validateName(sh.Name); err != nil {
			return nil, nil, err
		}
		if names[sh.Name] {
			return nil, nil, fmt.Errorf("%w: seed share %q is listed twice", ErrInvalidInput, sh.Name)
		}
		names[sh.Name] = true
		if err := validateCreatePolicy(sh.CreatePolicy); err != nil {
			return nil, nil, err
		}
		if sh.TargetCacheMode != "" {
			if err := validateCacheMode(sh.TargetCacheMode); err != nil {
				return nil, nil, err
			}
		}
		if sh.MinFreeSpace != "" && !minFreeSpacePattern.MatchString(sh.MinFreeSpace) {
			return nil, nil, fmt.Errorf("%w: minimum free space %q of share %s is not a mergerfs size (e.g. 1000K)", ErrInvalidInput, sh.MinFreeSpace, sh.Name)
		}
		smb := sh.SMB
		if !smb.TimeMachine {
			smb.TimeMachineMaxSize = ""
		}
		if err := validateSMB(smb); err != nil {
			return nil, nil, err
		}
		grants := make([]store.SeedGrant, 0, len(sh.Access))
		granted := map[string]bool{}
		for _, a := range sh.Access {
			if !known[a.Username] || granted[a.Username] {
				return nil, nil, fmt.Errorf("%w: share %s grants access to %q, who is not an account of this seed or is listed twice", ErrInvalidInput, sh.Name, a.Username)
			}
			if a.Access != "none" && a.Access != "read-only" && a.Access != "read-write" {
				return nil, nil, fmt.Errorf("%w: share %s grants %q the access %q", ErrInvalidInput, sh.Name, a.Username, a.Access)
			}
			granted[a.Username] = true
			grants = append(grants, store.SeedGrant{Username: a.Username, Access: a.Access})
		}
		nfs := defaultNFS()
		shares = append(shares, store.SeedShare{
			Share: toStore(Share{
				Name:            sh.Name,
				CacheMode:       pool.ArrayOnly,
				CreatePolicy:    sh.CreatePolicy,
				SMB:             smb,
				NFS:             nfs,
				CreatedAt:       now,
				UpdatedAt:       now,
				MinFreeSpace:    sh.MinFreeSpace,
				TargetCacheMode: sh.TargetCacheMode,
				MigrationNotes:  sh.Notes,
			}),
			Grants: grants,
		})
	}
	return users, shares, nil
}

// applySeeded generates the pool's units, smb.conf and exports from the stored
// rows. Like a share create, it refuses before writing anything when one of
// them is unmanaged or a host file Hoserva has not taken over (Q76); written
// says whether it got as far as writing the first file.
func (s *Service) applySeeded(ctx context.Context) (written bool, err error) {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	state, smb, nfs, err := s.shareFiles(ctx)
	if err != nil {
		return false, err
	}
	if err := s.Gen.CanWriteShareFiles(ctx, state); err != nil {
		return false, err
	}
	now := s.now()
	if err := s.Gen.WritePoolMounts(ctx, state, applyCommand, 1, now); err != nil {
		return true, err
	}
	if err := s.Gen.WriteSamba(ctx, smb, applyCommand, 1, now); err != nil {
		return true, err
	}
	if err := s.Gen.WriteNFS(ctx, nfs, applyCommand, 1, now); err != nil {
		return true, err
	}
	return true, nil
}

// undoSeed removes the rows a seed created and, when files were written from
// them, regenerates the files from what is left. It returns cause with what the
// undo did.
func (s *Service) undoSeed(ctx context.Context, seeded store.Seeded, written bool, cause error) error {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), seedUndoTimeout)
	defer cancel()
	if err := s.Shares.UnseedMigration(uctx, seeded); err != nil {
		return errors.Join(cause, fmt.Errorf("removing the seeded shares: %w", err))
	}
	if !written {
		return cause
	}
	if err := s.restoreGenerated(uctx); err != nil {
		return errors.Join(cause, fmt.Errorf("the seeded shares were removed but the generated files could not be restored: %w", err))
	}
	return cause
}
