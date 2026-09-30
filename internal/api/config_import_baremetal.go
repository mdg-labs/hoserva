package api

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
)

// bareMetalImport is importConfig on an installation with no array: the
// archive's database staged and upgraded, the mapping of its disks the user
// confirmed, and the mapping as the attached disks give it at the moment it
// was last confirmed. confirmed is what the request carried.
type bareMetalImport struct {
	staged    *backup.BareMetal
	confirmed *backup.DiskMapping
	mapped    []backup.MappedDisk
}

var (
	errDiskMappingRequired      = &apiError{code: "disk_mapping_required", statusCode: 409, message: backup.ErrDiskMappingRequired.Error()}
	errDiskMappingNotApplicable = &apiError{code: "disk_mapping_not_applicable", statusCode: 409, message: "this installation has an array, so an import restores no disks and takes no diskMapping"}
	errNoLongerFresh            = &apiError{code: "disk_mapping_stale", statusCode: 409, message: "an array was configured on this installation after the disk mapping was confirmed; nothing was restored"}
)

// listAttachedDisks is the attached-disk inventory a bare-metal restore
// maps against: disk.Provider.List, which reads udev's cache and never opens
// a device (Q13). A daemon with no disk provider, or none of the hooks the
// restore needs, cannot restore onto a fresh install.
func (h *Handler) listAttachedDisks(ctx context.Context) ([]disk.Disk, error) {
	if h.Disks == nil || h.RegenerateArray == nil {
		return nil, errConfigImportNotConfigured
	}
	attached, err := h.Disks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the attached disks: %w", err)
	}
	return attached, nil
}

// stageBareMetal stages the archive's database and refuses, before anything
// else is written, an archive that is newer than this installation's schema
// (409 archive_newer_version), one that cannot be upgraded (400
// incompatible_archive), a missing disk mapping (409 disk_mapping_required)
// and one that does not match the attached disks (409 disk_mapping_stale).
func (h *Handler) stageBareMetal(ctx context.Context, tree string, mapping *backup.DiskMapping) (*bareMetalImport, error) {
	attached, err := h.listAttachedDisks(ctx)
	if err != nil {
		return nil, err
	}
	staged, err := backup.StageBareMetal(ctx, h.Backup.DB, tree)
	var (
		newer      *backup.NewerArchiveError
		upgrade    *backup.ArchiveUpgradeError
		unreadable *backup.UnreadableArchiveError
	)
	switch {
	case errors.As(err, &newer):
		return nil, &apiError{code: backup.RefusalNewerArchive, statusCode: 409, message: err.Error()}
	case errors.As(err, &upgrade):
		return nil, &apiError{code: backup.RefusalIncompatibleArchive, statusCode: 400, message: err.Error()}
	case errors.As(err, &unreadable):
		return nil, &apiError{code: "invalid_archive", statusCode: 400, message: err.Error()}
	case err != nil:
		return nil, fmt.Errorf("staging the archive's database: %w", err)
	}
	bm := &bareMetalImport{staged: staged, confirmed: mapping}
	if bm.mapped, err = staged.Confirm(attached, bm.confirmed); err != nil {
		staged.Discard()
		return nil, diskMappingRefusal(err)
	}
	return bm, nil
}

// confirmBareMetal maps the disks again, once the scheduler's restore hold is
// taken, and checks the installation still has no array: a disk attached or
// pulled, or an array created, since the user confirmed the mapping would
// otherwise be restored over.
func (h *Handler) confirmBareMetal(ctx context.Context, bm *bareMetalImport) error {
	fresh, err := backup.IsFreshInstall(ctx, h.Backup.DB)
	if err != nil {
		return err
	}
	if !fresh {
		return errNoLongerFresh
	}
	attached, err := h.listAttachedDisks(ctx)
	if err != nil {
		return err
	}
	mapped, err := bm.staged.Confirm(attached, bm.confirmed)
	if err != nil {
		return diskMappingRefusal(err)
	}
	bm.mapped = mapped
	return nil
}

