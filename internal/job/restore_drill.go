package job

import (
	"context"
	"fmt"
	"io"
)

// RunRestoreDrill is the RunFunc hoservad registers for TypeRestoreDrill.
// drill is backup.Service.RunDrill: it returns an error for a drill that
// failed, so the job ends failed with the reason recorded.
func RunRestoreDrill(drill func(ctx context.Context, out io.Writer) error) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		if err := drill(ctx, rc.Output()); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(rc.Output(), "restore drill passed")
		return nil
	}
}
