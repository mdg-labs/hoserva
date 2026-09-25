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

// Removal states persisted in array_disks.removal_state (#359, doc 09
// §4's own disk-removal state machine). All four are the CHECK
// constraint's own spellings, declared together because SQLite cannot
// widen a CHECK without rebuilding the table (D16): RemovalStateEvacuating
// and RemovalStateEvacuated are this issue's own states; RemovalStateUnpooled
// and RemovalStateUnlisted belong to #358's own later removal steps.
const (
	RemovalStateEvacuating = "evacuating"
	RemovalStateEvacuated  = "evacuated"
	RemovalStateUnpooled   = "unpooled"
	RemovalStateUnlisted   = "unlisted"
)

var (
	// ErrNoArray is GetArray's result when create-array has never
	// succeeded — SQLite has no topology row, so generators must not
	// write units or snapraid.conf.
	ErrNoArray = errors.New("store: no array topology")
	// ErrArrayExists is PutArray's refusal when topology is already
	// persisted, and create-array's refusal of a retry whose plan does
	// not match stored devices/roles. A matching retry re-applies from
	// SQLite and never formats (#181).
	ErrArrayExists = errors.New("store: array topology already exists")
	// ErrArrayDiskExists is AddDataDisk/ReplaceDataDisk's refusal when the
	// device, filesystem UUID or mountpoint given is already claimed by
	// another row — the same UNIQUE constraints PutArray's own inserts
	// rely on (#288).
	ErrArrayDiskExists = errors.New("store: a disk already occupies that device, filesystem or mountpoint")
	// ErrArrayDiskNotFound is ReplaceDataDisk/GetDataDiskByMountpoint's
	// refusal when no data disk occupies the given mountpoint — including
	// when it names a parity or cache slot instead, since doc 02 §4
	// "Replacing a failed disk" is specific to data disks (#288).
	ErrArrayDiskNotFound = errors.New("store: no data disk at that mountpoint")
	// ErrArrayParityDiskNotFound is UpgradeParityDisk's refusal when no
	// parity disk occupies the given mountpoint (#289).
	ErrArrayParityDiskNotFound = errors.New("store: no parity disk at that mountpoint")
	// ErrAnotherDiskRemoving is SetRemovalState's refusal when a data disk
	// other than the one named already has a non-NULL removal_state
	// (#359, doc 09 §4 Open questions: one disk in removal at a time —
	// the *Removing mount builders each take a single removingDisk).
	ErrAnotherDiskRemoving = errors.New("store: another disk is already in removal")
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
	// Size is the disk's capacity in bytes at the time it joined the
	// array (Q21). SizeSet is false for rows written before that column
	// existed, or when a write path had no size to record — a weak-
	// identity match then falls back to filesystem UUID alone.
	Size    int64
	SizeSet bool
	// RemovalState is one of the RemovalState* constants above, or ""
	// for a disk not currently in removal (#359).
	RemovalState string
	// RemovalJobID is the job holding RemovalState, or "" when there is
	// none.
	RemovalJobID string
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
			SizeBytes:    nullInt64(d.Size, d.SizeSet),
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
		disks = append(disks, arrayDiskFromRow(r))
	}
	return settings, disks, nil
}

// AddDataDisk inserts one new data-disk row into an already-existing array
// (doc 02 §4 "Adding a disk" steps 5-6). Refuses (ErrNoArray) before
// create-array has ever run — this method only ever grows an array that
// already exists, never creates the first one (PutArray's own job) — and
// (ErrArrayDiskExists) when d's device, filesystem UUID or mountpoint is
// already claimed by another row.
func (s *ArrayStore) AddDataDisk(ctx context.Context, d ArrayDisk) error {
	exists, err := s.Exists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNoArray
	}
	if err := s.q.InsertArrayDisk(ctx, storedb.InsertArrayDiskParams{
		Role:         ArrayRoleData,
		RoleIndex:    int64(d.RoleIndex),
		Device:       d.Device,
		Filesystem:   d.Filesystem,
		FsUuid:       d.FSUUID,
		SizeBytes:    nullInt64(d.Size, d.SizeSet),
		Wwn:          nullString(d.WWN),
		Serial:       nullString(d.Serial),
		ByIDName:     nullString(d.ByIDName),
		WeakIdentity: boolToInt(d.WeakIdentity),
		Mountpoint:   d.Mountpoint,
	}); err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: %s", ErrArrayDiskExists, d.Device)
		}
		return fmt.Errorf("store: inserting data disk %s: %w", d.Device, err)
	}
	return nil
}

