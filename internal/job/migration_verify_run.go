package job

import (
	"context"
	"fmt"
	"io"
)

// RunMigrationVerify is the RunFunc hoservad registers for TypeMigrationVerify.
// verify is migrate.Service.RunVerify: it compares the adopted disks and the
// pool with the scan's baseline, records the result in the migration session
// and returns an error unless every comparison passed. The job has no params:
// what it compares is the session's baseline and the recorded pending array.
func RunMigrationVerify(verify func(ctx context.Context, out io.Writer) error) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		if err := verify(ctx, rc.Output()); err != nil {
			return fmt.Errorf("migration verify: %w", err)
		}
		return nil
	}
}
