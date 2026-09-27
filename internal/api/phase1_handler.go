package api

import (
	"archive/tar"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

func (h *Handler) GetPool(ctx context.Context) (*apiv1.PoolStatus, error) {
	settings, arrayDisks := h.arrayTopology(ctx)
	entries := []apiv1.PoolDiskEntry{}
	matched := make([]bool, len(arrayDisks))
	presentDevices := map[string]bool{}
	// mountFailed is cmd/hoservad's own live record of the storage-target
	// gate's last bounded mount attempt (#398) — never a device probe on
	// this poll path (Q13): h.MountFailedSlots reads state storageTargetSync
	// already recorded when it happened. nil (no hook wired) reports no
	// slot needing attention from this signal.
	mountFailed := map[string]bool{}
	if h.MountFailedSlots != nil {
		mountFailed = h.MountFailedSlots()
	}
	if h.Disks != nil {
		disks, err := h.Disks.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing disks: %w", err)
		}
		var present []disk.Disk
		for _, d := range disks {
			if !d.Boot {
				present = append(present, d)
			}
		}
		// Count claimants per member first: a weak-identity member and a
		// dd-made clone share a filesystem UUID, and choosing between
		// them by inventory order would report the wrong disk (or both)
		// as the member. An ambiguous member is assigned to neither.
		matchIdx := make([]int, len(present))
		claimants := make([]int, len(arrayDisks))
		for i, d := range present {
			matchIdx[i] = -1
			if idx, ok := matchArrayDisk(d, arrayDisks); ok {
				matchIdx[i] = idx
				claimants[idx]++
			}
		}
		for i, d := range present {
			presentDevices[d.Device] = true
			role := apiv1.PoolDiskEntryRoleUnassigned
			mountPoint := ""
			entry := apiv1.PoolDiskEntry{
				Device:     d.Device,
				MountPoint: mountPoint,
				Role:       role,
				State:      apiv1.DiskStateActive,
				SizeBytes:  apiv1.NewOptNilInt64(d.Size),
			}
			if idx := matchIdx[i]; idx >= 0 && claimants[idx] == 1 {
				matched[idx] = true
				entry.Role = arrayRoleToAPI(arrayDisks[idx].Role)
				entry.MountPoint = arrayDisks[idx].Mountpoint
				entry.RemovalState = removalStateToAPI(arrayDisks[idx].RemovalState)
				entry.FinishConfirmation = finishConfirmationForState(arrayDisks[idx].Mountpoint, arrayDisks[idx].RemovalState)
				switch {
				case wrongFilesystem(arrayDisks[idx], d):
					entry.State = apiv1.DiskStateWrongFilesystem
				case mountFailed[arrayDisks[idx].Mountpoint]:
					entry.State = apiv1.DiskStateMountFailed
				}
			}
			entries = append(entries, entry)
		}
	}
	// Every stored array member with no identity match above is a dead or
	// pulled drive (doc 02 §4) — reported as its own entry, at its stored
	// device/role/mountpoint with no size, rather than silently dropped
	// (#326). Its stored /dev path is dropped when a present disk now
	// holds that path: the pool and disks pages key entries by device.
	for i, ad := range arrayDisks {
		if matched[i] {
			continue
		}
		device := ad.Device
		if presentDevices[device] {
			device = ""
		}
		entries = append(entries, apiv1.PoolDiskEntry{
			Device:             device,
			MountPoint:         ad.Mountpoint,
			Role:               arrayRoleToAPI(ad.Role),
			RemovalState:       removalStateToAPI(ad.RemovalState),
			FinishConfirmation: finishConfirmationForState(ad.Mountpoint, ad.RemovalState),
			State:              apiv1.DiskStateMissing,
		})
	}
	mounted, err := pathIsMountpoint(pool.CatchAllPath)
	if err != nil {
		mounted = false
	}
	status := &apiv1.PoolStatus{Mounted: mounted, Disks: entries}
	h.populatePoolSpace(ctx, status, settings)
	return status, nil
}

