package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
)

// mockAppdataInstallation is the installation id the mock's own appdata
// archive names carry. An archive named for any other id is refused like
// production refuses another installation's.
const mockAppdataInstallation = "0a1b2c3d4e5f"

// mockAppdataArchives are the archives this mock instance reports on the
// pool destination: one weekly backup of jellyfin and the snapshot a
// restore took.
func mockAppdataArchives() []backup.AppdataArchive {
	at := time.Date(2026, 9, 27, 4, 0, 0, 0, time.UTC)
	return []backup.AppdataArchive{
		{
			Name:      "hoserva-appdata-" + mockAppdataInstallation + "-jellyfin-" + at.Add(-3*24*time.Hour).Format("2006-01-02T15-04-05") + ".tar.zst",
			Container: "jellyfin", DestinationID: backup.DefaultPoolID, DestinationName: "Pool",
			ModTime: at.Add(-3 * 24 * time.Hour), Size: 812 * 1024 * 1024,
		},
		{
			Name:      "hoserva-appdata-" + mockAppdataInstallation + "-jellyfin-" + at.Add(-2*24*time.Hour).Format("2006-01-02T15-04-05") + ".pre-restore.tar.zst",
			Container: "jellyfin", DestinationID: backup.DefaultPoolID, DestinationName: "Pool",
			ModTime: at.Add(-2 * 24 * time.Hour), Size: 790 * 1024 * 1024, Reason: backup.ReasonPreRestore,
		},
	}
}

