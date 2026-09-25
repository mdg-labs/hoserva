package api

import (
	"archive/tar"
	"context"
	"encoding/json"
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

func (h *Handler) ImportConfig(ctx context.Context, req *apiv1.ImportConfigReq) error {
	if !req.Confirm {
		return errConfirmRequired
	}
	if h.Backup == nil {
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
	if err := backup.VerifyArchive(tmpPath, ""); err != nil {
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
	dest := h.Backup.Paths.DBPath
	if dest == "" {
		return &apiError{code: "not_configured", statusCode: 501, message: "no database path configured for import"}
	}
	if err := copyFileAtomic(stateDB, dest); err != nil {
		return fmt.Errorf("restoring database: %w", err)
	}
	return nil
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

func copyFileAtomic(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dest + ".importing"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
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
