package share

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

const applyCommand = "share"

// ShareDirMode is the mode every share's top-level branch directory
// gets: rwxrwsr-x, setgid so files and directories created under it
// inherit the shared group (Q26). os.FileMode's setuid/setgid/sticky
// bits are not the raw unix 0o2000/0o4000/0o1000 bits (os.Chmod's
// syscallMode remaps os.ModeSetgid onto the real S_ISGID bit), so this
// is built from os.ModeSetgid rather than a literal 0o2775.
var ShareDirMode = os.FileMode(0o775) | os.ModeSetgid

const (
	// ShareGID is the numeric GID of the shared data group `users`
	// (Q26) — GID 100 on Debian's own base-passwd group table, the same
	// value Unraid uses, so migrated data needs no ownership rewrite.
	ShareGID = 100
)

// Mounter brings a share's mergerfs mount up or down. pool.Mounter
// satisfies it; tests inject a recorder that never touches /mnt.
type Mounter interface {
	Mount(ctx context.Context, mnt pool.Mount) error
	Unmount(ctx context.Context, where string) error
}

// SMB is a share's Samba options (doc 03 §4.2, Q73).
type SMB struct {
	Enabled            bool
	Guest              bool
	ReadOnly           bool
	Browseable         bool
	Recycle            bool
	TimeMachine        bool
	TimeMachineMaxSize string
}

// NFS is a share's NFS export options (doc 03 §4.2).
type NFS struct {
	Enabled bool
	Hosts   []string
	Squash  string
}

// Share is the domain record API handlers map to.
type Share struct {
	Name         string
	CacheMode    pool.CacheMode
	CreatePolicy pool.CreatePolicy
	SMB          SMB
	NFS          NFS
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Path is the share's user-facing mount (D10).
func (s Share) Path() string {
	return pool.SharePath(s.Name)
}

// CreateInput is createShare's payload. Empty cache mode / create policy
// / SMB / NFS take the documented defaults (Q11, Q12).
type CreateInput struct {
	Name         string
	CacheMode    pool.CacheMode
	CreatePolicy pool.CreatePolicy
	SMB          *SMB
	NFS          *NFS
}

// UpdateInput is updateShare's payload. Nil pointers keep the stored
// value.
type UpdateInput struct {
	CacheMode    *pool.CacheMode
	CreatePolicy *pool.CreatePolicy
	SMB          *SMB
	NFS          *NFS
}

// BrowseEntry is one listing row (doc 03 §4.2).
type BrowseEntry struct {
	Name      string
	Directory bool
	SizeBytes int64
	Disk      string
}

// Service is internal/share's exported entry point. Handlers translate
// generated types to this; they do not mkdir, generate smb.conf, or
// decide which paths delete-data may touch.
type Service struct {
	Shares  *store.ShareStore
	Array   *store.ArrayStore
	Gen     *config.Generator
	FS      FS
	Mounter Mounter
	Now     func() time.Time
	// CatchAll is the directory browse lists under. Empty uses
	// pool.CatchAllPath. Tests point it at a temp dir so listing never
	// walks the host's /mnt/user.
	CatchAll string
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) catchAll() string {
	if s.CatchAll != "" {
		return s.CatchAll
	}
	return pool.CatchAllPath
}

// List returns every share, sorted by name.
func (s *Service) List(ctx context.Context) ([]Share, error) {
	rows, err := s.Shares.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Share, 0, len(rows))
	for _, row := range rows {
		out = append(out, shareFromStore(row))
	}
	return out, nil
}

// Get returns one share by name.
func (s *Service) Get(ctx context.Context, name string) (Share, error) {
	if err := validateName(name); err != nil {
		return Share{}, err
	}
	row, err := s.Shares.Get(ctx, name)
	if err != nil {
		return Share{}, err
	}
	return shareFromStore(row), nil
}

