package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mdg-labs/hoserva/internal/store"
)

// ApplyVerifiedUpdate is hoservad --apply-verified-update: the transient
// unit's payload. It re-hashes the pending .deb against the sidecar
// written after a successful signed-checksum check, optionally restores
// the pre-migration snapshot, then installs the local file with dpkg
// (argv, never a URL). A checksum mismatch installs nothing.
func ApplyVerifiedUpdate(ctx context.Context, pendingDir, dbPath string, restore RestoreFunc, install DebInstaller) error {
	debPath := filepath.Join(pendingDir, pendingDebName)
	hashPath := filepath.Join(pendingDir, pendingHashName)
	wantRaw, err := os.ReadFile(hashPath)
	if err != nil {
		return fmt.Errorf("update: reading pending checksum: %w", err)
	}
	want := strings.TrimSpace(string(wantRaw))
	body, err := os.ReadFile(debPath)
	if err != nil {
		return fmt.Errorf("update: reading pending package: %w", err)
	}
	got := sha256Hex(body)
	if !checksumsMatch(got, want) {
		return fmt.Errorf("%w: pending package %s has checksum %s, sidecar has %s", ErrChecksumMismatch, debPath, got, want)
	}
	snapMarker := filepath.Join(pendingDir, pendingSnapshotName)
	if raw, err := os.ReadFile(snapMarker); err == nil {
		snapPath := strings.TrimSpace(string(raw))
		if restore == nil {
			restore = store.RestoreSnapshot
		}
		if err := restore(ctx, dbPath, snapPath); err != nil {
			return fmt.Errorf("update: restoring pre-migration snapshot: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("update: reading snapshot marker: %w", err)
	}
	if install == nil {
		install = dpkgInstall
	}
	return install(ctx, debPath)
}

// RestoreFunc restores snapshotPath over liveDB. Tests inject a recorder.
type RestoreFunc func(ctx context.Context, liveDB, snapshotPath string) error

// DebInstaller installs a local .deb by path. Tests inject a recorder.
type DebInstaller func(ctx context.Context, debPath string) error

func dpkgInstall(ctx context.Context, debPath string) error {
	cmd := exec.CommandContext(ctx, "dpkg", "-i", debPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("update: dpkg -i %s: %w: %s", debPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}
