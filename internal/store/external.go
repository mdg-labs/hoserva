package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

var (
	// ErrExternalNotFound is GetExternalDisk when no row matches the label.
	ErrExternalNotFound = errors.New("store: external disk not found")
	// ErrExternalExists is PutExternalDisk's refusal of a second row with
	// the same label, device, UUID or mountpoint.
	ErrExternalExists = errors.New("store: external disk already registered")
)

// ExternalDisk is one disk outside the array (Q72): mounted on request at
// /mnt/disks/<label>, never a pool or parity member.
type ExternalDisk struct {
	Label             string
	Device            string
	Filesystem        string
	FSUUID            string
	WWN               string
	Serial            string
	ByIDName          string
	WeakIdentity      bool
	Mountpoint        string
	BackupDestination bool
}

// ExternalStore persists Q72 external-disk rows in the central SQLite
// database (D4). It is a separate table from array_disks so an external
// disk can never hold a parity, data or cache role.
type ExternalStore struct {
	db *sql.DB
	q  *storedb.Queries
}

// NewExternalStore wraps db for external-disk persistence.
func NewExternalStore(db *sql.DB) *ExternalStore {
	return &ExternalStore{db: db, q: storedb.New(db)}
}

// External returns an ExternalStore sharing this ArrayStore's database.
func (s *ArrayStore) External() *ExternalStore {
	return &ExternalStore{db: s.db, q: s.q}
}

// PutExternalDisk inserts d. Unique-constraint collisions are
// ErrExternalExists rather than a raw SQLite error.
func (s *ExternalStore) PutExternalDisk(ctx context.Context, d ExternalDisk) error {
	err := s.q.InsertExternalDisk(ctx, storedb.InsertExternalDiskParams{
		Label:             d.Label,
		Device:            d.Device,
		Filesystem:        d.Filesystem,
		FsUuid:            d.FSUUID,
		Wwn:               nullString(d.WWN),
		Serial:            nullString(d.Serial),
		ByIDName:          nullString(d.ByIDName),
		WeakIdentity:      boolToInt(d.WeakIdentity),
		Mountpoint:        d.Mountpoint,
		BackupDestination: boolToInt(d.BackupDestination),
	})
	if err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: %s", ErrExternalExists, d.Label)
		}
		return fmt.Errorf("store: inserting external disk %s: %w", d.Label, err)
	}
	return nil
}

// GetExternalDisk returns the row for label, or ErrExternalNotFound.
func (s *ExternalStore) GetExternalDisk(ctx context.Context, label string) (ExternalDisk, error) {
	row, err := s.q.GetExternalDiskByLabel(ctx, label)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExternalDisk{}, ErrExternalNotFound
		}
		return ExternalDisk{}, err
	}
	return externalFromRow(row), nil
}

// GetExternalDiskByDevice returns the row for device, or ErrExternalNotFound.
func (s *ExternalStore) GetExternalDiskByDevice(ctx context.Context, device string) (ExternalDisk, error) {
	row, err := s.q.GetExternalDiskByDevice(ctx, device)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExternalDisk{}, ErrExternalNotFound
		}
		return ExternalDisk{}, err
	}
	return externalFromRow(row), nil
}

// ListExternalDisks returns every registered external disk, ordered by label.
func (s *ExternalStore) ListExternalDisks(ctx context.Context) ([]ExternalDisk, error) {
	rows, err := s.q.ListExternalDisks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ExternalDisk, 0, len(rows))
	for _, r := range rows {
		out = append(out, externalFromRow(r))
	}
	return out, nil
}

// UpdateExternalDisk replaces the persisted identity for label.
func (s *ExternalStore) UpdateExternalDisk(ctx context.Context, d ExternalDisk) error {
	n, err := s.q.UpdateExternalDisk(ctx, storedb.UpdateExternalDiskParams{
		Device:            d.Device,
		Filesystem:        d.Filesystem,
		FsUuid:            d.FSUUID,
		Wwn:               nullString(d.WWN),
		Serial:            nullString(d.Serial),
		ByIDName:          nullString(d.ByIDName),
		WeakIdentity:      boolToInt(d.WeakIdentity),
		Mountpoint:        d.Mountpoint,
		BackupDestination: boolToInt(d.BackupDestination),
		Label:             d.Label,
	})
	if err != nil {
		return fmt.Errorf("store: updating external disk %s: %w", d.Label, err)
	}
	if n == 0 {
		return ErrExternalNotFound
	}
	return nil
}

// SetBackupDestination sets whether label is a local backup destination.
func (s *ExternalStore) SetBackupDestination(ctx context.Context, label string, enabled bool) error {
	n, err := s.q.SetExternalBackupDestination(ctx, storedb.SetExternalBackupDestinationParams{
		BackupDestination: boolToInt(enabled),
		Label:             label,
	})
	if err != nil {
		return fmt.Errorf("store: setting backup destination on %s: %w", label, err)
	}
	if n == 0 {
		return ErrExternalNotFound
	}
	return nil
}

func externalFromRow(r *storedb.ExternalDisk) ExternalDisk {
	return ExternalDisk{
		Label:             r.Label,
		Device:            r.Device,
		Filesystem:        r.Filesystem,
		FSUUID:            r.FsUuid,
		WWN:               r.Wwn.String,
		Serial:            r.Serial.String,
		ByIDName:          r.ByIDName.String,
		WeakIdentity:      r.WeakIdentity != 0,
		Mountpoint:        r.Mountpoint,
		BackupDestination: r.BackupDestination != 0,
	}
}
