package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// AppdataBackupParams is appdata_backup's persisted payload: the
// containers to back up, or none for every included one.
type AppdataBackupParams struct {
	Containers []string `json:"containers,omitempty"`
}

// AppdataRestoreParams is appdata_restore's persisted payload.
type AppdataRestoreParams struct {
	Container     string `json:"container"`
	Archive       string `json:"archive"`
	DestinationID string `json:"destinationId"`
}

func decodeAppdataBackupParams(params []byte) (AppdataBackupParams, error) {
	params = bytes.TrimSpace(params)
	if len(params) == 0 || string(params) == "null" {
		return AppdataBackupParams{}, nil
	}
	var p AppdataBackupParams
	if err := decodeJSON(params, &p); err != nil {
		return AppdataBackupParams{}, err
	}
	for _, name := range p.Containers {
		if name == "" {
			return AppdataBackupParams{}, errors.New("job: appdata_backup names an empty container")
		}
	}
	return p, nil
}

func decodeAppdataRestoreParams(params []byte) (AppdataRestoreParams, error) {
	var p AppdataRestoreParams
	if err := decodeJSON(bytes.TrimSpace(params), &p); err != nil {
		return AppdataRestoreParams{}, err
	}
	if p.Container == "" || p.Archive == "" || p.DestinationID == "" {
		return AppdataRestoreParams{}, errors.New("job: appdata_restore params require a container, an archive and a destination")
	}
	return p, nil
}

// AppdataBackupDeps is what RunAppdataBackup needs.
type AppdataBackupDeps struct {
	// Backup runs the backup (backup.AppdataService.Run), writing progress
	// to out.
	Backup func(ctx context.Context, containers []string, out io.Writer) error
	// Failed is called with the error of a backup that failed, so it can
	// be alerted on. It is not called when the job was cancelled.
	Failed func(ctx context.Context, err error)
}

// RunAppdataBackup is the RunFunc hoservad registers for TypeAppdataBackup.
func RunAppdataBackup(deps AppdataBackupDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeAppdataBackupParams(rc.Params())
		if err != nil {
			return err
		}
		if err := deps.Backup(ctx, p.Containers, rc.Output()); err != nil {
			if deps.Failed != nil && ctx.Err() == nil {
				deps.Failed(context.WithoutCancel(ctx), err)
			}
			return err
		}
		_, _ = fmt.Fprintln(rc.Output(), "appdata backup finished")
		return nil
	}
}

// RunAppdataRestore is the RunFunc hoservad registers for
// TypeAppdataRestore. restore is backup.AppdataService.Restore.
func RunAppdataRestore(restore func(ctx context.Context, p AppdataRestoreParams, out io.Writer) error) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeAppdataRestoreParams(rc.Params())
		if err != nil {
			return err
		}
		if err := restore(ctx, p, rc.Output()); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(rc.Output(), "appdata of %s restored\n", p.Container)
		return nil
	}
}
