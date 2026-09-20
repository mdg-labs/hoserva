package share

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/mdg-labs/hoserva/internal/pool"
)

var (
	// ErrInvalidInput is a create/update that names a bad cache mode,
	// create policy, or Time Machine size.
	ErrInvalidInput = errors.New("share: invalid input")
	// ErrConfirmation is delete-data whose confirmation is not the
	// share name, or delete-definition without confirm.
	ErrConfirmation = errors.New("share: confirmation required")
	// ErrNoArray is create/update/delete when array topology has never
	// been written.
	ErrNoArray = errors.New("share: no array topology")
)

var timeMachineSize = regexp.MustCompile(`^[1-9][0-9]*[KMGT]?$`)

func defaultSMB() SMB {
	return SMB{Enabled: true, Browseable: true}
}

func validateCacheMode(mode pool.CacheMode) error {
	switch mode {
	case pool.CacheThenMove, pool.CacheOnly, pool.ArrayOnly:
		return nil
	default:
		return fmt.Errorf("%w: unknown cache mode %q", ErrInvalidInput, mode)
	}
}

func validateCreatePolicy(p pool.CreatePolicy) error {
	switch p {
	case pool.KeepFoldersTogether, pool.BalanceAcrossDisks, pool.QuietDisks, pool.FillDisksInOrder:
		return nil
	default:
		return fmt.Errorf("%w: unknown create policy %q", ErrInvalidInput, p)
	}
}

func validateSMB(smb SMB) error {
	if smb.TimeMachine {
		if smb.TimeMachineMaxSize == "" {
			return fmt.Errorf("%w: Time Machine shares require a maximum size (Q73)", ErrInvalidInput)
		}
		if !timeMachineSize.MatchString(smb.TimeMachineMaxSize) {
			return fmt.Errorf("%w: Time Machine max size %q is not a Samba size (e.g. 500G)", ErrInvalidInput, smb.TimeMachineMaxSize)
		}
	}
	return nil
}
