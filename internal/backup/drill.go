package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DrillJobResource serializes restore drills with one another.
const DrillJobResource = "backup:drill"

// DrillDestination is what the drill found on one destination.
type DrillDestination struct {
	DestinationID   string
	DestinationName string
	// Archive is the archive that was fetched and verified; empty when
	// none could be found.
	Archive string
	Passed  bool
	Error   string
}

// DrillResult is the outcome of one restore drill (doc 10 §1). It passed
// only when every enabled destination's newest archive verified.
type DrillResult struct {
	RanAt  time.Time
	Passed bool
	// Error says why the drill could not test any destination at all: none
	// is enabled, the destinations could not be listed, or the drill could
	// not be started.
	Error        string
	Destinations []DrillDestination
}

// DrillStore persists the last restore drill's result (D4).
type DrillStore interface {
	// RecordDrill replaces the stored result.
	RecordDrill(ctx context.Context, r DrillResult) error
	// LastDrill returns nil when no drill has ever been recorded.
	LastDrill(ctx context.Context) (*DrillResult, error)
}

// DrillAlert publishes one failed drill.
type DrillAlert func(ctx context.Context, r DrillResult) error

// LastDrill is the stored result of the most recent drill, nil if none has
// run.
func (s *Service) LastDrill(ctx context.Context) (*DrillResult, error) {
	if s.Drills == nil {
		return nil, errors.New("backup: restore drills are not configured")
	}
	return s.Drills.LastDrill(ctx)
}

// RunDrill fetches the newest archive this installation wrote to each
// enabled destination, opens it the way a restore would (an encrypted one
// through its identity sidecar and the backup passphrase alone, the way it
// is opened once this box is gone), verifies its checksums and database,
// and discards everything it fetched. It never writes to a destination and
// never reads the live database.
//
// A destination that cannot be read, or holds no archive, fails the drill:
// a drill that could not look is not a drill that passed. The result is
// recorded and, when the drill failed, alert is called; each is attempted
// whatever happened to the other, and the returned error names every part
// that failed. A run whose ctx ends concludes nothing and records nothing.
func (s *Service) RunDrill(ctx context.Context, out io.Writer, alert DrillAlert) error {
	if out == nil {
		out = io.Discard
	}
	res := s.drill(ctx, out)
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.concludeDrill(ctx, res, alert)
}

// FailDrill records and alerts on a drill that could not be started, so a
// scheduled drill that never ran is as visible as one that failed.
func (s *Service) FailDrill(ctx context.Context, cause error, alert DrillAlert) error {
	return s.concludeDrill(ctx, DrillResult{RanAt: s.now(), Error: cause.Error()}, alert)
}

