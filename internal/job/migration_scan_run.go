package job

import (
	"context"
	"fmt"
	"io"
)

// RunMigrationScan is the RunFunc hoservad registers for TypeMigrationScan.
// scan is migrate.Service.RunScan; the upload it scans is named in the job's
// params, which the migration service chose, so they survive a restart between
// queue and run.
func RunMigrationScan(scan func(ctx context.Context, out io.Writer, upload string) error) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeMigrationScanParams(rc.Params())
		if err != nil {
			return err
		}
		if err := scan(ctx, rc.Output(), p.Upload); err != nil {
			return fmt.Errorf("migration scan: %w", err)
		}
		return nil
	}
}