func diskMappingRefusal(err error) error {
	var stale *backup.DiskMappingStaleError
	switch {
	case errors.Is(err, backup.ErrDiskMappingRequired):
		return errDiskMappingRequired
	case errors.As(err, &stale):
		return &apiError{code: "disk_mapping_stale", statusCode: 409, message: err.Error()}
	}
	return fmt.Errorf("checking the disk mapping: %w", err)
}

// diskMappingFromRequest reads the diskMapping field: nil when the request
// carried none, and an *apiError 400 invalid_disk_mapping for one that is not
// a mapping.
func diskMappingFromRequest(m apiv1.OptString) (*backup.DiskMapping, error) {
	doc, ok := m.Get()
	if !ok {
		return nil, nil
	}
	parsed, err := backup.ParseDiskMapping(doc)
	if err != nil {
		return nil, &apiError{code: "invalid_disk_mapping", statusCode: 400, message: err.Error()}
	}
	return &parsed, nil
}

// previewBareMetal is previewConfigImport on an installation with no array.
func (h *Handler) previewBareMetal(ctx context.Context, tree string, passphrase apiv1.OptString) (*apiv1.ConfigImportPreview, error) {
	attached, err := h.listAttachedDisks(ctx)
	if err != nil {
		return nil, err
	}
	secrets, secretsErr := h.resolveSecrets(ctx, tree, passphrase)
	p, bm, err := backup.PreviewBareMetal(ctx, h.Backup.DB, h.Backup.Paths, tree, attached, backup.WithStackEnvs(secrets.StackEnvs()))
	var unreadable *backup.UnreadableArchiveError
	switch {
	case errors.As(err, &unreadable):
		return nil, &apiError{code: "invalid_archive", statusCode: 400, message: err.Error()}
	case err != nil:
		return nil, fmt.Errorf("previewing the import: %w", err)
	case secretsErr != nil:
		return nil, secretsErr
	}
	out := configImportPreviewToAPI(p, secrets)
	if bm != nil {
		out.BareMetal = apiv1.NewOptConfigImportBareMetal(bareMetalPreviewToAPI(bm))
	}
	return out, nil
}

func bareMetalPreviewToAPI(bm *backup.BareMetalPreview) apiv1.ConfigImportBareMetal {
	out := apiv1.ConfigImportBareMetal{
		SchemaUpgrade: bm.SchemaUpgrade,
		Disks:         make([]apiv1.ConfigImportDisk, len(bm.Disks)),
	}
	for i, d := range bm.Disks {
		r := d.Recorded
		e := apiv1.ConfigImportDisk{
			Name:         d.Name(),
			Role:         apiv1.ArrayDiskRole(r.Role),
			RoleIndex:    int64(r.RoleIndex),
			Mountpoint:   r.Mountpoint,
			FsUuid:       r.FSUUID,
			WeakIdentity: r.WeakIdentity,
			State:        apiv1.ConfigImportDiskState(d.State),
		}
		if r.WWN != "" {
			e.Wwn = apiv1.NewOptString(r.WWN)
		}
		if r.Serial != "" {
			e.Serial = apiv1.NewOptString(r.Serial)
		}
		if r.ByIDName != "" {
			e.ByIdName = apiv1.NewOptString(r.ByIDName)
		}
		if r.SizeSet {
			e.SizeBytes = apiv1.NewOptInt64(r.Size)
		}
		if d.Attached != nil {
			e.Device = apiv1.NewOptString(d.Attached.Device)
		}
		out.Disks[i] = e
	}
	m := backup.MatchedMapping(bm.Disks)
	out.DiskMapping = apiv1.ConfigImportDiskMapping{Disks: make([]apiv1.ConfigImportDiskMappingEntry, len(m.Disks))}
	for i, e := range m.Disks {
		out.DiskMapping.Disks[i] = apiv1.ConfigImportDiskMappingEntry{Role: apiv1.ArrayDiskRole(e.Role), RoleIndex: int64(e.RoleIndex), Device: e.Device}
	}
	return out
}