// GetDataDiskByMountpoint returns the data disk currently assigned to
// mountpoint (e.g. "/mnt/disk2"), or ErrArrayDiskNotFound when no data disk
// occupies it — including when mountpoint names a parity or cache slot
// instead (doc 02 §4 "Replacing a failed disk" is specific to data disks).
func (s *ArrayStore) GetDataDiskByMountpoint(ctx context.Context, mountpoint string) (ArrayDisk, error) {
	row, err := s.q.GetArrayDataDiskByMountpoint(ctx, mountpoint)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ArrayDisk{}, ErrArrayDiskNotFound
		}
		return ArrayDisk{}, err
	}
	return arrayDiskFromRow(row), nil
}

// ReplaceDataDisk re-points the data disk at mountpoint to a replacement's
// identity (doc 02 §4 "Replacing a failed disk" step 3): role and
// role_index are left unchanged, so mount units and snapraid.conf
// regenerate at the exact same slot the failed disk used. Refuses
// (ErrArrayDiskNotFound) when no data disk occupies mountpoint, and
// (ErrArrayDiskExists) when the replacement's own device or filesystem
// UUID collides with a disk already in the array.
func (s *ArrayStore) ReplaceDataDisk(ctx context.Context, mountpoint string, d ArrayDisk) error {
	n, err := s.q.ReplaceArrayDataDiskIdentity(ctx, storedb.ReplaceArrayDataDiskIdentityParams{
		Device:       d.Device,
		Filesystem:   d.Filesystem,
		FsUuid:       d.FSUUID,
		SizeBytes:    nullInt64(d.Size, d.SizeSet),
		Wwn:          nullString(d.WWN),
		Serial:       nullString(d.Serial),
		ByIDName:     nullString(d.ByIDName),
		WeakIdentity: boolToInt(d.WeakIdentity),
		Mountpoint:   mountpoint,
	})
	if err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: %s", ErrArrayDiskExists, d.Device)
		}
		return fmt.Errorf("store: replacing data disk at %s: %w", mountpoint, err)
	}
	if n == 0 {
		return ErrArrayDiskNotFound
	}
	return nil
}

// UpgradeParityDisk re-points the parity slot at oldMountpoint to d's own
// identity and mountpoint (doc 02 §4 "Larger parity disk" #289): role and
// role_index are left unchanged, so the array keeps exactly the same
// number of parity disks it always had, but the new disk takes over that
// slot at its own fresh mountpoint rather than the old disk's — unlike
// ReplaceDataDisk, which reuses the failed disk's own mountpoint, a parity
// upgrade's new disk is formatted, mounted and verified at an independent
// path while the old parity file stays exactly as valid as it was before
// the upgrade started (Q71). Refuses (ErrArrayParityDiskNotFound) when no
// parity disk occupies oldMountpoint, and (ErrArrayDiskExists) when d's
// own device, filesystem UUID or mountpoint collides with a disk already
// in the array.
func (s *ArrayStore) UpgradeParityDisk(ctx context.Context, oldMountpoint string, d ArrayDisk) error {
	n, err := s.q.UpgradeArrayParityDiskSlot(ctx, storedb.UpgradeArrayParityDiskSlotParams{
		Device:        d.Device,
		Filesystem:    d.Filesystem,
		FsUuid:        d.FSUUID,
		SizeBytes:     nullInt64(d.Size, d.SizeSet),
		Wwn:           nullString(d.WWN),
		Serial:        nullString(d.Serial),
		ByIDName:      nullString(d.ByIDName),
		WeakIdentity:  boolToInt(d.WeakIdentity),
		NewMountpoint: d.Mountpoint,
		OldMountpoint: oldMountpoint,
	})
	if err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: %s", ErrArrayDiskExists, d.Device)
		}
		return fmt.Errorf("store: upgrading parity disk at %s: %w", oldMountpoint, err)
	}
	if n == 0 {
		return ErrArrayParityDiskNotFound
	}
	return nil
}

