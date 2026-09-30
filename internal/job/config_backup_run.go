package job

import (
	"context"
	"fmt"
	"io"
)

// ConfigBackupDeps is what RunConfigBackup needs.
type ConfigBackupDeps struct {
	// Backup is backup.Service.RunConfigBackup: it writes the config
	// archive to every enabled destination, and returns an error when it
	// wrote none, so the job ends failed with the reason recorded.
	Backup func(ctx context.Context, out io.Writer) error
	// Failed is called with the error of a backup that failed, so it can
	// be alerted on. It is not called when the job was cancelled.
	Failed func(ctx context.Context, err error)
}

// RunConfigBackup is the RunFunc hoservad registers for TypeConfigBackup.
func RunConfigBackup(deps ConfigBackupDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		if err := deps.Backup(ctx, rc.Output()); err != nil {
			if deps.Failed != nil && ctx.Err() == nil {
				deps.Failed(context.WithoutCancel(ctx), err)
			}
			return err
		}
		_, _ = fmt.Fprintln(rc.Output(), "config backup finished")
		return nil
	}
}
