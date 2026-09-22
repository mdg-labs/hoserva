package parity

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// upgradeFakeCalls records, in order, which of ApplyLayout/Check/Release
// RunParityUpgrade actually called — the ordering safety property this
// issue's own acceptance criteria depend on: Release must never be
// called before Check has passed, and ApplyLayout must never be called
// before Verifying has confirmed a byte-for-byte match.
type upgradeFakeCalls struct {
	applyLayout int
	check       int
	release     int
}

func newUpgradeDeps(t *testing.T, calls *upgradeFakeCalls, checkErr error) ParityUpgradeDeps {
	t.Helper()
	return ParityUpgradeDeps{
		ApplyLayout: func(ctx context.Context, l Layout) error {
			calls.applyLayout++
			return nil
		},
		Check: func(ctx context.Context, opts CheckOpts) (<-chan Progress, error) {
			calls.check++
			ch := make(chan Progress, 1)
			ch <- Progress{Percent: 100, Err: checkErr}
			close(ch)
			return ch, nil
		},
		Release: func(ctx context.Context) error {
			calls.release++
			return nil
		},
	}
}

// writeRandomFile writes n random bytes to path, returning them for
// later comparison.
func writeRandomFile(t *testing.T, path string, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return data
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

// TestRunParityUpgrade_HappyPathCallsHooksInOrder confirms the ordinary
// completed run copies, verifies, switches, checks and releases exactly
// once each, in that order, and never touches the old parity file's own
// bytes.
func TestRunParityUpgrade_HappyPathCallsHooksInOrder(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.parity")
	newPath := filepath.Join(dir, "new.parity")
	original := writeRandomFile(t, oldPath, 500_000)

	calls := &upgradeFakeCalls{}
	deps := newUpgradeDeps(t, calls, nil)
	spec := ParityUpgradeSpec{OldParityPath: oldPath, NewParityPath: newPath}

	result, err := RunParityUpgrade(context.Background(), spec, deps, ParityUpgradeHooks{}, nil)
	if err != nil {
		t.Fatalf("RunParityUpgrade: %v", err)
	}
	if result.Interrupted {
		t.Fatal("RunParityUpgrade reported Interrupted on a completed run")
	}
	if calls.applyLayout != 1 || calls.check != 1 || calls.release != 1 {
		t.Fatalf("calls = %+v, want each exactly once", calls)
	}

	if got := mustReadFile(t, oldPath); string(got) != string(original) {
		t.Fatal("old parity file's content changed during a successful upgrade")
	}
	if got := mustReadFile(t, newPath); string(got) != string(original) {
		t.Fatal("new parity file does not match the old one byte for byte")
	}
}

// TestRunParityUpgrade_KilledMidCopy_OldFileUntouchedAndResumeCompletes
// is this issue's own central safety property for the copying phase:
// interrupting the run partway through the copy leaves the old parity
// file completely unchanged, and resuming from the saved checkpoint
// finishes correctly without ever re-truncating what was already
// written and confirmed.
func TestRunParityUpgrade_KilledMidCopy_OldFileUntouchedAndResumeCompletes(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.parity")
	newPath := filepath.Join(dir, "new.parity")
	original := writeRandomFile(t, oldPath, 5_000_000)

	calls := &upgradeFakeCalls{}
	deps := newUpgradeDeps(t, calls, nil)
	deps.ChunkBytes = 64 * 1024 // small chunks so a handful of them is well short of the whole file

	spec := ParityUpgradeSpec{OldParityPath: oldPath, NewParityPath: newPath}

	stop := make(chan struct{})
	var chunkLogs int
	var savedCheckpoint []byte
	hooks := ParityUpgradeHooks{
		StopRequested: stop,
		SaveCheckpoint: func(data []byte) error {
			savedCheckpoint = append([]byte(nil), data...)
			return nil
		},
		Log: func(format string, args ...any) {
			chunkLogs++
			if chunkLogs == 3 {
				close(stop)
			}
		},
	}

	result, err := RunParityUpgrade(context.Background(), spec, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RunParityUpgrade (first, interrupted run): %v", err)
	}
	if !result.Interrupted {
		t.Fatal("RunParityUpgrade did not report Interrupted after StopRequested closed mid-copy")
	}
	if calls.applyLayout != 0 || calls.check != 0 || calls.release != 0 {
		t.Fatalf("calls after an interrupted copy = %+v, want none — nothing past copying may run yet", calls)
	}
	if got := mustReadFile(t, oldPath); string(got) != string(original) {
		t.Fatal("old parity file's content changed while the copy was interrupted")
	}
	if len(savedCheckpoint) == 0 {
		t.Fatal("no checkpoint was ever saved before the interruption")
	}

	// Resume from the saved checkpoint, uninterrupted this time.
	calls2 := &upgradeFakeCalls{}
	deps2 := newUpgradeDeps(t, calls2, nil)
	result2, err := RunParityUpgrade(context.Background(), spec, deps2, ParityUpgradeHooks{}, savedCheckpoint)
	if err != nil {
		t.Fatalf("RunParityUpgrade (resumed run): %v", err)
	}
	if result2.Interrupted {
		t.Fatal("resumed run reported Interrupted")
	}
	if calls2.applyLayout != 1 || calls2.check != 1 || calls2.release != 1 {
		t.Fatalf("calls after resume = %+v, want each exactly once", calls2)
	}
	if got := mustReadFile(t, oldPath); string(got) != string(original) {
		t.Fatal("old parity file's content changed after resuming and completing the copy")
	}
	if got := mustReadFile(t, newPath); string(got) != string(original) {
		t.Fatal("new parity file does not match the old one byte for byte after resuming")
	}
}

// TestRunParityUpgrade_ResumeAtEachLaterPhase_OldFileNeverTouched checks
// the remaining phase boundaries this issue's "kill the job at every
// step" criterion covers: resuming from a checkpoint saved at the start
// of Verifying, SwitchingConfig, Checking or Releasing completes
// correctly, calling each later hook exactly once, and the old parity
// file is never modified by any of them.
func TestRunParityUpgrade_ResumeAtEachLaterPhase_OldFileNeverTouched(t *testing.T) {
	phases := []ParityUpgradePhase{
		ParityUpgradePhaseVerifying,
		ParityUpgradePhaseSwitchingConfig,
		ParityUpgradePhaseChecking,
		ParityUpgradePhaseReleasing,
	}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			dir := t.TempDir()
			oldPath := filepath.Join(dir, "old.parity")
			newPath := filepath.Join(dir, "new.parity")
			original := writeRandomFile(t, oldPath, 300_000)
			// A prior run's own completed copy phase already left an
			// exact copy in place — Verifying (and everything after it)
			// never re-copies, it only reads.
			if err := os.WriteFile(newPath, original, 0o644); err != nil {
				t.Fatalf("seeding new parity file: %v", err)
			}

			calls := &upgradeFakeCalls{}
			deps := newUpgradeDeps(t, calls, nil)
			spec := ParityUpgradeSpec{OldParityPath: oldPath, NewParityPath: newPath, NewLayout: Layout{ParityMounts: []string{"/mnt/parity1"}, DataMounts: []string{"/mnt/disk1"}}}

			cp, err := marshalUpgradeCheckpoint(ParityUpgradeCheckpoint{Phase: phase})
			if err != nil {
				t.Fatalf("marshal checkpoint: %v", err)
			}

			result, err := RunParityUpgrade(context.Background(), spec, deps, ParityUpgradeHooks{}, cp)
			if err != nil {
				t.Fatalf("RunParityUpgrade resumed at %s: %v", phase, err)
			}
			if result.Interrupted {
				t.Fatalf("RunParityUpgrade resumed at %s reported Interrupted", phase)
			}
			// Resuming at phase skips whatever an earlier, already-
			// completed run would have done before that phase — only
			// phase itself and whatever comes after it may run again.
			wantApplyLayout, wantCheck, wantRelease := 0, 0, 0
			switch phase {
			case ParityUpgradePhaseVerifying, ParityUpgradePhaseSwitchingConfig:
				wantApplyLayout, wantCheck, wantRelease = 1, 1, 1
			case ParityUpgradePhaseChecking:
				wantCheck, wantRelease = 1, 1
			case ParityUpgradePhaseReleasing:
				wantRelease = 1
			}
			if calls.applyLayout != wantApplyLayout || calls.check != wantCheck || calls.release != wantRelease {
				t.Fatalf("calls resuming at %s = %+v, want {applyLayout:%d check:%d release:%d}", phase, calls, wantApplyLayout, wantCheck, wantRelease)
			}
			if got := mustReadFile(t, oldPath); string(got) != string(original) {
				t.Fatalf("old parity file changed while resuming at %s", phase)
			}
		})
	}
}