// Create persists the share, mkdirs its branch directories, writes the
// per-share mergerfs units through the existing renderer, mounts them,
// and regenerates smb.conf and /etc/exports.
func (s *Service) Create(ctx context.Context, in CreateInput) (Share, error) {
	if err := validateName(in.Name); err != nil {
		return Share{}, err
	}
	mode := in.CacheMode
	if mode == "" {
		mode = pool.CacheThenMove
	}
	if err := validateCacheMode(mode); err != nil {
		return Share{}, err
	}
	policy := in.CreatePolicy
	if policy == "" {
		policy = pool.DefaultCreatePolicy
	}
	if err := validateCreatePolicy(policy); err != nil {
		return Share{}, err
	}
	smb := defaultSMB()
	if in.SMB != nil {
		smb = *in.SMB
	}
	if !smb.TimeMachine {
		smb.TimeMachineMaxSize = ""
	}
	if err := validateSMB(smb); err != nil {
		return Share{}, err
	}
	nfs := defaultNFS()
	if in.NFS != nil {
		nfs = normalizeNFS(*in.NFS)
	}
	if err := validateNFS(nfs); err != nil {
		return Share{}, err
	}

	_, disks, err := s.array(ctx)
	if err != nil {
		return Share{}, err
	}
	data, cache, _ := splitDisks(disks)
	if (mode == pool.CacheThenMove || mode == pool.CacheOnly) && cache == "" {
		return Share{}, fmt.Errorf("%w: cache mode %q needs a cache disk", ErrInvalidInput, mode)
	}

	roots, err := shareDataRoots(in.Name, mode, data, cache)
	if err != nil {
		return Share{}, err
	}
	for _, dir := range roots {
		if err := s.FS.MkdirAll(dir, 0o755); err != nil {
			return Share{}, fmt.Errorf("share: creating branch directory %s: %w", dir, err)
		}
		if err := s.ensureShareDirOwnership(dir); err != nil {
			return Share{}, err
		}
	}

	now := s.now().UTC()
	rec := toStore(Share{
		Name:         in.Name,
		CacheMode:    mode,
		CreatePolicy: policy,
		SMB:          smb,
		NFS:          nfs,
		CreatedAt:    now,
		UpdatedAt:    now,
	})
	if err := s.Shares.Insert(ctx, rec); err != nil {
		return Share{}, err
	}

	created := shareFromStore(rec)
	if err := s.apply(ctx, created, true, ""); err != nil {
		if rbErr := s.rollbackCreate(ctx, in.Name); rbErr != nil {
			return Share{}, fmt.Errorf("%w (rolling back create: %v)", applyCause(err), rbErr)
		}
		if rbErr := s.rollbackFiles(ctx, err); rbErr != nil {
			return Share{}, fmt.Errorf("%w (rolling back create: %v)", applyCause(err), rbErr)
		}
		return Share{}, applyCause(err)
	}
	return created, nil
}

