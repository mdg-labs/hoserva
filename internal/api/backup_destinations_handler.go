package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
)

func mapBackupDestinationError(err error) error {
	switch {
	case errors.Is(err, backup.ErrPassphraseRequired):
		return &apiError{code: "backup_passphrase_required", statusCode: 400, message: err.Error()}
	case errors.Is(err, backup.ErrInvalidDestination):
		return &apiError{code: "backup_destination_invalid", statusCode: 400, message: err.Error()}
	case errors.Is(err, backup.ErrDestinationExists):
		return &apiError{code: "backup_destination_exists", statusCode: 409, message: err.Error()}
	case errors.Is(err, backup.ErrDestinationNotFound):
		return &apiError{code: "backup_destination_not_found", statusCode: 404, message: "no backup destination with that id"}
	case errors.Is(err, backup.ErrNoEnabledDestination):
		return &apiError{code: "backup_no_destination", statusCode: 409, message: err.Error()}
	case errors.Is(err, backup.ErrRcloneMissing):
		return &apiError{code: "rclone_missing", statusCode: 424, message: err.Error()}
	default:
		return err
	}
}

func (h *Handler) backupDestinations() (*backup.Service, error) {
	if h.Backup == nil || h.Backup.Store == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "backup destinations are not configured on this daemon"}
	}
	return h.Backup, nil
}

func backupDestinationToAPI(d backup.Destination, now time.Time) apiv1.BackupDestination {
	typ := d.Type
	if typ == "" {
		typ = backup.TypeLocal
	}
	out := apiv1.BackupDestination{
		ID:      d.ID,
		Name:    d.Name,
		Type:    apiv1.BackupDestinationType(typ),
		Path:    d.Path,
		Enabled: d.Enabled,
		Encrypt: d.Encrypt,
		Retention: apiv1.BackupRetention{
			Daily:   int32(d.Retention.Daily),
			Weekly:  int32(d.Retention.Weekly),
			Monthly: int32(d.Retention.Monthly),
		},
		HasSecrets: len(d.SealedSecrets) > 0,
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

func (h *Handler) ListBackupDestinations(ctx context.Context) (*apiv1.ListBackupDestinationsOK, error) {
	svc, err := h.backupDestinations()
	if err != nil {
		return nil, err
	}
	dests, err := svc.ListDestinations(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing backup destinations: %w", err)
	}
	now := time.Now().UTC()
	out := &apiv1.ListBackupDestinationsOK{Destinations: make([]apiv1.BackupDestination, 0, len(dests))}
	for _, d := range dests {
		out.Destinations = append(out.Destinations, backupDestinationToAPI(d, now))
	}
	return out, nil
}

func (h *Handler) CreateBackupDestination(ctx context.Context, req *apiv1.CreateBackupDestinationRequest) (*apiv1.BackupDestination, error) {
	svc, err := h.backupDestinations()
	if err != nil {
		return nil, err
	}
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
	dest, err := svc.AddDestination(ctx, nd)
	if err != nil {
		return nil, mapBackupDestinationError(err)
	}
	out := backupDestinationToAPI(dest, time.Now().UTC())
	return &out, nil
}

func (h *Handler) UpdateBackupDestination(ctx context.Context, req *apiv1.UpdateBackupDestinationRequest, params apiv1.UpdateBackupDestinationParams) (*apiv1.BackupDestination, error) {
	svc, err := h.backupDestinations()
	if err != nil {
		return nil, err
	}
	var u backup.DestinationUpdate
	if v, ok := req.Enabled.Get(); ok {
		u.Enabled = &v
	}
	if v, ok := req.Retention.Get(); ok {
		u.Retention = &backup.Retention{Daily: int(v.Daily), Weekly: int(v.Weekly), Monthly: int(v.Monthly)}
	}
	dest, err := svc.UpdateDestination(ctx, params.DestinationId, u)
	if err != nil {
		return nil, mapBackupDestinationError(err)
	}
	out := backupDestinationToAPI(dest, time.Now().UTC())
	return &out, nil
}

func (h *Handler) DeleteBackupDestination(ctx context.Context, params apiv1.DeleteBackupDestinationParams) error {
	svc, err := h.backupDestinations()
	if err != nil {
		return err
	}
	if err := svc.RemoveDestination(ctx, params.DestinationId); err != nil {
		return mapBackupDestinationError(err)
	}
	return nil
}

func (h *Handler) TestBackupDestination(ctx context.Context, params apiv1.TestBackupDestinationParams) (*apiv1.BackupDestinationTestResult, error) {
	svc, err := h.backupDestinations()
	if err != nil {
		return nil, err
	}
	result, err := svc.TestDestination(ctx, params.DestinationId)
	if err != nil {
		return nil, mapBackupDestinationError(err)
	}
	out := &apiv1.BackupDestinationTestResult{Success: result.Success}
	if !result.Success {
		out.Error = apiv1.NewOptNilString(result.Error)
	}
	return out, nil
}
