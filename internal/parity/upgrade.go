package parity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// DefaultParityUpgradeChunkBytes is ParityUpgradeDeps.ChunkBytes's own
// default: how much of the parity file RunParityUpgrade copies between
// each checkpoint (doc 02 §4 "Larger parity disk", Q71). Checkpointing
// at this granularity, rather than only once the whole file is copied,
// means a killed job resumes from roughly where it left off instead of
// re-copying a parity file that can be many terabytes — and never risks
// more than one chunk's own bytes being left inconsistent by an
// interrupted write, since the next resume simply rewrites that chunk
// from its start.
const DefaultParityUpgradeChunkBytes = 4 << 20 // 4 MiB

// ErrParityUpgradeCopyMismatch is RunParityUpgrade's own refusal to
// switch the configuration onto the new parity disk (doc 02 §4, Q71):
// the byte-for-byte comparison between the old parity file and its new
// copy found a difference. Returning this is always safe — this
// package never writes to, truncates or removes the old parity file at
// any phase, so the array's protection stays exactly where it was
// before this run: the old file, under the configuration that still
// names it.
var ErrParityUpgradeCopyMismatch = errors.New("parity: new parity disk's copy does not match the old parity file byte for byte")

// ErrParityUpgradeDepsRequired is RunParityUpgrade's refusal to start
// at all when any of ParityUpgradeDeps' three required hooks is nil —
// before touching the old parity file, the new one, or the
// configuration.
var ErrParityUpgradeDepsRequired = errors.New("parity: run parity upgrade: ApplyLayout, Check and Release are all required")

// ParityUpgradePhase is where one RunParityUpgrade run currently is
// (doc 02 §4 "Larger parity disk", Q71): copy the parity file, verify
// it byte for byte, switch the configuration to the new disk, pass
// `snapraid check` against it, then release the old parity disk. The
// old parity disk's own file is never written to, read destructively,
// or removed by this package at any phase — and the configuration
// keeps naming it until SwitchingConfig itself actually succeeds — so
// the array's protection never lapses: whatever a caller does after an
// interruption, or after Verifying or Checking fails outright, the old
// file and (until the switch commits) the old configuration are
// exactly as valid as they were before this run started. Even a switch
// to a new disk that later fails its own Checking phase is not
// unrecoverable while the old physical disk still exists, unreleased:
// doc 02 §4's ordinary "Replacing a failed disk" flow (`snapraid fix`)
// can always reconstruct the array's expected state from parity plus
// the surviving data disks, regardless of which physical disk the
// configuration happens to name at the time.
type ParityUpgradePhase string

const (
	ParityUpgradePhaseCopying         ParityUpgradePhase = "copying"
	ParityUpgradePhaseVerifying       ParityUpgradePhase = "verifying"
	ParityUpgradePhaseSwitchingConfig ParityUpgradePhase = "switching_config"
	ParityUpgradePhaseChecking        ParityUpgradePhase = "checking"
	ParityUpgradePhaseReleasing       ParityUpgradePhase = "releasing"
)

// ParityUpgradeCheckpoint is RunParityUpgrade's own resumable progress
// marker (Q29). Offset is meaningful only during Copying: how many
// bytes of the new parity file are already a confirmed-in-flight copy
// of the old one, so a resumed copy seeks both files to Offset rather
// than restarting from scratch or trusting an unverified tail — a
// resumed copy that turns out to have been corrupted in flight (a
// partial write, a hole from a resume gone wrong) is still caught by
// Verifying's own byte-for-byte comparison before anything is switched.
type ParityUpgradeCheckpoint struct {
	Phase  ParityUpgradePhase `json:"phase"`
	Offset int64              `json:"offset,omitempty"`
}

