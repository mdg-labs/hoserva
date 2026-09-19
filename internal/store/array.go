package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// Array roles persisted in array_disks.role (#180). These are the
// CHECK-constraint spellings, not display labels.
const (
	ArrayRoleParity = "parity"
	ArrayRoleData   = "data"
	ArrayRoleCache  = "cache"
)

var (
	// ErrNoArray is GetArray's result when create-array has never
	// succeeded — SQLite has no topology row, so generators must not
	// write units or snapraid.conf.
	ErrNoArray = errors.New("store: no array topology")
	// ErrArrayExists is PutArray's refusal when topology is already
	// persisted. create-array must not format or rewrite an array that
	// already exists (#180; adding/removing disks is a later issue).
	ErrArrayExists = errors.New("store: array topology already exists")
)

// ArraySettings is the singleton pool-wide create-array row: mergerfs
// create policy and min-free-space the wizard collected (doc 02 §1).
type ArraySettings struct {
	CreatePolicy string
	MinFreeSpace string
	CreatedAt    time.Time
}

// ArrayDisk is one assigned disk after a successful FormatPlan: role,
// identity, filesystem UUID (Q21) and the documented mountpoint
// (doc 01 §6). Job-params JSON is not this record.
type ArrayDisk struct {
	Role         string
	RoleIndex    int
	Device       string
	Filesystem   string
	FSUUID       string
	WWN          string
	Serial       string
	ByIDName     string
	WeakIdentity bool
	Mountpoint   string
}

// ArrayStore persists create-array topology in the central SQLite
// database (D4) through the sqlc-generated internal/store/db package.
// PutArray is the only write: it inserts the singleton settings row and
// every assigned disk in one transaction, and refuses if the singleton
// already exists so a failed FormatPlan can never be followed by a
// silent overwrite of a live array.
type ArrayStore struct {
	db *sql.DB
	q  *storedb.Queries
}

// NewArrayStore wraps db for array-topology persistence.
func NewArrayStore(db *sql.DB) *ArrayStore {
	return &ArrayStore{db: db, q: storedb.New(db)}
}

// Exists reports whether a successful create-array has already written
// the singleton settings row.
func (s *ArrayStore) Exists(ctx context.Context) (bool, error) {
	n, err := s.q.CountArraySettings(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PutArray inserts settings and disks. It refuses (ErrArrayExists)
// without writing if topology is already present, and never deletes.
func (s *ArrayStore) PutArray(ctx context.Context, settings ArraySettings, disks []ArrayDisk) error {
	exists, err := s.Exists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return ErrArrayExists
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning array topology transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := s.q.WithTx(tx)
	if err := q.InsertArraySettings(ctx, storedb.InsertArraySettingsParams{
		CreatePolicy: settings.CreatePolicy,
		MinFreeSpace: settings.MinFreeSpace,
		CreatedAt:    settings.CreatedAt.UTC().Format(TimeFormat),
	}); err != nil {
		return fmt.Errorf("store: inserting array settings: %w", err)
	}
	for _, d := range disks {
		if err := q.InsertArrayDisk(ctx, storedb.InsertArrayDiskParams{
			Role:         d.Role,
			RoleIndex:    int64(d.RoleIndex),
			Device:       d.Device,
			Filesystem:   d.Filesystem,
			FsUuid:       d.FSUUID,
			Wwn:          nullString(d.WWN),
			Serial:       nullString(d.Serial),
			ByIDName:     nullString(d.ByIDName),
			WeakIdentity: boolToInt(d.WeakIdentity),
			Mountpoint:   d.Mountpoint,
		}); err != nil {
			return fmt.Errorf("store: inserting array disk %s: %w", d.Device, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing array topology: %w", err)
	}
	return nil
}

// GetArray returns the persisted topology, or ErrNoArray.
func (s *ArrayStore) GetArray(ctx context.Context) (ArraySettings, []ArrayDisk, error) {
	row, err := s.q.GetArraySettings(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ArraySettings{}, nil, ErrNoArray
		}
		return ArraySettings{}, nil, err
	}
	createdAt, err := time.Parse(TimeFormat, row.CreatedAt)
	if err != nil {
		return ArraySettings{}, nil, fmt.Errorf("store: parsing array created_at: %w", err)
	}
	settings := ArraySettings{
		CreatePolicy: row.CreatePolicy,
		MinFreeSpace: row.MinFreeSpace,
		CreatedAt:    createdAt,
	}

	rows, err := s.q.ListArrayDisks(ctx)
	if err != nil {
		return ArraySettings{}, nil, err
	}
	disks := make([]ArrayDisk, 0, len(rows))
	for _, r := range rows {
		disks = append(disks, ArrayDisk{
			Role:         r.Role,
			RoleIndex:    int(r.RoleIndex),
			Device:       r.Device,
			Filesystem:   r.Filesystem,
			FSUUID:       r.FsUuid,
			WWN:          r.Wwn.String,
			Serial:       r.Serial.String,
			ByIDName:     r.ByIDName.String,
			WeakIdentity: r.WeakIdentity != 0,
			Mountpoint:   r.Mountpoint,
		})
	}
	return settings, disks, nil
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
