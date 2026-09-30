package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ConfigBackupJobResource is the resource scope of a config_backup job: two
// of them never run at once, and one asked for while another runs queues
// behind it.
const ConfigBackupJobResource = "backup:config"

// ErrNoEnabledDestination is a config backup asked for while no backup
// destination is enabled: there is nowhere to write it.
var ErrNoEnabledDestination = errors.New("backup: no backup destination is enabled, so a config backup would be written nowhere — enable or add one first")

// RequireEnabledDestination returns ErrNoEnabledDestination unless at least
// one destination is enabled.
func (s *Service) RequireEnabledDestination(ctx context.Context) error {
	dests, err := s.loadDestinations(ctx)
	if err != nil {
		return err
	}
	for _, d := range dests {
		if d.Enabled {
			return nil
		}
	}
	return ErrNoEnabledDestination
}

// RunConfigBackup is the config_backup job's body: the same archive, write,
// verification and pruning as the nightly chain's config backup, taken now
// on request. The archive carries no reason, so retention counts it like a
// scheduled one and it never takes a pre-change archive's slot.
//
// It writes one line to out for the destinations written and one for each
// that failed. A run that wrote at least one destination succeeds — the
// nightly chain's rule, with the stale-destination alert covering the
// destinations that keep failing — and a run that wrote none returns the
// destinations' errors, or ErrNoEnabledDestination when none was enabled by
// the time it ran.
func (s *Service) RunConfigBackup(ctx context.Context, out io.Writer) error {
	written, err := s.RunReasonArchive(ctx, ReasonNone)
	if err != nil {
		return err
	}
	if len(written.Destinations) == 0 {
		return ErrNoEnabledDestination
	}
	_, _ = fmt.Fprintf(out, "wrote %s to %s\n", written.Name, strings.Join(written.Destinations, ", "))
	for _, f := range written.Failed {
		_, _ = fmt.Fprintf(out, "%s: %v\n", f.Destination, f.Err)
	}
	return nil
}