// Update changes cache mode, create policy, SMB and NFS options, then
// regenerates mounts, smb.conf and exports. Existing files are not relocated.
func (s *Service) Update(ctx context.Context, name string, in UpdateInput) (Share, error) {
	existing, err := s.Get(ctx, name)
	if err != nil {
		return Share{}, err
	}
	prev := existing
	if in.CacheMode != nil {
		if err := validateCacheMode(*in.CacheMode); err != nil {
			return Share{}, err
		}
		existing.CacheMode = *in.CacheMode
	}
	if in.CreatePolicy != nil {
		if err := validateCreatePolicy(*in.CreatePolicy); err != nil {
			return Share{}, err
		}
		existing.CreatePolicy = *in.CreatePolicy
	}
	if in.SMB != nil {
		smb := *in.SMB
		if !smb.TimeMachine {
			smb.TimeMachineMaxSize = ""
		}
		if err := validateSMB(smb); err != nil {
			return Share{}, err
		}
		existing.SMB = smb
	}
	if in.NFS != nil {
		nfs := normalizeNFS(*in.NFS)
		if err := validateNFS(nfs); err != nil {
			return Share{}, err
		}
		existing.NFS = nfs
	}

	_, disks, err := s.array(ctx)
	if err != nil {
		return Share{}, err
	}
	data, cache, _ := splitDisks(disks)
	if (existing.CacheMode == pool.CacheThenMove || existing.CacheMode == pool.CacheOnly) && cache == "" {
		return Share{}, fmt.Errorf("%w: cache mode %q needs a cache disk", ErrInvalidInput, existing.CacheMode)
	}
	roots, err := shareDataRoots(existing.Name, existing.CacheMode, data, cache)
	if err != nil {
		return Share{}, err
	}
	for _, dir := range roots {
		if err := s.FS.MkdirAll(dir, 0o755); err != nil {
			return Share{}, fmt.Errorf("share: creating branch directory %s: %w", dir, err)
		}
		if err := s.ensureShareDirOwnership(dir); err != nil {
			return Share{}, err
		}
	}

	existing.UpdatedAt = s.now().UTC()
	if err := s.Shares.Update(ctx, toStore(existing)); err != nil {
		return Share{}, err
	}
	if err := s.apply(ctx, existing, true, prev.CacheMode); err != nil {
		if rbErr := s.Shares.Update(ctx, toStore(prev)); rbErr != nil {
			return Share{}, fmt.Errorf("%w (restoring previous share: %v)", applyCause(err), rbErr)
		}
		if rbErr := s.rollbackFiles(ctx, err); rbErr != nil {
			return Share{}, fmt.Errorf("%w (restoring previous share: %v)", applyCause(err), rbErr)
		}
		if rbErr := s.restoreLiveMounts(ctx, prev); rbErr != nil {
			return Share{}, fmt.Errorf("%w (restoring previous share: %v)", applyCause(err), rbErr)
		}
		return Share{}, applyCause(err)
	}
	return existing, nil
}

// ensureShareDirOwnership brings a share's top-level branch directory to
// the setgid mode and shared group Q26 requires. It touches only dir
// itself, never recursively, so pre-existing file ownership under an
// adopted or migrated share directory is never rewritten (AC3).
func (s *Service) ensureShareDirOwnership(dir string) error {
	if err := s.FS.Chmod(dir, ShareDirMode); err != nil {
		return fmt.Errorf("share: setting mode on %s: %w", dir, err)
	}
	if err := s.FS.Chown(dir, -1, ShareGID); err != nil {
		return fmt.Errorf("share: setting group on %s: %w", dir, err)
	}
	return nil
}

// Delete removes the share definition and regenerates mounts, smb.conf
// and /etc/exports. It does not delete files. confirm must be true.
func (s *Service) Delete(ctx context.Context, name string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("%w: delete definition requires confirm=true", ErrConfirmation)
	}
	existing, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	state, _, _, err := s.shareFiles(ctx)
	if err != nil {
		return err
	}
	if err := s.Gen.CanWriteShareFiles(ctx, state); err != nil {
		return err
	}
	if err := s.unmountShare(ctx, existing); err != nil {
		return err
	}
	if err := s.Shares.Delete(ctx, name); err != nil {
		if rbErr := s.restoreLiveMounts(ctx, existing); rbErr != nil {
			return fmt.Errorf("%w (remounting share: %v)", err, rbErr)
		}
		return err
	}
	if err := s.apply(ctx, Share{}, false, ""); err != nil {
		if insErr := s.Shares.Insert(ctx, toStore(existing)); insErr != nil {
			return fmt.Errorf("%w (restoring deleted share: %v)", applyCause(err), insErr)
		}
		if rbErr := s.rollbackFiles(ctx, err); rbErr != nil {
			return fmt.Errorf("%w (restoring deleted share: %v)", applyCause(err), rbErr)
		}
		if rbErr := s.restoreLiveMounts(ctx, existing); rbErr != nil {
			return fmt.Errorf("%w (restoring deleted share: %v)", applyCause(err), rbErr)
		}
		return applyCause(err)
	}
	return nil
}