// ParityUpgradeHooks lets a caller observe and control one
// RunParityUpgrade call without this package depending on internal/job
// (mirroring internal/cache's own RunHooks, doc.go): every field is
// optional, and StopRequested/SaveCheckpoint/SetProgress are shaped to
// be a direct passthrough from a *job.RunContext.
type ParityUpgradeHooks struct {
	StopRequested  <-chan struct{}
	SaveCheckpoint func(data []byte) error
	SetProgress    func(pct int)
	Log            func(format string, args ...any)
}

func (h ParityUpgradeHooks) checkpoint(cp ParityUpgradeCheckpoint) error {
	if h.SaveCheckpoint == nil {
		return nil
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("parity: encode parity upgrade checkpoint: %w", err)
	}
	return h.SaveCheckpoint(data)
}

func (h ParityUpgradeHooks) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

func (h ParityUpgradeHooks) progress(pct int) {
	if h.SetProgress != nil {
		h.SetProgress(pct)
	}
}

func (h ParityUpgradeHooks) stopRequested() bool {
	if h.StopRequested == nil {
		return false
	}
	select {
	case <-h.StopRequested:
		return true
	default:
		return false
	}
}

// ParityUpgradeSpec is one "larger parity disk" upgrade (doc 02 §4,
// Q71). OldParityPath is the current parity file — this package only
// ever opens it read-only, and never removes it. NewParityPath is the
// fresh, already-formatted-and-mounted new disk's own parity file path.
// NewLayout is the Layout to switch to once the copy is verified: the
// same ParityMounts as before, with the old parity disk's own mount
// replaced by the new one's (D4: this package only computes and hands
// off state, never writes snapraid.conf itself).
type ParityUpgradeSpec struct {
	OldParityPath string
	NewParityPath string
	NewLayout     Layout
}

// ParityUpgradeDeps are RunParityUpgrade's own system-touching
// dependencies (CLAUDE.md: every system-touching subsystem sits behind
// a package interface with a scriptable fake). ApplyLayout, Check and
// Release are all required.
type ParityUpgradeDeps struct {
	// ApplyLayout regenerates snapraid.conf (and anything else config
	// regeneration touches) from the layout handed to it — the "switch
	// the configuration" step (D4). This package never writes a managed
	// config file itself; the real wiring for this belongs with the rest
	// of internal/config's generator calls in internal/job, outside this
	// package's own scope.
	ApplyLayout func(ctx context.Context, layout Layout) error
	// Check runs `snapraid check` against the just-switched configuration
	// (Engine.Check) — the paranoid verification Q71 requires before the
	// old parity disk is released. A real caller wires this directly to
	// a *SnapraidEngine's own Check method.
	Check func(ctx context.Context, opts CheckOpts) (<-chan Progress, error)
	// Release is called only once Check has confirmed the new parity
	// disk is good: whatever the caller does to free the old parity disk
	// for reuse (doc 02 §4's "reuses the old parity disk as a data
	// disk") — disk- and store-level bookkeeping outside this package's
	// own scope. RunParityUpgrade's only guarantee about it is when it is
	// called: last, and only once Checking has actually passed.
	Release func(ctx context.Context) error
	// ChunkBytes overrides DefaultParityUpgradeChunkBytes.
	ChunkBytes int64
	Now        func() time.Time
}