func arrayDiskFromRow(r *storedb.ArrayDisk) ArrayDisk {
	return ArrayDisk{
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
		Size:         r.SizeBytes.Int64,
		SizeSet:      r.SizeBytes.Valid,
		RemovalState: r.RemovalState.String,
		RemovalJobID: r.RemovalJobID.String,
	}
}

// SetRemovalState marks the data disk at mountpoint with state — one of
// the RemovalState* constants above (#359, doc 09 §4 step 2) — and
// records jobID as the job holding it. It refuses
// (ErrAnotherDiskRemoving) when a different disk already has a non-NULL
// removal_state, inside the same transaction: only one disk is ever in
// removal at a time (the *Removing mount builders each take a single
// removingDisk). The same disk again succeeds and takes jobID as its new
// holder: a resume re-applies its own state, and a new evacuation of a
// disk a failed one left "evacuating" takes it over. Refuses
// (ErrArrayDiskNotFound) when mountpoint does not name a data disk, and
// refuses an empty jobID.
func (s *ArrayStore) SetRemovalState(ctx context.Context, mountpoint, state, jobID string) error {
	if jobID == "" {
		return fmt.Errorf("store: marking %s %s: no job id", mountpoint, state)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning removal-state transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := s.q.WithTx(tx)
	current, err := q.GetRemovingArrayDisk(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: reading the current removing disk: %w", err)
	}
	if err == nil && current.Mountpoint != mountpoint {
		return fmt.Errorf("%w: %s", ErrAnotherDiskRemoving, current.Mountpoint)
	}

	n, err := q.SetArrayDiskRemovalState(ctx, storedb.SetArrayDiskRemovalStateParams{
		RemovalState: sql.NullString{String: state, Valid: true},
		RemovalJobID: sql.NullString{String: jobID, Valid: true},
		Mountpoint:   mountpoint,
	})
	if err != nil {
		return fmt.Errorf("store: marking %s %s: %w", mountpoint, state, err)
	}
	if n == 0 {
		return ErrArrayDiskNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing removal-state change: %w", err)
	}
	return nil
}

// ReleaseRemovalState clears mountpoint's removal state back to NULL
// only while it is "evacuating" and held by jobID, and reports whether
// it did. A cancelled evacuation calls it with its own job id: any other
// holder (a later evacuation of the same disk), and an "evacuated" disk
// (#361's cancelDiskRemoval, not an evacuation cancel), are left as
// they are.
func (s *ArrayStore) ReleaseRemovalState(ctx context.Context, mountpoint, jobID string) (bool, error) {
	if jobID == "" {
		return false, fmt.Errorf("store: releasing removal state at %s: no job id", mountpoint)
	}
	n, err := s.q.ReleaseArrayDiskRemovalState(ctx, storedb.ReleaseArrayDiskRemovalStateParams{
		Mountpoint:   mountpoint,
		RemovalJobID: sql.NullString{String: jobID, Valid: true},
	})
	if err != nil {
		return false, fmt.Errorf("store: releasing removal state at %s: %w", mountpoint, err)
	}
	return n > 0, nil
}

// RemovingDisk returns the mountpoint and state of the array's one disk
// currently in removal (#359), or ("", "", nil) when none is. Used by
// planDiskEvacuation/evacuateDisk to refuse a second disk while one is
// already in removal.
func (s *ArrayStore) RemovingDisk(ctx context.Context) (mountpoint, state string, err error) {
	row, err := s.q.GetRemovingArrayDisk(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("store: reading the current removing disk: %w", err)
	}
	return row.Mountpoint, row.RemovalState.String, nil
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullInt64(v int64, set bool) sql.NullInt64 {
	if !set {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