func (s *Service) concludeDrill(ctx context.Context, res DrillResult, alert DrillAlert) error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	if !res.Passed {
		errs = append(errs, errors.New("restore drill failed: "+res.Failure()))
	}
	if s.Drills == nil {
		errs = append(errs, errors.New("recording the drill result: no drill store is configured"))
	} else if err := s.Drills.RecordDrill(ctx, res); err != nil {
		errs = append(errs, fmt.Errorf("recording the drill result: %w", err))
	}
	if !res.Passed && alert != nil {
		if err := alert(ctx, res); err != nil {
			errs = append(errs, fmt.Errorf("alerting on the failed drill: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) drill(ctx context.Context, out io.Writer) DrillResult {
	res := DrillResult{RanAt: s.now()}
	all, err := s.loadDestinations(ctx)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for _, dest := range all {
		if !dest.Enabled {
			continue
		}
		if ctx.Err() != nil {
			return res
		}
		d := s.drillDestination(ctx, dest)
		if d.Passed {
			_, _ = fmt.Fprintf(out, "%s: verified %s\n", dest.Name, d.Archive)
		} else {
			_, _ = fmt.Fprintf(out, "%s: %s\n", dest.Name, d.Error)
		}
		res.Destinations = append(res.Destinations, d)
	}
	if len(res.Destinations) == 0 {
		res.Error = "no backup destination is enabled, so there is no archive to test"
		return res
	}
	res.Passed = true
	for _, d := range res.Destinations {
		res.Passed = res.Passed && d.Passed
	}
	return res
}

func (s *Service) drillDestination(ctx context.Context, dest Destination) DrillDestination {
	d := DrillDestination{DestinationID: dest.ID, DestinationName: dest.Name}
	dir, err := os.MkdirTemp("", "hoserva-backup-drill-*")
	if err != nil {
		d.Error = fmt.Sprintf("creating the drill's temporary directory: %v", err)
		return d
	}
	defer func() { _ = os.RemoveAll(dir) }()

	fetched, err := s.fetchNewest(ctx, dest, dir)
	d.Archive = fetched.name
	if err == nil {
		err = s.openAndVerify(ctx, fetched, dir)
	}
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.Passed = true
	return d
}

type drillFetch struct {
	name    string
	path    string
	sidecar string
}

// fetchNewest copies the destination's newest archive of this installation
// into dir, with its identity sidecar when it is encrypted. The pool gate is
// held only for the copy, so a drill cannot hold up an array stop while it
// verifies.
func (s *Service) fetchNewest(ctx context.Context, dest Destination, dir string) (drillFetch, error) {
	release, why := s.admitDestination(ctx, dest)
	if why != "" {
		return drillFetch{}, fmt.Errorf("the destination cannot be read now: %s", why)
	}
	defer release()

	target, err := s.targetFor(ctx, dest)
	if err != nil {
		return drillFetch{}, fmt.Errorf("preparing the destination: %w", err)
	}
	listed, err := target.list(ctx)
	if err != nil {
		return drillFetch{}, fmt.Errorf("listing the destination: %w", err)
	}
	owner := s.archiveOwner(dest)
	var newest *archiveEntry
	for i := range listed {
		if owner.owns(listed[i]) && (newest == nil || listed[i].modTime.After(newest.modTime)) {
			newest = &listed[i]
		}
	}
	if newest == nil {
		return drillFetch{}, errors.New("the destination holds no config archive written by this installation")
	}

	got := drillFetch{name: newest.name, path: filepath.Join(dir, newest.name)}
	if err := target.fetch(ctx, newest.name, got.path); err != nil {
		return got, fmt.Errorf("fetching %s: %w", newest.name, err)
	}
	if strings.HasSuffix(newest.name, ".age") {
		got.sidecar = got.path + identitySidecarSuffix
		if err := target.fetch(ctx, newest.name+identitySidecarSuffix, got.sidecar); err != nil {
			return got, fmt.Errorf("fetching the identity sidecar of %s: %w", newest.name, err)
		}
	}
	return got, nil
}

func (s *Service) openAndVerify(ctx context.Context, f drillFetch, dir string) error {
	passphrase := ""
	if s.Secrets != nil {
		p, ok, err := s.Secrets.BackupPassphrase(ctx)
		if err != nil {
			return fmt.Errorf("reading the backup passphrase: %w", err)
		}
		if ok {
			passphrase = p
		}
	}
	verifyPath := f.path
	if f.sidecar != "" {
		if passphrase == "" {
			return fmt.Errorf("%s is encrypted and no backup passphrase is set, so it cannot be opened", f.name)
		}
		plain, err := decryptArchiveWithPassphrase(f.path, f.sidecar, passphrase)
		if err != nil {
			return fmt.Errorf("opening %s with the backup passphrase: %w", f.name, err)
		}
		verifyPath = filepath.Join(dir, "decrypted.tar.zst")
		if err := os.WriteFile(verifyPath, plain, 0o600); err != nil {
			return fmt.Errorf("writing the decrypted archive: %w", err)
		}
	}
	if err := VerifyArchive(verifyPath, passphrase); err != nil {
		return fmt.Errorf("verifying %s: %w", f.name, err)
	}
	return nil
}

// Failure says why the drill failed: its own error and every destination
// that did not pass, each with its reason.
func (r DrillResult) Failure() string {
	var parts []string
	if r.Error != "" {
		parts = append(parts, r.Error)
	}
	for _, d := range r.Destinations {
		if !d.Passed {
			parts = append(parts, fmt.Sprintf("%s: %s", d.DestinationName, d.Error))
		}
	}
	return strings.Join(parts, "; ")
}