func (d ParityUpgradeDeps) withDefaults() ParityUpgradeDeps {
	if d.ChunkBytes <= 0 {
		d.ChunkBytes = DefaultParityUpgradeChunkBytes
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return d
}

// ParityUpgradeResult is what RunParityUpgrade returns once it stops,
// whether by finishing, by a clean interruption at a checkpoint
// boundary, or (Interrupted false, a non-nil error) by a genuine
// failure such as ErrParityUpgradeCopyMismatch or a failed Check.
type ParityUpgradeResult struct {
	Interrupted bool
}

// RunParityUpgrade executes spec (doc 02 §4 "Larger parity disk", Q71),
// resuming from initialCheckpoint when it is non-empty (Q29). See
// ParityUpgradePhase's own doc comment for the safety property this
// enforces at every phase boundary.
func RunParityUpgrade(ctx context.Context, spec ParityUpgradeSpec, deps ParityUpgradeDeps, hooks ParityUpgradeHooks, initialCheckpoint []byte) (ParityUpgradeResult, error) {
	deps = deps.withDefaults()
	if deps.ApplyLayout == nil || deps.Check == nil || deps.Release == nil {
		return ParityUpgradeResult{}, ErrParityUpgradeDepsRequired
	}

	var cp ParityUpgradeCheckpoint
	if len(initialCheckpoint) > 0 {
		if err := json.Unmarshal(initialCheckpoint, &cp); err != nil {
			return ParityUpgradeResult{}, fmt.Errorf("parity: decode parity upgrade checkpoint: %w", err)
		}
	}
	if cp.Phase == "" {
		cp.Phase = ParityUpgradePhaseCopying
	}

	if cp.Phase == ParityUpgradePhaseCopying {
		interrupted, err := copyParityFile(ctx, spec, deps, hooks, cp.Offset)
		if err != nil {
			return ParityUpgradeResult{}, err
		}
		if interrupted {
			return ParityUpgradeResult{Interrupted: true}, nil
		}
		cp = ParityUpgradeCheckpoint{Phase: ParityUpgradePhaseVerifying}
		if err := hooks.checkpoint(cp); err != nil {
			return ParityUpgradeResult{}, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return ParityUpgradeResult{Interrupted: true}, nil
		}
	}

	if cp.Phase == ParityUpgradePhaseVerifying {
		hooks.logf("parity upgrade: verifying %s against %s byte for byte", spec.NewParityPath, spec.OldParityPath)
		same, err := filesByteIdentical(spec.OldParityPath, spec.NewParityPath)
		if err != nil {
			return ParityUpgradeResult{}, fmt.Errorf("parity: verify new parity disk copy: %w", err)
		}
		if !same {
			return ParityUpgradeResult{}, ErrParityUpgradeCopyMismatch
		}
		cp = ParityUpgradeCheckpoint{Phase: ParityUpgradePhaseSwitchingConfig}
		if err := hooks.checkpoint(cp); err != nil {
			return ParityUpgradeResult{}, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return ParityUpgradeResult{Interrupted: true}, nil
		}
	}

	if cp.Phase == ParityUpgradePhaseSwitchingConfig {
		hooks.logf("parity upgrade: switching configuration to %s", spec.NewParityPath)
		if err := deps.ApplyLayout(ctx, spec.NewLayout); err != nil {
			return ParityUpgradeResult{}, fmt.Errorf("parity: switch configuration to new parity disk: %w", err)
		}
		cp = ParityUpgradeCheckpoint{Phase: ParityUpgradePhaseChecking}
		if err := hooks.checkpoint(cp); err != nil {
			return ParityUpgradeResult{}, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return ParityUpgradeResult{Interrupted: true}, nil
		}
	}

	if cp.Phase == ParityUpgradePhaseChecking {
		hooks.logf("parity upgrade: running snapraid check against the new parity disk")
		ch, err := deps.Check(ctx, CheckOpts{})
		if err != nil {
			return ParityUpgradeResult{}, fmt.Errorf("parity: check new parity disk: %w", err)
		}
		var final Progress
		for p := range ch {
			final = p
			hooks.progress(int(p.Percent))
		}
		if final.Err != nil {
			return ParityUpgradeResult{}, fmt.Errorf("parity: check new parity disk: %w", final.Err)
		}
		cp = ParityUpgradeCheckpoint{Phase: ParityUpgradePhaseReleasing}
		if err := hooks.checkpoint(cp); err != nil {
			return ParityUpgradeResult{}, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return ParityUpgradeResult{Interrupted: true}, nil
		}
	}

	if cp.Phase == ParityUpgradePhaseReleasing {
		hooks.logf("parity upgrade: releasing the old parity disk")
		if err := deps.Release(ctx); err != nil {
			return ParityUpgradeResult{}, fmt.Errorf("parity: release old parity disk: %w", err)
		}
	}

	return ParityUpgradeResult{}, nil
}

// copyParityFile copies spec.OldParityPath to spec.NewParityPath,
// resuming at startOffset (0 for a fresh start). It only ever opens
// OldParityPath read-only; NewParityPath is truncated only when
// startOffset is 0, so a resumed copy never discards bytes an earlier,
// interrupted run already wrote and checkpointed past. It checkpoints
// after every chunk, and checks ctx and hooks.stopRequested() before
// each one, so a caller can interrupt it between any two chunks without
// losing more than the (unwritten) chunk in flight.
func copyParityFile(ctx context.Context, spec ParityUpgradeSpec, deps ParityUpgradeDeps, hooks ParityUpgradeHooks, startOffset int64) (interrupted bool, err error) {
	src, err := os.Open(spec.OldParityPath)
	if err != nil {
		return false, fmt.Errorf("parity: open old parity file: %w", err)
	}
	defer func() { _ = src.Close() }()

	flags := os.O_CREATE | os.O_WRONLY
	if startOffset == 0 {
		flags |= os.O_TRUNC
	}
	dst, err := os.OpenFile(spec.NewParityPath, flags, 0o644)
	if err != nil {
		return false, fmt.Errorf("parity: open new parity file: %w", err)
	}
	defer func() { _ = dst.Close() }()

	if startOffset > 0 {
		if _, err := src.Seek(startOffset, io.SeekStart); err != nil {
			return false, fmt.Errorf("parity: seek old parity file to resume offset %d: %w", startOffset, err)
		}
		if _, err := dst.Seek(startOffset, io.SeekStart); err != nil {
			return false, fmt.Errorf("parity: seek new parity file to resume offset %d: %w", startOffset, err)
		}
	}

	total, err := fileSize(spec.OldParityPath)
	if err != nil {
		return false, err
	}

	buf := make([]byte, deps.ChunkBytes)
	offset := startOffset
	for {
		if ctx.Err() != nil || hooks.stopRequested() {
			return true, nil
		}

		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return false, fmt.Errorf("parity: write new parity file: %w", werr)
			}
			offset += int64(n)
			if err := hooks.checkpoint(ParityUpgradeCheckpoint{Phase: ParityUpgradePhaseCopying, Offset: offset}); err != nil {
				return false, err
			}
			if total > 0 {
				hooks.progress(int(offset * 100 / total))
			}
			hooks.logf("parity upgrade: copied %d/%d bytes to %s", offset, total, spec.NewParityPath)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return false, fmt.Errorf("parity: read old parity file: %w", rerr)
		}
	}

	if err := dst.Sync(); err != nil {
		return false, fmt.Errorf("parity: fsync new parity file: %w", err)
	}
	return false, nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("parity: stat %s: %w", path, err)
	}
	return info.Size(), nil
}

// filesByteIdentical reports whether a and b have identical content,
// read start to finish, comparing chunk by chunk — literally byte for
// byte (Q71), never a checksum shortcut.
func filesByteIdentical(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", a, err)
	}
	defer func() { _ = fa.Close() }()
	fb, err := os.Open(b)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", b, err)
	}
	defer func() { _ = fb.Close() }()

	bufA := make([]byte, 1<<20)
	bufB := make([]byte, 1<<20)
	for {
		na, erra := io.ReadFull(fa, bufA)
		nb, errb := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}

		doneA := erra == io.EOF || erra == io.ErrUnexpectedEOF
		doneB := errb == io.EOF || errb == io.ErrUnexpectedEOF
		if doneA != doneB {
			return false, nil
		}
		if doneA {
			return true, nil
		}
		if erra != nil {
			return false, fmt.Errorf("read %s: %w", a, erra)
		}
		if errb != nil {
			return false, fmt.Errorf("read %s: %w", b, errb)
		}
	}
}
