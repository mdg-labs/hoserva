package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
)

// mockBackupDestinations is a fresh install's two local destinations
// (Q40, doc 10 §4), each just backed up.
func mockBackupDestinations() []backup.Destination {
	now := time.Now().UTC()
	dests := backup.DefaultDestinations()
	for i := range dests {
		dests[i].CreatedAt = now.Add(-24 * time.Hour)
		last := now.Add(-6 * time.Hour)
		dests[i].LastSuccessfulBackupAt = &last
	}
	return dests
}

func mapMockBackupDestinationError(err error) error {
	switch {
	case errors.Is(err, backup.ErrPassphraseRequired):
		return &mockError{code: "backup_passphrase_required", statusCode: 400, message: err.Error()}
	case errors.Is(err, backup.ErrInvalidDestination):
		return &mockError{code: "backup_destination_invalid", statusCode: 400, message: err.Error()}
	case errors.Is(err, backup.ErrDestinationExists):
		return &mockError{code: "backup_destination_exists", statusCode: 409, message: err.Error()}
	default:
		return err
	}
}

func mockBackupDestinationToAPI(d backup.Destination, now time.Time) apiv1.BackupDestination {
	out := apiv1.BackupDestination{
		ID:      d.ID,
		Name:    d.Name,
		Type:    apiv1.BackupDestinationType(d.Type),
		Path:    d.Path,
		Enabled: d.Enabled,
		Encrypt: d.Encrypt,
		Retention: apiv1.BackupRetention{
			Daily:   int32(d.Retention.Daily),
			Weekly:  int32(d.Retention.Weekly),
			Monthly: int32(d.Retention.Monthly),
		},
		HasSecrets: len(d.Secrets) > 0,
		Stale:      backup.IsStale(d, now),
		CreatedAt:  d.CreatedAt,
	}
	if len(d.Options) > 0 {
		out.Options = apiv1.NewOptBackupDestinationOptions(apiv1.BackupDestinationOptions(d.Options))
	}
	if d.LastSuccessfulBackupAt != nil {
		out.LastSuccessfulBackupAt = apiv1.NewOptDateTime(*d.LastSuccessfulBackupAt)
	}
	return out
}

func (h *handler) ListBackupDestinations(ctx context.Context) (*apiv1.ListBackupDestinationsOK, error) {
	h.backupMu.Lock()
	defer h.backupMu.Unlock()
	now := time.Now().UTC()
	out := &apiv1.ListBackupDestinationsOK{Destinations: make([]apiv1.BackupDestination, 0, len(h.backupDestinations))}
	for _, d := range h.backupDestinations {
		out.Destinations = append(out.Destinations, mockBackupDestinationToAPI(d, now))
	}
	return out, nil
}

// CreateBackupDestination runs the same backup.PrepareDestination
// admission check production does (D18). This mock has rclone installed.
func (h *handler) CreateBackupDestination(ctx context.Context, req *apiv1.CreateBackupDestinationRequest) (*apiv1.BackupDestination, error) {
	h.notifyMu.Lock()
	hasPassphrase := h.generalSettings.BackupPassphraseSet
	h.notifyMu.Unlock()

	nd := backup.NewDestination{
		Name:    req.Name,
		Type:    backup.DestinationType(req.Type),
		Path:    req.Path,
		Options: map[string]string(req.Options.Value),
		Secrets: map[string]string(req.Secrets.Value),
	}
	if v, ok := req.Enabled.Get(); ok {
		nd.Enabled = &v
	}
	if v, ok := req.Encrypt.Get(); ok {
		nd.Encrypt = &v
	}
	if v, ok := req.Retention.Get(); ok {
		nd.Retention = &backup.Retention{Daily: int(v.Daily), Weekly: int(v.Weekly), Monthly: int(v.Monthly)}
	}

	h.backupMu.Lock()
	defer h.backupMu.Unlock()
	paths := backup.DefaultPaths("", "")
	dest, err := backup.PrepareDestination(nd, h.backupDestinations, hasPassphrase, paths.StateDir, paths.ConfigRoot)
	if err != nil {
		return nil, mapMockBackupDestinationError(err)
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	dest.ID = "dest-" + hex.EncodeToString(id[:])
	dest.CreatedAt = time.Now().UTC()
	h.backupDestinations = append(h.backupDestinations, dest)
	out := mockBackupDestinationToAPI(dest, dest.CreatedAt)
	return &out, nil
}

func (h *handler) DeleteBackupDestination(ctx context.Context, params apiv1.DeleteBackupDestinationParams) error {
	h.backupMu.Lock()
	defer h.backupMu.Unlock()
	for i, d := range h.backupDestinations {
		if d.ID == params.DestinationId {
			h.backupDestinations = append(h.backupDestinations[:i], h.backupDestinations[i+1:]...)
			return nil
		}
	}
	return &mockError{code: "backup_destination_not_found", statusCode: 404, message: "no backup destination with that id"}
}

// TestBackupDestination reports success for any known destination: the
// mock has no destination to write to.
func (h *handler) TestBackupDestination(ctx context.Context, params apiv1.TestBackupDestinationParams) (*apiv1.BackupDestinationTestResult, error) {
	h.backupMu.Lock()
	defer h.backupMu.Unlock()
	for _, d := range h.backupDestinations {
		if d.ID == params.DestinationId {
			return &apiv1.BackupDestinationTestResult{Success: true}, nil
		}
	}
	return nil, &mockError{code: "backup_destination_not_found", statusCode: 404, message: "no backup destination with that id"}
}