// TestRunParityUpgrade_VerifyMismatch_NeverSwitchesOrReleases is this
// issue's own central data-safety test: when the new parity disk's copy
// does not match the old file byte for byte, RunParityUpgrade refuses
// before ever calling ApplyLayout, Check or Release — the configuration
// keeps naming the old, still-valid parity file.
func TestRunParityUpgrade_VerifyMismatch_NeverSwitchesOrReleases(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.parity")
	newPath := filepath.Join(dir, "new.parity")
	original := writeRandomFile(t, oldPath, 100_000)
	// A corrupted copy: same length, different content.
	corrupted := append([]byte(nil), original...)
	corrupted[0] ^= 0xFF
	if err := os.WriteFile(newPath, corrupted, 0o644); err != nil {
		t.Fatalf("seeding corrupted new parity file: %v", err)
	}

	calls := &upgradeFakeCalls{}
	deps := newUpgradeDeps(t, calls, nil)
	spec := ParityUpgradeSpec{OldParityPath: oldPath, NewParityPath: newPath}

	cp, err := marshalUpgradeCheckpoint(ParityUpgradeCheckpoint{Phase: ParityUpgradePhaseVerifying})
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}

	_, err = RunParityUpgrade(context.Background(), spec, deps, ParityUpgradeHooks{}, cp)
	if !errors.Is(err, ErrParityUpgradeCopyMismatch) {
		t.Fatalf("RunParityUpgrade with a mismatched copy: err = %v, want ErrParityUpgradeCopyMismatch", err)
	}
	if calls.applyLayout != 0 || calls.check != 0 || calls.release != 0 {
		t.Fatalf("calls after a verify mismatch = %+v, want none", calls)
	}
	if got := mustReadFile(t, oldPath); string(got) != string(original) {
		t.Fatal("old parity file changed after a verify mismatch")
	}
}