// DeleteData deletes this share's files on the branches that hold it.
// confirmation must equal the share name. The definition is left in
// place. Paths that are not this share's computed roots are refused
// before anything is removed.
func (s *Service) DeleteData(ctx context.Context, name, confirmation string) error {
	if confirmation != name {
		return fmt.Errorf("%w: type the share name %q", ErrConfirmation, name)
	}
	existing, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	_, disks, err := s.array(ctx)
	if err != nil {
		return err
	}
	data, cache, _ := splitDisks(disks)
	roots, err := shareDataRoots(existing.Name, existing.CacheMode, data, cache)
	if err != nil {
		return err
	}
	for _, dir := range roots {
		if err := deleteSharePath(s.FS, dir, roots); err != nil {
			return err
		}
	}
	return nil
}

// deleteSharePath removes path only when it is exactly one of allowed
// (after Clean). A wrong-share or `..`-escaped path is refused and
// nothing is deleted.
func deleteSharePath(fs FS, path string, allowed []string) error {
	if !allowedSharePath(path, allowed) {
		return fmt.Errorf("%w: %s", ErrWrongSharePath, path)
	}
	if _, err := fs.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := fs.RemoveAll(path); err != nil {
		return fmt.Errorf("share: deleting %s: %w", path, err)
	}
	return nil
}

// Browse lists one directory of the share. rel is relative to the share
// root; empty is the root. May wake disks (doc 03 §4.2).
func (s *Service) Browse(ctx context.Context, name, rel string) (string, []BrowseEntry, error) {
	if err := validateName(name); err != nil {
		return "", nil, err
	}
	if _, err := s.Shares.Get(ctx, name); err != nil {
		return "", nil, err
	}
	root := filepath.Join(s.catchAll(), name)
	dir, err := confineSharePathOnFS(s.FS, root, rel)
	if err != nil {
		return "", nil, err
	}
	listed := ""
	if rel != "" && rel != "." {
		listed = filepath.Clean(rel)
		if listed == "." {
			listed = ""
		}
	}
	entries, err := s.FS.ReadDir(dir)
	if err != nil {
		return "", nil, fmt.Errorf("share: listing %s: %w", dir, err)
	}
	out := make([]BrowseEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(dir, e.Name())
		disk := ""
		if x, err := s.FS.GetXattr(full, MergerFSBasepath); err == nil {
			disk = string(x)
		}
		item := BrowseEntry{Name: e.Name(), Directory: e.IsDir(), Disk: disk}
		if !e.IsDir() {
			item.SizeBytes = info.Size()
		}
		out = append(out, item)
	}
	return listed, out, nil
}

func (s *Service) unmountShare(ctx context.Context, sh Share) error {
	if s.Mounter == nil {
		return nil
	}
	if err := s.Mounter.Unmount(ctx, pool.SharePath(sh.Name)); err != nil {
		return fmt.Errorf("share: unmounting %s: %w", pool.SharePath(sh.Name), err)
	}
	if sh.CacheMode != pool.CacheOnly {
		return s.unmountMoverTarget(ctx, sh.Name)
	}
	return nil
}

func (s *Service) unmountMoverTarget(ctx context.Context, name string) error {
	if s.Mounter == nil {
		return nil
	}
	if err := s.Mounter.Unmount(ctx, pool.MoverTargetPath(name)); err != nil {
		return fmt.Errorf("share: unmounting mover target %s: %w", name, err)
	}
	return nil
}

func (s *Service) rollbackCreate(ctx context.Context, name string) error {
	if s.Mounter != nil {
		_ = s.Mounter.Unmount(ctx, pool.SharePath(name))
		_ = s.Mounter.Unmount(ctx, pool.MoverTargetPath(name))
	}
	return s.Shares.Delete(ctx, name)
}

// applyWrittenError marks that apply already replaced at least one
// generated file. Callers restore the previous SQLite row and then
// regenerate files from that restored state.
type applyWrittenError struct {
	err error
}

func (e *applyWrittenError) Error() string { return e.err.Error() }
func (e *applyWrittenError) Unwrap() error { return e.err }

