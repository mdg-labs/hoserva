package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
// and RemovalStateEvacuated are the evacuation's own states;
// RemovalStateUnpooled (out of every pool mount, still in snapraid.conf)
// and RemovalStateUnlisted (out of snapraid.conf too; only its unmount
// and this row's deletion are left) are the disk_remove job's (#358).
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
	// ErrMigrationNotPending is RecordParityInit's refusal when the array is not
	// a pending migration's.
	ErrMigrationNotPending = errors.New("store: the array is not a pending Unraid migration")
	// ErrMigrationNotFinishing is FinishMigration's refusal when no migration
	// is part-way through its point of no return.
	ErrMigrationNotFinishing = errors.New("store: no Unraid migration is part-way through its point of no return")
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
	// ErrDiskLeavingArray is SetRemovalState's refusal for a disk already
	// past its evacuation (unpooled or unlisted, #358): it has left the
	// pool, so a new evacuation must not put it back in as no-create.
	ErrDiskLeavingArray = errors.New("store: the disk is already leaving the array")
	// ErrRemovalStateMismatch is AdvanceRemovalState's and
	// DeleteUnlistedDataDisk's refusal when the disk's removal state is
	// not the one the step expects — another writer changed it since the
	// caller read it (#358).
	ErrRemovalStateMismatch = errors.New("store: the disk's removal state is not the one this step expects")
)

// ArraySettings is the singleton pool-wide create-array row: mergerfs
// create policy and min-free-space the wizard collected (doc 02 §1).
type ArraySettings struct {
	CreatePolicy string
	MinFreeSpace string
	CreatedAt    time.Time
	// MigrationPending is true from an Unraid import's adoption until the
	// point of no return clears it (doc 05 §4 steps 14-16): the data disks are
	// mounted read-only, the catch-all pool is read-only, there is no
	// snapraid.conf, and parity and cache are only recorded
	// (RecordedDisk), never formatted or mounted.
	MigrationPending bool
}

// RecordedDisk is a former Unraid parity or cache disk of a pending
// migration: its identity, kept for the point of no return, and nothing
// else. It has no filesystem UUID or mountpoint; it is never mounted.
type RecordedDisk struct {
	Role         string `json:"role"`
	RoleIndex    int    `json:"roleIndex"`
	Device       string `json:"device"`
	Size         int64  `json:"size"`
	WWN          string `json:"wwn,omitempty"`
	Serial       string `json:"serial,omitempty"`
	ByIDName     string `json:"byId,omitempty"`
	WeakIdentity bool   `json:"weakIdentity,omitempty"`
	// PartUUID is the partition table's identifier of a cache on a spare
	// partition of the boot disk, empty for a whole disk.
	PartUUID string `json:"partUuid,omitempty"`
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
	// MountSource is the /dev/disk/by-id path the disk's mount unit binds to
	// instead of its filesystem UUID, or "" to mount by UUID. Only an adopted
	// Unraid data disk has one.
	MountSource string
}

// LeavingArray reports whether d is in removal: from the moment an
// evacuation marks it until the disk_remove job deletes its row (#366).
// Nothing may be placed on a leaving disk, and no disk operation other
// than its own removal may act on it.
func (d ArrayDisk) LeavingArray() bool {
	return d.RemovalState != ""
}

// LeftPool reports whether d's removal is past its evacuation: it is
// unpooled or unlisted, out of every pool mount, and only the
// disk_remove job may take it further (#358). A new evacuation of it is
// refused.
func (d ArrayDisk) LeftPool() bool {
	return d.RemovalState == RemovalStateUnpooled || d.RemovalState == RemovalStateUnlisted
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
	return s.put(ctx, settings, disks, nil)
}

// PutPendingArray is PutArray for an Unraid import's adoption: settings is
// stored with MigrationPending set, disks are the adopted data disks, and
// recorded the former parity and cache disks, kept in the settings row.
// Settings, disks and the record are written in one transaction. It refuses (ErrArrayExists) without writing when topology is
// already present.
func (s *ArrayStore) PutPendingArray(ctx context.Context, settings ArraySettings, disks []ArrayDisk, recorded []RecordedDisk) error {
	settings.MigrationPending = true
	return s.put(ctx, settings, disks, recorded)
}

