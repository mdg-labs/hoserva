package job

import (
	"context"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// RunDiskFormat is the RunFunc hoservad registers for TypeDiskFormat: it
// reads persisted DiskFormatParams and calls disk.FormatPlan. Confirmation
// and Validate run again here so a queued job cannot skip the guard the
// handler already enforced (doc 03 §3.1 step 6).
func RunDiskFormat(p disk.Provider, r disk.Runner) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		params, err := decodeDiskFormatParams(rc.Params())
		if err != nil {
			return err
		}
		return disk.FormatPlan(ctx, p, r, params.Plan(), params.Sizes, params.Confirmation)
	}
}
