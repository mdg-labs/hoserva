package share

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// ImportFromHost inserts share rows parsed from existing smb.conf /
// exports content (Q76). sambaRaw / nfsRaw nil skips that source; a
// non-nil empty slice parses as no sections. Names already in the
// database are skipped without error; names pool.ValidateShareName
// refuses are skipped with a warning. Inserted names are returned so
// ApplyHostConfig can roll them back if a later step fails. This never
// writes smb.conf or /etc/exports — regeneration happens on the next
// share topology write (D4, D10).
func (s *Service) ImportFromHost(ctx context.Context, sambaRaw, nfsRaw []byte) (inserted []string, err error) {
	if s.Shares == nil {
		return nil, fmt.Errorf("share: ImportFromHost requires a ShareStore")
	}
	now := s.now().UTC()
	pending := map[string]store.Share{}

	if sambaRaw != nil {
		details, err := config.ParseSambaShareDetails(sambaRaw)
		if err != nil {
			return nil, fmt.Errorf("share: parsing smb.conf for import: %w", err)
		}
		for _, d := range details {
			if err := validateName(d.Name); err != nil {
				log.Printf("share: skipping invalid Samba section %q on import: %v", d.Name, err)
				continue
			}
			rec := pending[d.Name]
			if rec.Name == "" {
				rec = importDefaults(d.Name, now)
			}
			rec.SMBEnabled = true
			rec.SMBGuest = d.Guest
			rec.SMBReadOnly = d.ReadOnly
			rec.SMBBrowseable = d.Browseable
			rec.SMBRecycle = d.Recycle
			rec.SMBTimeMachine = d.TimeMachine
			rec.SMBTimeMachineMaxSize = d.TimeMachineMaxSize
			if !rec.SMBTimeMachine {
				rec.SMBTimeMachineMaxSize = ""
			}
			pending[d.Name] = rec
		}
	}

	if nfsRaw != nil {
		details, err := config.ParseNFSExportDetails(nfsRaw)
		if err != nil {
			return nil, fmt.Errorf("share: parsing exports for import: %w", err)
		}
		for _, d := range details {
			name := d.Name
			if name == "" || name == "." || name == string(filepath.Separator) {
				log.Printf("share: skipping NFS export %q on import: no usable share name", d.Path)
				continue
			}
			if err := validateName(name); err != nil {
				log.Printf("share: skipping invalid NFS export %q (name %q) on import: %v", d.Path, name, err)
				continue
			}
			if len(d.Hosts) == 0 {
				log.Printf("share: skipping NFS export %q on import: no client hosts", d.Path)
				continue
			}
			rec := pending[name]
			if rec.Name == "" {
				rec = importDefaults(name, now)
			}
			rec.NFSEnabled = true
			rec.NFSHosts = append([]string(nil), d.Hosts...)
			rec.NFSSquash = d.Squash
			if rec.NFSSquash == "" {
				rec.NFSSquash = squashRoot
			}
			pending[name] = rec
		}
	}

	for _, rec := range pending {
		_, err := s.Shares.Get(ctx, rec.Name)
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrShareNotFound) {
			return nil, rollbackImport(ctx, s.Shares, inserted, err)
		}
		if err := s.Shares.Insert(ctx, rec); err != nil {
			if errors.Is(err, store.ErrShareExists) {
				continue
			}
			return nil, rollbackImport(ctx, s.Shares, inserted, err)
		}
		inserted = append(inserted, rec.Name)
	}
	return inserted, nil
}

func importDefaults(name string, now time.Time) store.Share {
	return store.Share{
		Name:         name,
		CacheMode:    string(pool.ArrayOnly),
		CreatePolicy: string(pool.DefaultCreatePolicy),
		CreatedAt:    now,
		UpdatedAt:    now,
		NFSSquash:    squashRoot,
	}
}

func rollbackImport(ctx context.Context, shares *store.ShareStore, inserted []string, cause error) error {
	for i := len(inserted) - 1; i >= 0; i-- {
		if err := shares.Delete(ctx, inserted[i]); err != nil && !errors.Is(err, store.ErrShareNotFound) {
			return fmt.Errorf("%w (rolling back imported share %s: %v)", cause, inserted[i], err)
		}
	}
	return cause
}