func applyCause(err error) error {
	var written *applyWrittenError
	if errors.As(err, &written) {
		return written.err
	}
	return err
}

func (s *Service) rollbackFiles(ctx context.Context, applyErr error) error {
	var written *applyWrittenError
	if !errors.As(applyErr, &written) {
		return nil
	}
	if err := s.restoreGenerated(ctx); err != nil {
		return fmt.Errorf("restoring generated files: %w", err)
	}
	return nil
}

func (s *Service) restoreGenerated(ctx context.Context) error {
	state, smb, nfs, err := s.shareFiles(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.Gen.WritePoolMounts(ctx, state, applyCommand, 1, now); err != nil {
		return err
	}
	if err := s.Gen.WriteSamba(ctx, smb, applyCommand, 1, now); err != nil {
		return err
	}
	return s.Gen.WriteNFS(ctx, nfs, applyCommand, 1, now)
}

func (s *Service) restoreLiveMounts(ctx context.Context, sh Share) error {
	if s.Mounter == nil || sh.Name == "" {
		return nil
	}
	state, _, _, err := s.shareFiles(ctx)
	if err != nil {
		return err
	}
	return s.syncLiveMounts(ctx, sh, sh.CacheMode == pool.CacheOnly, state)
}

func (s *Service) shareFiles(ctx context.Context) (config.PoolState, []config.SambaShare, []config.NFSShare, error) {
	settings, disks, err := s.array(ctx)
	if err != nil {
		return config.PoolState{}, nil, nil, err
	}
	rows, err := s.Shares.List(ctx)
	if err != nil {
		return config.PoolState{}, nil, nil, err
	}
	data, cache, _ := splitDisks(disks)
	state := config.PoolState{
		DataDisks:    data,
		CachePath:    cache,
		CreatePolicy: pool.CreatePolicy(settings.CreatePolicy),
		Options:      pool.Options{MinFreeSpace: settings.MinFreeSpace},
	}
	var smb []config.SambaShare
	var nfs []config.NFSShare
	for _, row := range rows {
		sh := shareFromStore(row)
		state.Shares = append(state.Shares, config.PoolShare{
			Name:         sh.Name,
			CacheMode:    sh.CacheMode,
			CreatePolicy: sh.CreatePolicy,
		})
		if sh.SMB.Enabled {
			smb = append(smb, config.SambaShare{
				Name:               sh.Name,
				Guest:              sh.SMB.Guest,
				ReadOnly:           sh.SMB.ReadOnly,
				Browseable:         sh.SMB.Browseable,
				Recycle:            sh.SMB.Recycle,
				TimeMachine:        sh.SMB.TimeMachine,
				TimeMachineMaxSize: sh.SMB.TimeMachineMaxSize,
			})
		}
		if sh.NFS.Enabled {
			nfs = append(nfs, config.NFSShare{
				Name:   sh.Name,
				Hosts:  append([]string(nil), sh.NFS.Hosts...),
				Squash: sh.NFS.Squash,
			})
		}
	}
	return state, smb, nfs, nil
}

func (s *Service) apply(ctx context.Context, latest Share, mountLatest bool, prevMode pool.CacheMode) error {
	state, smb, nfs, err := s.shareFiles(ctx)
	if err != nil {
		return err
	}
	if err := s.Gen.CanWriteShareFiles(ctx, state); err != nil {
		return err
	}
	now := s.now()
	if err := s.Gen.WritePoolMounts(ctx, state, applyCommand, 1, now); err != nil {
		return &applyWrittenError{err: err}
	}
	if err := s.Gen.WriteSamba(ctx, smb, applyCommand, 1, now); err != nil {
		return &applyWrittenError{err: err}
	}
	if err := s.Gen.WriteNFS(ctx, nfs, applyCommand, 1, now); err != nil {
		return &applyWrittenError{err: err}
	}
	if mountLatest && s.Mounter != nil && latest.Name != "" {
		dropMover := latest.CacheMode == pool.CacheOnly && prevMode != "" && prevMode != pool.CacheOnly
		if err := s.syncLiveMounts(ctx, latest, dropMover, state); err != nil {
			return &applyWrittenError{err: err}
		}
	}
	return nil
}

func (s *Service) syncLiveMounts(ctx context.Context, latest Share, dropMover bool, state config.PoolState) error {
	opts := pool.Options{MinFreeSpace: state.Options.MinFreeSpace}
	mnt, err := pool.ShareMount(pool.Share{
		Name:         latest.Name,
		CacheMode:    latest.CacheMode,
		CreatePolicy: latest.CreatePolicy,
	}, state.DataDisks, state.CachePath, opts)
	if err != nil {
		return err
	}
	if err := s.Mounter.Mount(ctx, mnt); err != nil {
		return err
	}
	if latest.CacheMode != pool.CacheOnly {
		mover, err := pool.MoverTargetMount(pool.Share{
			Name:         latest.Name,
			CacheMode:    latest.CacheMode,
			CreatePolicy: latest.CreatePolicy,
		}, state.DataDisks, opts)
		if err != nil {
			return err
		}
		return s.Mounter.Mount(ctx, mover)
	}
	if dropMover {
		return s.unmountMoverTarget(ctx, latest.Name)
	}
	return nil
}

func (s *Service) array(ctx context.Context) (store.ArraySettings, []store.ArrayDisk, error) {
	settings, disks, err := s.Array.GetArray(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			return store.ArraySettings{}, nil, ErrNoArray
		}
		return store.ArraySettings{}, nil, err
	}
	return settings, disks, nil
}

