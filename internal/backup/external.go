package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

const externalDestinationPrefix = "external:"

// ExternalDestination is a local config-backup target on an external
// disk's mount (doc 10 §1, Q72). Path is /mnt/disks/<label> — the same
// stable path a container bind-mount uses.
func ExternalDestination(label string) (Destination, error) {
	path, err := disk.ExternalMountPoint(label)
	if err != nil {
		return Destination{}, err
	}
	return Destination{
		ID:      externalDestinationPrefix + label,
		Name:    label,
		Type:    TypeLocal,
		Path:    path,
		Enabled: true,
		Retention: Retention{
			Daily:   DefaultRetentionDaily,
			Weekly:  DefaultRetentionWeekly,
			Monthly: DefaultRetentionMonthly,
		},
	}, nil
}

// PrepareExternalDestination returns the destination an external disk's
// backup-destination flag creates, refusing it with ErrDestinationExists
// when another destination already has its name or its path. The disk's
// own destination, already among existing, is returned as it is. It is the
// one admission check both the daemon and cmd/mockapi run (D18).
func PrepareExternalDestination(label string, existing []Destination) (Destination, error) {
	dest, err := ExternalDestination(label)
	if err != nil {
		return Destination{}, err
	}
	for _, e := range existing {
		if e.ID == dest.ID {
			return e, nil
		}
	}
	for _, e := range existing {
		if strings.EqualFold(e.Name, dest.Name) || sameTarget(e, dest) {
			return Destination{}, ErrDestinationExists
		}
	}
	return dest, nil
}

// ExternalDestinationStore is the DestinationStore capability that keeps
// an external disk's backup-destination flag and its "external:<label>"
// destination row from drifting (D4): each method changes both in one
// transaction. DeleteDestination of an "external:<label>" id clears that
// disk's flag in the same transaction too.
type ExternalDestinationStore interface {
	// SetExternalDestination sets label's flag to dest != nil and, in the
	// same transaction, creates dest unless a row with its id exists; a nil
	// dest deletes the row "external:<label>", if any. It returns
	// store.ErrExternalNotFound for an unregistered label.
	SetExternalDestination(ctx context.Context, label string, dest *Destination) error
	// PutExternalDisk registers d and, when dest is not nil, creates dest in
	// the same transaction.
	PutExternalDisk(ctx context.Context, d store.ExternalDisk, dest *Destination) error
	// FlaggedExternalLabels lists the disks whose flag is set.
	FlaggedExternalLabels(ctx context.Context) ([]string, error)
}

func (s *Service) requireExternalStore() (ExternalDestinationStore, error) {
	st, err := s.requireStore()
	if err != nil {
		return nil, err
	}
	ext, ok := st.(ExternalDestinationStore)
	if !ok {
		return nil, errors.New("backup: the destination store cannot track external disks")
	}
	return ext, nil
}

// SetExternalDestination turns label's backup-destination flag on, which
// creates the disk's destination unless it exists, or off, which removes
// it. Archives already written to the disk stay where they are.
func (s *Service) SetExternalDestination(ctx context.Context, label string, enabled bool) error {
	ext, err := s.requireExternalStore()
	if err != nil {
		return err
	}
	s.destMu.Lock()
	defer s.destMu.Unlock()
	if !enabled {
		return ext.SetExternalDestination(ctx, label, nil)
	}
	dest, err := s.prepareExternalLocked(ctx, label)
	if err != nil {
		return err
	}
	return ext.SetExternalDestination(ctx, label, &dest)
}

// RegisterExternalDisk registers d; with its BackupDestination flag set it
// also creates the disk's destination, in the same transaction.
func (s *Service) RegisterExternalDisk(ctx context.Context, d store.ExternalDisk) error {
	ext, err := s.requireExternalStore()
	if err != nil {
		return err
	}
	s.destMu.Lock()
	defer s.destMu.Unlock()
	if !d.BackupDestination {
		return ext.PutExternalDisk(ctx, d, nil)
	}
	dest, err := s.prepareExternalLocked(ctx, d.Label)
	if err != nil {
		return err
	}
	return ext.PutExternalDisk(ctx, d, &dest)
}

func (s *Service) prepareExternalLocked(ctx context.Context, label string) (Destination, error) {
	existing, err := s.Store.ListDestinations(ctx)
	if err != nil {
		return Destination{}, fmt.Errorf("listing destinations: %w", err)
	}
	dest, err := PrepareExternalDestination(label, existing)
	if err != nil {
		return Destination{}, err
	}
	dest.CreatedAt = s.now()
	return dest, nil
}

// ReconcileExternalDestinations creates the destination of every disk
// whose flag is set but whose destination row is missing, as a flag set
// before the flag created the row leaves it. It attempts every disk and
// reports the ones it could not fix.
func (s *Service) ReconcileExternalDestinations(ctx context.Context) error {
	ext, err := s.requireExternalStore()
	if err != nil {
		return err
	}
	labels, err := ext.FlaggedExternalLabels(ctx)
	if err != nil {
		return fmt.Errorf("listing flagged external disks: %w", err)
	}
	var failures []error
	for _, label := range labels {
		if err := s.SetExternalDestination(ctx, label, true); err != nil {
			failures = append(failures, fmt.Errorf("creating the backup destination of external disk %q: %w", label, err))
		}
	}
	return errors.Join(failures...)
}