func (s *ArrayStore) put(ctx context.Context, settings ArraySettings, disks []ArrayDisk, recorded []RecordedDisk) error {
	var recordedJSON string
	if len(recorded) > 0 {
		raw, err := json.Marshal(recorded)
		if err != nil {
			return fmt.Errorf("store: encoding the recorded migration disks: %w", err)
		}
		recordedJSON = string(raw)
	}
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

		MigrationPending:  boolToInt(settings.MigrationPending),
		MigrationRecorded: recordedJSON,
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
			MountSource:  nullString(d.MountSource),
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
		CreatePolicy:     row.CreatePolicy,
		MinFreeSpace:     row.MinFreeSpace,
		CreatedAt:        createdAt,
		MigrationPending: row.MigrationPending != 0,
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

// MigrationPending reports whether the array is an Unraid import's adoption
// still waiting for the point of no return. No array is not pending.
func (s *ArrayStore) MigrationPending(ctx context.Context) (bool, error) {
	row, err := s.q.GetArraySettings(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: reading whether a migration is pending: %w", err)
	}
	return row.MigrationPending != 0, nil
}

// RecordedDisks returns the former parity and cache disks of a pending
// migration, parity first. None is not an error: an array that is not a
// pending migration's has none.
func (s *ArrayStore) RecordedDisks(ctx context.Context) ([]RecordedDisk, error) {
	row, err := s.q.GetArraySettings(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: reading the recorded migration disks: %w", err)
	}
	if row.MigrationRecorded == "" {
		return nil, nil
	}
	var out []RecordedDisk
	if err := json.Unmarshal([]byte(row.MigrationRecorded), &out); err != nil {
		return nil, fmt.Errorf("store: decoding the recorded migration disks: %w", err)
	}
	return out, nil
}

// MigrationFinishing reports whether the point of no return has recorded the
// former parity and cache disks as the array's own but has not finished
// (RecordParityInit has run and FinishMigration has not): the record of them is
// kept until the last step so that a run that stopped in between is finished by
// running it again, never by formatting anything a second time.
func (s *ArrayStore) MigrationFinishing(ctx context.Context) (bool, error) {
	row, err := s.q.GetArraySettings(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: reading whether a migration is finishing: %w", err)
	}
	return row.MigrationPending == 0 && row.MigrationRecorded != "", nil
}

// MigrationUnfinished reports whether an Unraid migration is anywhere between
// its adoption and the end of its point of no return: waiting for it
// (MigrationPending) or part-way through it (MigrationFinishing). Parity,
// array-write and topology jobs are refused for as long as it is true.
func (s *ArrayStore) MigrationUnfinished(ctx context.Context) (bool, error) {
	row, err := s.q.GetArraySettings(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: reading whether a migration is unfinished: %w", err)
	}
	return row.MigrationPending != 0 || row.MigrationRecorded != "", nil
}

// RecordParityInit is the point of no return's one write to the array record:
// in one transaction it adds the formatted former parity disks and the cache as
// array disks and clears migration_pending, but only while the array is a
// pending migration's (ErrMigrationNotPending otherwise, writing nothing). The
// record of the former disks stays until FinishMigration.
func (s *ArrayStore) RecordParityInit(ctx context.Context, disks []ArrayDisk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning the parity initialisation record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE array_settings SET migration_pending = 0 WHERE migration_pending = 1`)
	if err != nil {
		return fmt.Errorf("store: clearing migration_pending: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: clearing migration_pending: %w", err)
	} else if n == 0 {
		return ErrMigrationNotPending
	}
	q := s.q.WithTx(tx)
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
			MountSource:  nullString(d.MountSource),
		}); err != nil {
			return fmt.Errorf("store: inserting array disk %s: %w", d.Device, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing the parity initialisation record: %w", err)
	}
	return nil
}

// FinishMigration drops the record of the former parity and cache disks, the
// last step of the point of no return, and in the same statement records that
// the initial sync is owed (InitialSyncOwed) and when the migration finished
// (MigrationFinishedAt): the migration reads finished from then on, so a stop
// before the sync is queued must leave a durable record of it. It refuses
// (ErrMigrationNotFinishing) unless RecordParityInit has run and this has not.
func (s *ArrayStore) FinishMigration(ctx context.Context) error {
	res, err := s.db.ExecContext(ctx, `UPDATE array_settings SET migration_recorded = '', initial_sync_owed = 1, migration_finished_at = ? WHERE migration_pending = 0 AND migration_recorded != ''`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("store: finishing the migration: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: finishing the migration: %w", err)
	} else if n == 0 {
		return ErrMigrationNotFinishing
	}
	return nil
}

// MigrationFinishedAt is when FinishMigration ran, and false for an array it
// never ran on: no array, one created by hand, a pending adoption, or one whose
// adoption was undone, which deletes the record. A stamp that cannot be read is
// an error, never a finished migration.
func (s *ArrayStore) MigrationFinishedAt(ctx context.Context) (time.Time, bool, error) {
	row, err := s.q.GetArraySettings(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("store: reading when the migration finished: %w", err)
	}
	if row.MigrationFinishedAt == "" {
		return time.Time{}, false, nil
	}
	at, err := time.Parse(time.RFC3339, row.MigrationFinishedAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: reading when the migration finished: %w", err)
	}
	return at, true, nil
}

// InitialSyncOwed reports whether a finished migration's initial sync has not
// been queued yet. An error is never "nothing owed": the caller must not treat
// a failed read as an array whose parity is built.
func (s *ArrayStore) InitialSyncOwed(ctx context.Context) (bool, error) {
	row, err := s.q.GetArraySettings(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: reading whether the initial sync is owed: %w", err)
	}
	return row.InitialSyncOwed != 0, nil
}

// ClearInitialSyncOwed records that the initial sync has been queued (or that
// parity has been built some other way). Clearing what is not owed is not an
// error.
func (s *ArrayStore) ClearInitialSyncOwed(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE array_settings SET initial_sync_owed = 0 WHERE initial_sync_owed != 0`); err != nil {
		return fmt.Errorf("store: clearing the owed initial sync: %w", err)
	}
	return nil
}