func splitDisks(disks []store.ArrayDisk) (data []string, cache, parity string) {
	for _, d := range disks {
		switch d.Role {
		case store.ArrayRoleData:
			data = append(data, d.Mountpoint)
		case store.ArrayRoleCache:
			cache = d.Mountpoint
		case store.ArrayRoleParity:
			parity = d.Mountpoint
		}
	}
	return data, cache, parity
}

func shareFromStore(row store.Share) Share {
	return Share{
		Name:         row.Name,
		CacheMode:    pool.CacheMode(row.CacheMode),
		CreatePolicy: pool.CreatePolicy(row.CreatePolicy),
		SMB: SMB{
			Enabled:            row.SMBEnabled,
			Guest:              row.SMBGuest,
			ReadOnly:           row.SMBReadOnly,
			Browseable:         row.SMBBrowseable,
			Recycle:            row.SMBRecycle,
			TimeMachine:        row.SMBTimeMachine,
			TimeMachineMaxSize: row.SMBTimeMachineMaxSize,
		},
		NFS: NFS{
			Enabled: row.NFSEnabled,
			Hosts:   append([]string(nil), row.NFSHosts...),
			Squash:  row.NFSSquash,
		},
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

func toStore(s Share) store.Share {
	return store.Share{
		Name:                  s.Name,
		CacheMode:             string(s.CacheMode),
		CreatePolicy:          string(s.CreatePolicy),
		SMBEnabled:            s.SMB.Enabled,
		SMBGuest:              s.SMB.Guest,
		SMBReadOnly:           s.SMB.ReadOnly,
		SMBBrowseable:         s.SMB.Browseable,
		SMBRecycle:            s.SMB.Recycle,
		SMBTimeMachine:        s.SMB.TimeMachine,
		SMBTimeMachineMaxSize: s.SMB.TimeMachineMaxSize,
		NFSEnabled:            s.NFS.Enabled,
		NFSHosts:              append([]string(nil), s.NFS.Hosts...),
		NFSSquash:             s.NFS.Squash,
		CreatedAt:             s.CreatedAt,
		UpdatedAt:             s.UpdatedAt,
	}
}

func normalizeNFS(nfs NFS) NFS {
	nfs.Hosts = append([]string(nil), nfs.Hosts...)
	if nfs.Squash == "" {
		nfs.Squash = squashRoot
	}
	return nfs
}