// matchArrayDisk finds the stored array_disks row that identifies the same
// physical disk as d (Q21), reusing disk.Identity.Matches rather than a
// second implementation: WWN when both sides have one, else serial. It
// never falls back to comparing /dev/sdX paths, which renumber across
// reboots (#326) — a stored member whose device changed is still found by
// its identity, and a different disk that took over its old path is left
// unmatched. A weak-identity array disk (no wwn/serial by-id link at all,
// e.g. every disk in the loop-device lab, doc 06 §3) is matched by
// filesystem UUID and size (Q21) instead, since disk.Identity carries no
// field to compare those through Matches. When the stored row has no size
// (NULL, rows from before #327), the match falls back to filesystem UUID
// alone so an upgrade never leaves a live member unmatched.
func matchArrayDisk(d disk.Disk, arrayDisks []store.ArrayDisk) (int, bool) {
	inv := disk.Identity{WWN: d.WWN, Serial: d.Serial, WeakIdentity: d.WeakIdentity, ByIDName: d.ByIDName}
	for i, ad := range arrayDisks {
		stored := disk.Identity{WWN: ad.WWN, Serial: ad.Serial, WeakIdentity: ad.WeakIdentity, ByIDName: ad.ByIDName}
		if inv.Matches(stored) {
			return i, true
		}
		if ad.WeakIdentity && d.WeakIdentity && ad.FSUUID != "" && ad.FSUUID == d.FSUUID {
			if ad.SizeSet && ad.Size != d.Size {
				continue
			}
			return i, true
		}
	}
	return 0, false
}

// wrongFilesystem reports whether a present disk matched to ad by identity
// (Q21) carries a filesystem UUID different from what SQLite recorded for
// that slot (#388) — a replacement disk that kept the original disk's
// serial/WWN (a cloned or reused drive, or one from the same enclosure)
// but was formatted differently, or not at all. disk.FSUUIDMismatch is the
// one shared definition of this rule (also used by
// disk.StorageGate.evaluate and job.ConfirmReplacementTargetAbsent's own
// relaxation) — either side empty is not a mismatch there either: a slot
// never formatted, or a present disk this build could not read a
// filesystem UUID for, has nothing to compare.
func wrongFilesystem(ad store.ArrayDisk, d disk.Disk) bool {
	return disk.FSUUIDMismatch(ad.FSUUID, d.FSUUID)
}

// arrayTopology returns the persisted array settings and every assigned
// disk, or a zero settings value and nil slice with no ArrayStore
// configured or no array created yet (GetPool must still report disk
// inventory before create-array has run).
func (h *Handler) arrayTopology(ctx context.Context) (store.ArraySettings, []store.ArrayDisk) {
	if h.ArrayStore == nil {
		return store.ArraySettings{}, nil
	}
	settings, disks, err := h.ArrayStore.GetArray(ctx)
	if err != nil {
		return store.ArraySettings{}, nil
	}
	return settings, disks
}

// arrayRoleToAPI maps a persisted store.ArrayRole* spelling to its API
// enum value, honestly falling back to unassigned for anything unrecognized
// rather than guessing.
func arrayRoleToAPI(role string) apiv1.PoolDiskEntryRole {
	switch role {
	case store.ArrayRoleParity:
		return apiv1.PoolDiskEntryRoleParity
	case store.ArrayRoleData:
		return apiv1.PoolDiskEntryRoleData
	case store.ArrayRoleCache:
		return apiv1.PoolDiskEntryRoleCache
	default:
		return apiv1.PoolDiskEntryRoleUnassigned
	}
}

// removalStateToAPI mirrors state (a store.RemovalState* constant, or ""
// for a disk not in removal, #359) into PoolDiskEntry's own nullable
// removalState field.
func removalStateToAPI(state string) apiv1.OptNilDiskRemovalState {
	if state == "" {
		return apiv1.OptNilDiskRemovalState{}
	}
	return apiv1.NewOptNilDiskRemovalState(apiv1.DiskRemovalState(state))
}