// DeletePendingArray deletes the array and its disks (the recorded migration
// disks go with the settings row) in one transaction, but only while the array is a pending migration's
// (MigrationPending): it undoes a failed adoption, and can never delete an
// array that has been through the point of no return or was created by hand.
// It reports whether it deleted one.
func (s *ArrayStore) DeletePendingArray(ctx context.Context) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: beginning pending-array deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	if _, err := q.DeletePendingMigrationArrayDisks(ctx); err != nil {
		return false, fmt.Errorf("store: deleting the pending array's disks: %w", err)
	}
	n, err := q.DeletePendingMigrationArraySettings(ctx)
	if err != nil {
		return false, fmt.Errorf("store: deleting the pending array: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: committing pending-array deletion: %w", err)
	}
	return n > 0, nil
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

// ReplaceDataDiskAbandoningRemoval is ReplaceDataDisk for a slot whose old
// disk is evacuated or unpooled (doc 09 §4 "Other operations…", #384): the
// same identity swap, plus clearing removal_state and removal_job_id back
// to NULL in the same UPDATE, so the replacement rejoins the pool as an
// ordinary disk instead of silently inheriting the old disk's removal
// state the way ReplaceDataDisk alone would (#368) — the caller
// (RunDiskReplace) has already confirmed the old disk is genuinely
// missing before this runs. Refuses (ErrArrayDiskNotFound) when no data
// disk at mountpoint is currently evacuated or unpooled, including when a
// concurrent writer has already moved it past that state, so a failure
// here never leaves a half-adopted disk still carrying the old removal
// state. Refuses (ErrArrayDiskExists) when the replacement's own device or
// filesystem UUID collides with a disk already in the array.
func (s *ArrayStore) ReplaceDataDiskAbandoningRemoval(ctx context.Context, mountpoint string, d ArrayDisk) error {
	n, err := s.q.ReplaceArrayDataDiskIdentityAbandoningRemoval(ctx, storedb.ReplaceArrayDataDiskIdentityAbandoningRemovalParams{
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
		return fmt.Errorf("store: replacing data disk at %s while abandoning its removal: %w", mountpoint, err)
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
		MountSource:  r.MountSource.String,
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
	if err == nil && (ArrayDisk{RemovalState: current.RemovalState.String}).LeftPool() {
		return fmt.Errorf("%w: %s is %s", ErrDiskLeavingArray, mountpoint, current.RemovalState.String)
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

// AdvanceRemovalState moves the data disk at mountpoint from the removal
// state from to to, and records jobID as its holder — the disk_remove
// job's own steps (#358, doc 09 §4 steps 7-8: evacuated to unpooled,
// unpooled to unlisted). A disk already in to is left there, under
// jobID, so a re-run repeats the step. Any other current state refuses
// with ErrRemovalStateMismatch and writes nothing, as does a mountpoint
// that is not a data disk.
func (s *ArrayStore) AdvanceRemovalState(ctx context.Context, mountpoint, from, to, jobID string) error {
	if jobID == "" {
		return fmt.Errorf("store: marking %s %s: no job id", mountpoint, to)
	}
	n, err := s.q.AdvanceArrayDiskRemovalState(ctx, storedb.AdvanceArrayDiskRemovalStateParams{
		ToState:    sql.NullString{String: to, Valid: true},
		JobID:      sql.NullString{String: jobID, Valid: true},
		Mountpoint: mountpoint,
		FromState:  sql.NullString{String: from, Valid: true},
		SameState:  sql.NullString{String: to, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("store: marking %s %s: %w", mountpoint, to, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s is not %s or %s", ErrRemovalStateMismatch, mountpoint, from, to)
	}
	return nil
}

// DeleteUnlistedDataDisk deletes the data disk row at mountpoint, but
// only while its removal state is "unlisted" (#358, doc 09 §4 step 9):
// the disk is already out of every pool mount and out of snapraid.conf.
// Any other state refuses with ErrRemovalStateMismatch, and a mountpoint
// with no data disk with ErrArrayDiskNotFound.
func (s *ArrayStore) DeleteUnlistedDataDisk(ctx context.Context, mountpoint string) error {
	n, err := s.q.DeleteUnlistedArrayDataDisk(ctx, mountpoint)
	if err != nil {
		return fmt.Errorf("store: deleting data disk %s: %w", mountpoint, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := s.GetDataDiskByMountpoint(ctx, mountpoint); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s is not %s", ErrRemovalStateMismatch, mountpoint, RemovalStateUnlisted)
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

// CancelRemovalState clears mountpoint's removal state back to NULL, and
// reports whether it did (#361's cancelDiskRemoval), only while the row
// still holds exactly expectedState and expectedJobID — the values the
// caller read when it decided, from the disk's RemovalJobID and the job
// store, that no evacuation job is still queued, running or interrupted
// for this disk. A re-evacuation of an evacuated disk that starts after
// that read changes the job id, so the cancel leaves the new evacuation's
// state alone and reports false. Only "evacuating" and "evacuated" are
// ever cleared: a disk that is "unpooled" or "unlisted" has already left
// the pool, so only finishDiskRemoval takes it further (doc 09 §4's own
// Open questions). An expectedJobID of "" matches a NULL job id.
func (s *ArrayStore) CancelRemovalState(ctx context.Context, mountpoint, expectedState, expectedJobID string) (bool, error) {
	n, err := s.q.CancelArrayDiskRemovalState(ctx, storedb.CancelArrayDiskRemovalStateParams{
		Mountpoint:    mountpoint,
		ExpectedState: sql.NullString{String: expectedState, Valid: true},
		ExpectedJobID: nullString(expectedJobID),
	})
	if err != nil {
		return false, fmt.Errorf("store: cancelling removal state at %s: %w", mountpoint, err)
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
