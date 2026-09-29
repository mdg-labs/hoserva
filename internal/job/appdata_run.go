package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// AppdataBackupParams is appdata_backup's persisted payload: the
// containers requested, or none for every included one, and the names
// those resolved to when the job was submitted. Resolved is the job's
// scope, so the run acts on no container outside it and a job without it
// does not run. A submit that resolved to nothing records an empty,
// non-nil list.
type AppdataBackupParams struct {
	Containers []string `json:"containers,omitempty"`
	Resolved   []string `json:"resolved"`
}

// AppdataRestoreParams is the persisted payload of appdata_restore and of
// appdata_restore_preview, which names the same archive.
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
	for _, name := range append(append([]string(nil), p.Containers...), p.Resolved...) {
		if name == "" {
			return AppdataBackupParams{}, errors.New("job: appdata_backup names an empty container")
		}
	}
	return p, nil
}

func decodeAppdataRestoreParams(t Type, params []byte) (AppdataRestoreParams, error) {
	var p AppdataRestoreParams
	if err := decodeJSON(bytes.TrimSpace(params), &p); err != nil {
		return AppdataRestoreParams{}, err
	}
	if p.Container == "" || p.Archive == "" || p.DestinationID == "" {
		return AppdataRestoreParams{}, fmt.Errorf("job: %s params require a container, an archive and a destination", t)
	}
	return p, nil
}

// AppdataBackupDeps is what RunAppdataBackup needs.
type AppdataBackupDeps struct {
	// Backup runs the backup (backup.AppdataService.Run) on exactly the
	// resolved names the job was submitted with, skipping any that are gone
	// or no longer included, writing progress to out.
	Backup func(ctx context.Context, requested, resolved []string, out io.Writer) error
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
		if p.Resolved == nil {
			return errors.New("job: appdata_backup params carry no resolved containers, so the job has no scope to run in")
		}
		if err := deps.Backup(ctx, p.Containers, p.Resolved, rc.Output()); err != nil {
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
		p, err := decodeAppdataRestoreParams(TypeAppdataRestore, rc.Params())
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

// RunAppdataRestorePreview is the RunFunc hoservad registers for
// TypeAppdataRestorePreview. preview is backup.AppdataService.RunPreview,
// which holds the result under the job's id.
func RunAppdataRestorePreview(preview func(ctx context.Context, jobID string, p AppdataRestoreParams, out io.Writer) error) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeAppdataRestoreParams(TypeAppdataRestorePreview, rc.Params())
		if err != nil {
			return err
		}
		if err := preview(ctx, rc.JobID(), p, rc.Output()); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(rc.Output(), "previewed restoring %s\n", p.Container)
		return nil
	}
}