func mapMockAppdataError(err error) error {
	switch {
	case errors.Is(err, backup.ErrAppdataContainerNotFound):
		return &mockError{code: "container_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, backup.ErrAppdataArchiveInvalid):
		return &mockError{code: "appdata_archive_invalid", statusCode: 400, message: err.Error()}
	default:
		return err
	}
}

// appdataScope lists the containers with a bind-mounted directory strictly
// inside the appdata location, the way backup.AppdataService.Scope does,
// with each one's stored policy. The caller holds appsMu and appdataMu.
func (h *handler) appdataScope() []backup.AppdataContainer {
	var out []backup.AppdataContainer
	for _, app := range h.apps {
		var dirs []string
		for _, m := range app.Mounts {
			src := m.Source.Or("")
			if strings.HasPrefix(src, mockAppdataRoot) && len(src) > len(mockAppdataRoot) {
				dirs = append(dirs, src)
			}
		}
		if len(dirs) == 0 {
			continue
		}
		c := backup.AppdataContainer{
			Name:          app.Name,
			Image:         app.Image,
			Running:       app.State == apiv1.AppStateRunning || app.State == apiv1.AppStateRestarting || app.State == apiv1.AppStatePaused,
			Stop:          true,
			Included:      true,
			DatabaseImage: backup.IsDatabaseImage(app.Image),
			Dirs:          dirs,
		}
		if p, ok := h.appdataPolicies[app.Name]; ok {
			c.Stop, c.Included = p.Stop, p.Included
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (h *handler) mockAppdataScope() []backup.AppdataContainer {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	h.appdataMu.Lock()
	defer h.appdataMu.Unlock()
	return h.appdataScope()
}

func mockAppdataContainerToAPI(c backup.AppdataContainer) apiv1.AppdataBackupContainer {
	out := apiv1.AppdataBackupContainer{
		Name: c.Name, Image: c.Image, Running: c.Running, Stop: c.Stop, Included: c.Included, DatabaseImage: c.DatabaseImage,
	}
	if w := c.Warning(); w != "" {
		out.Warning = apiv1.NewOptNilString(w)
	}
	return out
}

func (h *handler) GetAppdataBackup(ctx context.Context) (*apiv1.AppdataBackupConfig, error) {
	scope := h.mockAppdataScope()
	out := &apiv1.AppdataBackupConfig{Containers: make([]apiv1.AppdataBackupContainer, 0, len(scope))}
	for _, c := range scope {
		out.Containers = append(out.Containers, mockAppdataContainerToAPI(c))
	}
	return out, nil
}

func (h *handler) SetAppdataBackupContainer(ctx context.Context, req *apiv1.SetAppdataBackupContainerRequest, params apiv1.SetAppdataBackupContainerParams) (*apiv1.AppdataBackupContainer, error) {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	h.appdataMu.Lock()
	defer h.appdataMu.Unlock()
	for _, c := range h.appdataScope() {
		if c.Name != params.Name {
			continue
		}
		h.appdataPolicies[c.Name] = backup.AppdataPolicy{Container: c.Name, Stop: req.Stop, Included: req.Included}
		c.Stop, c.Included = req.Stop, req.Included
		out := mockAppdataContainerToAPI(c)
		return &out, nil
	}
	return nil, mapMockAppdataError(fmt.Errorf("%w: %s", backup.ErrAppdataContainerNotFound, params.Name))
}

func (h *handler) queueMockJob(typ apiv1.JobType) (*apiv1.Job, error) {
	return h.queueMockJobIn(typ, apiv1.JobClassService)
}

// queueMockJobIn queues a job of typ in class, the same way for every job type
// this mock queues.
func (h *handler) queueMockJobIn(typ apiv1.JobType, class apiv1.JobClass) (*apiv1.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maintenance {
		return nil, errMaintenanceMode()
	}
	if class != apiv1.JobClassService && class != apiv1.JobClassVM && typ != apiv1.JobTypeMigrationImport && typ != apiv1.JobTypeMigrationVerify && typ != apiv1.JobTypeMigrationParity && h.migration.unfinished() {
		return nil, errMigrationInProgress()
	}
	j := apiv1.Job{
		ID:        uuid.New(),
		Type:      typ,
		Class:     class,
		Status:    apiv1.JobStatusQueued,
		CreatedAt: time.Now().UTC(),
	}
	h.jobs[j.ID] = j
	return &j, nil
}

// StartAppdataBackup mirrors production's order: the array must be
// running, every named container must be in scope, then the job is queued
// like every other mock job submission (this mock has no scheduler).
func (h *handler) StartAppdataBackup(ctx context.Context, req apiv1.OptStartAppdataBackupRequest) (*apiv1.Job, error) {
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	inScope := map[string]bool{}
	for _, c := range h.mockAppdataScope() {
		inScope[c.Name] = true
	}
	for _, name := range req.Value.Containers {
		if !inScope[name] {
			return nil, mapMockAppdataError(fmt.Errorf("%w: %s", backup.ErrAppdataContainerNotFound, name))
		}
	}
	return h.queueMockJob(apiv1.JobTypeAppdataBackup)
}

func (h *handler) ListAppdataArchives(ctx context.Context, params apiv1.ListAppdataArchivesParams) (*apiv1.ListAppdataArchivesOK, error) {
	container := params.Container.Or("")
	h.backupMu.Lock()
	known := map[string]bool{}
	for _, d := range h.backupDestinations {
		known[d.ID] = true
	}
	h.backupMu.Unlock()

	out := &apiv1.ListAppdataArchivesOK{Archives: []apiv1.AppdataArchive{}, Unavailable: []apiv1.ListAppdataArchivesOKUnavailableItem{}}
	for _, a := range mockAppdataArchives() {
		if !known[a.DestinationID] || container != "" && a.Container != container {
			continue
		}
		item := apiv1.AppdataArchive{
			Name: a.Name, Container: a.Container, DestinationId: a.DestinationID, DestinationName: a.DestinationName,
			CreatedAt: a.ModTime, Size: a.Size, Encrypted: a.Encrypted,
		}
		if a.Reason != backup.ReasonNone {
			item.Reason = apiv1.NewOptNilString(string(a.Reason))
		}
		out.Archives = append(out.Archives, item)
	}
	return out, nil
}

// RestoreAppdata runs the checks production's handler runs before it
// queues the job, in the same order: confirmation, a running array, the
// archive's name (backup.CheckAppdataArchiveName), the destination, and
// the archive being on it.
func (h *handler) RestoreAppdata(ctx context.Context, req *apiv1.RestoreAppdataRequest) (*apiv1.Job, error) {
	if !req.Confirm {
		return nil, &mockError{code: "confirmation_required", statusCode: 400, message: "restoring overwrites the container's appdata: send confirm: true"}
	}
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	if err := backup.CheckAppdataArchiveName(req.Archive, req.Container, mockAppdataInstallation); err != nil {
		return nil, mapMockAppdataError(err)
	}
	h.backupMu.Lock()
	found := false
	for _, d := range h.backupDestinations {
		found = found || d.ID == req.DestinationId
	}
	h.backupMu.Unlock()
	if !found {
		return nil, &mockError{code: "backup_destination_not_found", statusCode: 404, message: "no backup destination with that id"}
	}
	for _, a := range mockAppdataArchives() {
		if a.Name == req.Archive && a.DestinationID == req.DestinationId {
			return h.queueMockJob(apiv1.JobTypeAppdataRestore)
		}
	}
	return nil, &mockError{code: "archive_not_found", statusCode: 404, message: fmt.Sprintf("%v: %s on destination %q", backup.ErrAppdataArchiveNotFound, req.Archive, req.DestinationId)}
}

// PreviewAppdataRestore runs the checks production's handler runs before it
// queues the preview job, in the same order: a running array, the archive's
// name (backup.CheckAppdataArchiveName), the destination, and the archive
// being on it. The mock has no archive to read and no scheduler, so a valid
// request returns a preview job that has already succeeded, holding a fixed
// comparison of jellyfin's appdata for GetAppdataRestorePreview.
func (h *handler) PreviewAppdataRestore(ctx context.Context, req *apiv1.PreviewAppdataRestoreRequest) (*apiv1.Job, error) {
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	if err := backup.CheckAppdataArchiveName(req.Archive, req.Container, mockAppdataInstallation); err != nil {
		return nil, mapMockAppdataError(err)
	}
	h.backupMu.Lock()
	found := false
	for _, d := range h.backupDestinations {
		found = found || d.ID == req.DestinationId
	}
	h.backupMu.Unlock()
	if !found {
		return nil, &mockError{code: "backup_destination_not_found", statusCode: 404, message: "no backup destination with that id"}
	}
	for _, a := range mockAppdataArchives() {
		if a.Name != req.Archive || a.DestinationID != req.DestinationId {
			continue
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.maintenance {
			return nil, errMaintenanceMode()
		}
		now := time.Now().UTC()
		j := apiv1.Job{
			ID:          uuid.New(),
			Type:        apiv1.JobTypeAppdataRestorePreview,
			Class:       apiv1.JobClassService,
			Status:      apiv1.JobStatusSucceeded,
			Cancellable: true,
			CreatedAt:   now,
			StartedAt:   apiv1.NewOptNilDateTime(now),
			FinishedAt:  apiv1.NewOptNilDateTime(now),
		}
		h.jobs[j.ID] = j
		h.appdataMu.Lock()
		defer h.appdataMu.Unlock()
		if h.appdataPreviews == nil {
			h.appdataPreviews = map[uuid.UUID]apiv1.AppdataRestorePreview{}
		}
		h.appdataPreviews[j.ID] = apiv1.AppdataRestorePreview{
			Container: req.Container, Archive: req.Archive, DestinationId: req.DestinationId, CreatedAt: a.ModTime,
			Directories: []apiv1.AppdataRestorePreviewDirectory{{
				Directory: "jellyfin",
				Replaced:  apiv1.AppdataRestorePreviewGroup{Files: 2, Bytes: 48 * 1024 * 1024, Sample: []string{"data/library.db", "config/system.xml"}},
				Added:     apiv1.AppdataRestorePreviewGroup{Files: 1, Bytes: 4096, Sample: []string{"config/encoding.xml"}},
				Removed:   apiv1.AppdataRestorePreviewGroup{Files: 1, Bytes: 12 * 1024, Sample: []string{"log/log_20260929.log"}},
			}},
		}
		return &j, nil
	}
	return nil, &mockError{code: "archive_not_found", statusCode: 404, message: fmt.Sprintf("%v: %s on destination %q", backup.ErrAppdataArchiveNotFound, req.Archive, req.DestinationId)}
}

// GetAppdataRestorePreview answers job_not_found for an id that is no
// restore preview job, like production; every preview job the mock holds
// has already succeeded.
func (h *handler) GetAppdataRestorePreview(ctx context.Context, params apiv1.GetAppdataRestorePreviewParams) (*apiv1.AppdataRestorePreview, error) {
	h.mu.Lock()
	j, ok := h.jobs[params.JobId]
	h.mu.Unlock()
	if !ok || j.Type != apiv1.JobTypeAppdataRestorePreview {
		return nil, errJobNotFound(params.JobId)
	}
	h.appdataMu.Lock()
	defer h.appdataMu.Unlock()
	p, ok := h.appdataPreviews[params.JobId]
	if !ok {
		return nil, &mockError{code: "appdata_preview_gone", statusCode: 404, message: "the result of this restore preview is no longer held: preview again"}
	}
	return &p, nil
}