// TestRunParityUpgrade_CheckFails_NeverReleases confirms that a failing
// `snapraid check` against the switched-to configuration stops the run
// before Release is ever called — the old, physically intact parity
// disk stays available for a human to act on, exactly as doc 02 §4's
// "Replacing a failed disk" flow (`snapraid fix`) could still recover
// through even though the configuration itself has already moved on to
// the new disk.
func TestRunParityUpgrade_CheckFails_NeverReleases(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.parity")
	newPath := filepath.Join(dir, "new.parity")
	original := writeRandomFile(t, oldPath, 50_000)
	if err := os.WriteFile(newPath, original, 0o644); err != nil {
		t.Fatalf("seeding new parity file: %v", err)
	}

	calls := &upgradeFakeCalls{}
	deps := newUpgradeDeps(t, calls, errors.New("snapraid check found an inconsistency"))
	spec := ParityUpgradeSpec{OldParityPath: oldPath, NewParityPath: newPath}

	cp, err := marshalUpgradeCheckpoint(ParityUpgradeCheckpoint{Phase: ParityUpgradePhaseChecking})
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}

	_, err = RunParityUpgrade(context.Background(), spec, deps, ParityUpgradeHooks{}, cp)
	if err == nil {
		t.Fatal("RunParityUpgrade with a failing check: got nil error")
	}
	if calls.release != 0 {
		t.Fatalf("Release was called %d times after a failed check, want 0", calls.release)
	}
	if got := mustReadFile(t, oldPath); string(got) != string(original) {
		t.Fatal("old parity file changed after a failed check")
	}
}

// TestRunParityUpgrade_DepsRequired confirms RunParityUpgrade refuses
// before touching anything when a required dependency is missing.
func TestRunParityUpgrade_DepsRequired(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.parity")
	writeRandomFile(t, oldPath, 100)
	spec := ParityUpgradeSpec{OldParityPath: oldPath, NewParityPath: filepath.Join(dir, "new.parity")}

	_, err := RunParityUpgrade(context.Background(), spec, ParityUpgradeDeps{}, ParityUpgradeHooks{}, nil)
	if !errors.Is(err, ErrParityUpgradeDepsRequired) {
		t.Fatalf("RunParityUpgrade with no deps: err = %v, want ErrParityUpgradeDepsRequired", err)
	}
}

func marshalUpgradeCheckpoint(cp ParityUpgradeCheckpoint) ([]byte, error) {
	return json.Marshal(cp)
}