// finishConfirmationForState is job.EvacuationConfirmation(mountpoint)
// whenever state is set — the same fixed phrase finishDiskRemoval checks
// — so the pool page can retry Finish removal once the disk has left the
// pool (unpooled/unlisted), where planDiskEvacuation itself now refuses
// (#361, finding 1 of this issue's fix round).
func finishConfirmationForState(mountpoint, state string) apiv1.OptString {
	if state == "" {
		return apiv1.OptString{}
	}
	return apiv1.NewOptString(job.EvacuationConfirmation(mountpoint))
}

// populatePoolSpace fills status's pool-free, largest-single-disk-free and
// per-disk free/nearMinFreeSpace figures straight from statfs(2) on each
// present data disk's mountpoint (pool.ComputePoolSpace, doc 09 §5) — never
// a directory walk. It reads mountpoints from status.Disks itself (already
// reconciled by identity, #326) rather than re-deriving them from the
// stored array topology by device path, since a renumbered disk's current
// entry no longer shares its key. A missing data disk's stale mountpoint is
// deliberately excluded: statfs-ing it would fail and, since
// ComputePoolSpace is all-or-nothing, take every other disk's figures down
// with it. It is best-effort throughout: with no data disk to statfs, or
// without an ArrayStore configured, status is returned with these fields
// unset rather than as an error, since GetPool must still report disk
// inventory before create-array has run.
func (h *Handler) populatePoolSpace(ctx context.Context, status *apiv1.PoolStatus, settings store.ArraySettings) {
	if h.ArrayStore == nil {
		return
	}
	var dataMounts []string
	for _, e := range status.Disks {
		if e.Role != apiv1.PoolDiskEntryRoleData || e.State != apiv1.DiskStateActive || e.MountPoint == "" {
			continue
		}
		dataMounts = append(dataMounts, e.MountPoint)
	}
	if len(dataMounts) == 0 {
		return
	}
	space, err := pool.ComputePoolSpace(ctx, pool.StatfsSpaceStatter{}, dataMounts, settings.MinFreeSpace)
	if err != nil {
		return
	}
	status.PoolFreeBytes = apiv1.NewOptNilInt64(space.PoolFreeBytes)
	status.LargestDiskFreeBytes = apiv1.NewOptNilInt64(space.LargestDiskFreeBytes)
	status.LargestDiskPath = apiv1.NewOptNilString(space.LargestDiskPath)

	byPath := make(map[string]pool.DiskSpace, len(space.Disks))
	for _, d := range space.Disks {
		byPath[d.Path] = d
	}
	for i := range status.Disks {
		if status.Disks[i].Role != apiv1.PoolDiskEntryRoleData || status.Disks[i].State != apiv1.DiskStateActive || status.Disks[i].MountPoint == "" {
			continue
		}
		d, ok := byPath[status.Disks[i].MountPoint]
		if !ok {
			continue
		}
		status.Disks[i].FreeBytes = apiv1.NewOptNilInt64(d.FreeBytes)
		status.Disks[i].NearMinFreeSpace = apiv1.NewOptBool(d.NearMinFreeSpace)
	}
}

func diskToAPI(d disk.Disk) apiv1.DiskInventoryEntry {
	entry := apiv1.DiskInventoryEntry{
		Device:          d.Device,
		SizeBytes:       d.Size,
		Boot:            d.Boot,
		Failed:          apiv1.NewOptBool(d.Failed),
		WeakIdentity:    apiv1.NewOptBool(d.WeakIdentity),
		ContainsData:    apiv1.NewOptBool(d.ContainsData),
		LooksLikeUnraid: apiv1.NewOptBool(d.LooksLikeUnraid),
	}
	if d.Model != "" {
		entry.Model = apiv1.NewOptString(d.Model)
	}
	if d.Serial != "" {
		entry.Serial = apiv1.NewOptString(d.Serial)
	}
	if d.WWN != "" {
		entry.Wwn = apiv1.NewOptString(d.WWN)
	}
	if d.Filesystem != "" {
		entry.Filesystem = apiv1.NewOptString(d.Filesystem)
	}
	if d.Label != "" {
		entry.Label = apiv1.NewOptString(d.Label)
	}
	return entry
}

