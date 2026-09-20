package share

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"unicode"

	"github.com/mdg-labs/hoserva/internal/pool"
)

var (
	// ErrInvalidInput is a create/update that names a bad cache mode,
	// create policy, Time Machine size, or NFS host/subnet/squash.
	ErrInvalidInput = errors.New("share: invalid input")
	// ErrConfirmation is delete-data whose confirmation is not the
	// share name, or delete-definition without confirm.
	ErrConfirmation = errors.New("share: confirmation required")
	// ErrNoArray is create/update/delete when array topology has never
	// been written.
	ErrNoArray = errors.New("share: no array topology")
)

var (
	timeMachineSize = regexp.MustCompile(`^[1-9][0-9]*[KMGT]?$`)
	nfsHostname     = regexp.MustCompile(`^(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))*$`)
)

const (
	squashRoot   = "root_squash"
	squashNoRoot = "no_root_squash"
	squashAll    = "all_squash"
)

func defaultSMB() SMB {
	return SMB{Enabled: true, Browseable: true}
}

func defaultNFS() NFS {
	return NFS{Squash: squashRoot}
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

func validateNFS(nfs NFS) error {
	switch nfs.Squash {
	case squashRoot, squashNoRoot, squashAll:
	default:
		return fmt.Errorf("%w: unknown NFS squash option %q", ErrInvalidInput, nfs.Squash)
	}
	if nfs.Enabled && len(nfs.Hosts) == 0 {
		return fmt.Errorf("%w: NFS shares require at least one allowed host or subnet", ErrInvalidInput)
	}
	for _, h := range nfs.Hosts {
		if err := validateNFSClient(h); err != nil {
			return err
		}
	}
	return nil
}

func validateNFSClient(h string) error {
	if h == "" || strings.TrimSpace(h) != h {
		return fmt.Errorf("%w: NFS host or subnet %q is empty or has surrounding whitespace", ErrInvalidInput, h)
	}
	if strings.ContainsFunc(h, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("#()[]{},;|&$`\\\"'<>", r)
	}) {
		return fmt.Errorf("%w: NFS host or subnet %q is not a hostname, IP address, or CIDR subnet", ErrInvalidInput, h)
	}
	if net.ParseIP(h) != nil {
		return nil
	}
	if _, _, err := net.ParseCIDR(h); err == nil {
		return nil
	}
	if len(h) <= 253 && nfsHostname.MatchString(h) {
		return nil
	}
	return fmt.Errorf("%w: NFS host or subnet %q is not a hostname, IP address, or CIDR subnet", ErrInvalidInput, h)
}
