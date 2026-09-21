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
	entries := []apiv1.PoolDiskEntry{}
	if h.Disks != nil {
		disks, err := h.Disks.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing disks: %w", err)
		}
		for _, d := range disks {
			if d.Boot {
				continue
			}
			entries = append(entries, apiv1.PoolDiskEntry{
				Device:     d.Device,
				MountPoint: "",
				Role:       apiv1.PoolDiskEntryRoleUnassigned,
				State:      apiv1.DiskStateActive,
				SizeBytes:  apiv1.NewOptNilInt64(d.Size),
			})
		}
	}
	mounted, err := pathIsMountpoint(pool.CatchAllPath)
	if err != nil {
		mounted = false
	}
	status := &apiv1.PoolStatus{Mounted: mounted, Disks: entries}
	h.populatePoolSpace(ctx, status)
	return status, nil
}

// populatePoolSpace fills status's pool-free, largest-single-disk-free and
// per-disk free/nearMinFreeSpace figures straight from statfs(2) on each
// data disk's mountpoint (pool.ComputePoolSpace, doc 09 §5) — never a
// directory walk. It is best-effort: with no array topology yet (no data
// disk mountpoints to statfs), or without an ArrayStore configured, status
// is returned with these fields unset rather than as an error, since
// GetPool must still report disk inventory before create-array has run.
func (h *Handler) populatePoolSpace(ctx context.Context, status *apiv1.PoolStatus) {
	if h.ArrayStore == nil {
		return
	}
	settings, disks, err := h.ArrayStore.GetArray(ctx)
	if err != nil {
		return
	}
	mountByDevice := make(map[string]string, len(disks))
	var dataMounts []string
	for _, d := range disks {
		if d.Role != store.ArrayRoleData {
			continue
		}
		mountByDevice[d.Device] = d.Mountpoint
		dataMounts = append(dataMounts, d.Mountpoint)
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
		mnt, ok := mountByDevice[status.Disks[i].Device]
		if !ok {
			continue
		}
		d, ok := byPath[mnt]
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