func (h *Handler) ListDisks(ctx context.Context) (*apiv1.ListDisksOK, error) {
	if h.Disks == nil {
		return &apiv1.ListDisksOK{Disks: []apiv1.DiskInventoryEntry{}}, nil
	}
	disks, err := h.Disks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing disks: %w", err)
	}
	out := make([]apiv1.DiskInventoryEntry, 0, len(disks))
	for _, d := range disks {
		entry := diskToAPI(d)
		report, err := h.Disks.SMART(ctx, d.Device, disk.SMARTPollRespectStandby)
		if err == nil {
			entry.SmartStatus = apiv1.NewOptString(smartStatusString(report))
		}
		out = append(out, entry)
	}
	return &apiv1.ListDisksOK{Disks: out}, nil
}

func smartStatusString(r disk.SMARTReport) string {
	if r.Skipped {
		return "standby"
	}
	if r.SelfTestFailed || r.PendingSectors > 0 || r.OfflineUncorrectable > 0 || r.ReallocatedSectors > 0 {
		return "failing"
	}
	return "ok"
}

func (h *Handler) StartSync(ctx context.Context, req *apiv1.StartSyncRequest) (*apiv1.Job, error) {
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	params, err := json.Marshal(job.SyncParams{
		DryRun:  req.DryRun.Or(false),
		Confirm: req.Confirm.Or(false),
	})
	if err != nil {
		return nil, fmt.Errorf("encoding sync params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeSync, nil, params)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func (h *Handler) StartScrub(ctx context.Context, req *apiv1.StartScrubRequest) (*apiv1.Job, error) {
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	p := job.ScrubParams{}
	if v, ok := req.Percent.Get(); ok {
		pct := int(v)
		p.Percent = &pct
	}
	params, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encoding scrub params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeScrub, nil, params)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func (h *Handler) StartFix(ctx context.Context, req *apiv1.StartFixRequest) (*apiv1.Job, error) {
	if !req.Confirm {
		return nil, errConfirmRequired
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	p := job.FixParams{Confirm: true}
	if v, ok := req.Disk.Get(); ok {
		d := int(v)
		p.Disk = &d
	}
	params, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encoding fix params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeFix, nil, params)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

// StartMover is `hoserva mover run`'s manual trigger (doc 09 §2) — it
// submits the same TypeMover job the threshold poll and the nightly chain
// do (job.RunMover, registered once in cmd/hoservad), so there is no
// second mover-invocation path.
func (h *Handler) StartMover(ctx context.Context) (*apiv1.Job, error) {
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeMover, nil, nil)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func (h *Handler) ExportConfig(ctx context.Context) (apiv1.ExportConfigOK, error) {
	if h.Backup == nil {
		return apiv1.ExportConfigOK{}, &apiError{code: "not_configured", statusCode: 501, message: "config export is not configured on this daemon"}
	}
	staging, err := os.MkdirTemp("", "hoserva-export-staging-*")
	if err != nil {
		return apiv1.ExportConfigOK{}, err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	now := time.Now().UTC()
	if h.Backup.Now != nil {
		now = h.Backup.Now().UTC()
	}
	if _, err := backup.BuildArchive(ctx, h.Backup.DB, h.Backup.Paths, h.Backup.Secrets, h.Backup.Cipher,
		h.Backup.Hostname, h.Backup.Version, now, staging); err != nil {
		return apiv1.ExportConfigOK{}, err
	}

	tmpFile, err := os.CreateTemp("", "hoserva-config-*.tar.zst")
	if err != nil {
		return apiv1.ExportConfigOK{}, err
	}
	archivePath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(archivePath)
		return apiv1.ExportConfigOK{}, err
	}
	if err := packTarZst(staging, archivePath); err != nil {
		_ = os.Remove(archivePath)
		return apiv1.ExportConfigOK{}, err
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return apiv1.ExportConfigOK{}, err
	}
	return apiv1.ExportConfigOK{Data: &exportReadCloser{f: f, path: archivePath}}, nil
}

type exportReadCloser struct {
	f    *os.File
	path string
}

func (e *exportReadCloser) Read(p []byte) (int, error) { return e.f.Read(p) }
func (e *exportReadCloser) Close() error {
	err := e.f.Close()
	_ = os.Remove(e.path)
	return err
}

const maxConfigArchiveBytes = 512 << 20

// errImportJobInProgress refuses an import while a job is running or
// queued (doc 01 §4): restoring the database would rewrite the jobs
// table and the relocation/mover state underneath it.
var errImportJobInProgress = &apiError{code: "job_in_progress", statusCode: 409, message: "a job is running or queued — import would rewrite the jobs table underneath it"}

// ImportConfig restores the database step of doc 10 §1's in-place
// restore (#269): checksum and integrity verification, then a schema-
// version match, then a pre-import safety backup, then
// backup.RestoreDatabase — SQLite's own online backup API, never a file-
// level copy or rename over the live database's path (see
// RestoreDatabase's own doc comment for why that corrupted it). Restoring
// generated config files, stacks, templates and secrets.age is #62.
func (h *Handler) ImportConfig(ctx context.Context, req *apiv1.ImportConfigReq) error {
	if !req.Confirm {
		return errConfirmRequired
	}
	if h.Backup == nil || h.Backup.DB == nil || h.Scheduler == nil {
		return &apiError{code: "not_configured", statusCode: 501, message: "config import is not configured on this daemon"}
	}

	tmp, err := os.CreateTemp("", "hoserva-config-import-*.tar.zst")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := io.Copy(tmp, io.LimitReader(req.Archive.File, maxConfigArchiveBytes+1)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("reading archive: %w", err)
	}
	st, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if st.Size() > maxConfigArchiveBytes {
		_ = tmp.Close()
		return &apiError{code: "archive_too_large", statusCode: 413, message: "config archive exceeds 512 MiB"}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Structural verification only (checksums, PRAGMA integrity_check):
	// secrets.age, if present, is neither required nor decrypted here —
	// this restores the database only (#62 restores the rest).
	if err := backup.VerifyArchiveForImport(tmpPath); err != nil {
		return &apiError{code: "invalid_archive", statusCode: 400, message: err.Error()}
	}
	staging, err := os.MkdirTemp("", "hoserva-import-staging-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := unpackTarZst(tmpPath, staging); err != nil {
		return &apiError{code: "invalid_archive", statusCode: 400, message: err.Error()}
	}
	stateDB := filepath.Join(staging, "state.db")
	if _, err := os.Stat(stateDB); err != nil {
		return &apiError{code: "invalid_archive", statusCode: 400, message: "archive is missing state.db"}
	}

	archiveVersion, err := readSchemaVersion(ctx, "file:"+stateDB+"?mode=ro")
	if err != nil {
		return &apiError{code: "invalid_archive", statusCode: 400, message: fmt.Sprintf("reading archive schema version: %v", err)}
	}
	liveVersion, err := (&store.Runner{DB: h.Backup.DB}).CurrentVersion(ctx)
	if err != nil {
		return fmt.Errorf("reading live database schema version: %w", err)
	}
	if archiveVersion != liveVersion {
		return &apiError{
			code:       "incompatible_archive",
			statusCode: 400,
			message:    fmt.Sprintf("archive schema version %s does not match the running database's %s", archiveVersion, liveVersion),
		}
	}

	// The archive's own queued/running job ids, read from the staged copy
	// before anything overwrites it (#402): the only ids the post-restore
	// interrupt step below is ever allowed to touch. A job inserted into
	// the live database around the restore, but not part of the archive,
	// must never be relabelled just because its own status happens to be
	// queued or running.
	restoredActiveIDs, err := readActiveJobIDs(ctx, "file:"+stateDB+"?mode=ro")
	if err != nil {
		return fmt.Errorf("reading the archive's active job ids: %w", err)
	}

	// The same config backup the pre-update chain runs (doc 10 §1) — if
	// it fails, the import is refused and the live database is untouched.
	if err := h.Backup.Run(ctx); err != nil {
		return fmt.Errorf("backing up before import: %w", err)
	}

	// Acquired here, immediately before the restore, rather than at the
	// top of this method: a job submitted during the upload, the two
	// verification passes or the backup above must still be refused, not
	// silently orphaned by RestoreDatabase overwriting the jobs table
	// underneath it. BeginDatabaseRestore checks the store under the
	// scheduler's own lock and, from here on, refuses every Submit, Resume
	// and Cancel until release runs — on every path below, including
	// panic. Only the hold's own refusals (another restore already held,
	// or a job active or being aborted) are reported as the ordinary
	// "a job is running" 409 below — a failure while taking the hold
	// (e.g. the store read it does) is returned as an error with its own
	// detail, never misreported as a running job.
	release, err := h.Scheduler.BeginDatabaseRestore(ctx)
	if err != nil {
		if errors.Is(err, job.ErrJobsActiveForRestore) || errors.Is(err, job.ErrDatabaseRestoreInProgress) {
			return errImportJobInProgress
		}
		return fmt.Errorf("beginning database restore: %w", err)
	}
	defer release()

	if err := backup.RestoreDatabase(ctx, h.Backup.DB, stateDB); err != nil {
		return fmt.Errorf("restoring database: %w", err)
	}

	if importPostRestoreHookForTest != nil {
		importPostRestoreHookForTest()
	}

	// The archive's jobs table is restored as-is, including any row that
	// was queued or running when it was exported — nothing on this
	// daemon is actually running it. Mark exactly those rows interrupted
	// the same way RecoverFromRestart does at boot (doc 01 §4), so a
	// stale "running" row does not refuse the next import with
	// job_in_progress forever — never a blanket sweep over whatever the
	// now-restored table holds, which would also catch a job inserted
	// into the live database in the narrow window around this restore
	// but never part of the archive at all (#402).
	if h.Store != nil {
		if err := h.Store.InterruptByID(ctx, restoredActiveIDs, time.Now().UTC()); err != nil {
			return fmt.Errorf("interrupting jobs restored from the archive: %w", err)
		}
	}
	return nil
}

// importPostRestoreHookForTest, when non-nil, is called by ImportConfig
// synchronously right after backup.RestoreDatabase has returned and
// before the targeted post-restore interrupt step runs — so a test can
// land a write to the live database deterministically inside that exact
// window (#402), instead of racing the real clock. Never set outside a
// test.
var importPostRestoreHookForTest func()

// readActiveJobIDs opens dsn read-only and returns the ids of every job
// whose status is queued or running, without mutating the file it points
// at — used against the staged, not-yet-trusted archive copy of state.db,
// so ImportConfig knows exactly which ids its own restore is allowed to
// mark interrupted afterward (#402).
func readActiveJobIDs(ctx context.Context, dsn string) ([]string, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT id FROM jobs WHERE "status" IN ('queued', 'running')`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// readSchemaVersion opens dsn read-only and reads its store bookkeeping
// version, without mutating the file it points at — used against the
// staged, not-yet-trusted archive copy of state.db.
func readSchemaVersion(ctx context.Context, dsn string) (string, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	return (&store.Runner{DB: db}).CurrentVersion(ctx)
}

func packTarZst(dir, dest string) error {
	out, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := out.Name()
	if err := out.Chmod(0o600); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	zw, err := zstd.NewWriter(out)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	tw := tar.NewWriter(zw)
	if err := addDirToTar(tw, dir, ""); err != nil {
		_ = tw.Close()
		_ = zw.Close()
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func addDirToTar(tw *tar.Writer, root, prefix string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(filepath.Join(prefix, rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		_ = f.Close()
		return err
	})
}

func unpackTarZst(archivePath, dest string) error {
	in, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	zr, err := zstd.NewReader(in)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	remaining := int64(maxConfigArchiveBytes)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(dest) {
			return fmt.Errorf("archive entry escapes destination")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			n, copyErr := io.CopyN(out, tr, remaining+1)
			closeErr := out.Close()
			if n > remaining {
				return fmt.Errorf("archive exceeds extraction limit")
			}
			remaining -= n
			if copyErr != nil && copyErr != io.EOF {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	return nil
}
