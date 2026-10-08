//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host: the trusted extended-attribute namespace needs root, which the
// lab container has and the host test run does not. It sets, reads and checks
// the real anchor of appdata directories on the lab's cache filesystem, with
// no fake in between.

package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func labAnchor(t *testing.T, dir string) (string, bool) {
	t.Helper()
	buf := make([]byte, unix.PathMax+1)
	n, err := unix.Getxattr(dir, appdataAnchorAttr, buf)
	switch {
	case err == nil:
		return string(buf[:n]), true
	case errors.Is(err, unix.ENODATA):
		return "", false
	default:
		t.Fatalf("reading %s of %s on the lab's cache filesystem: %v", appdataAnchorAttr, dir, err)
		return "", false
	}
}

func requireLabAnchor(t *testing.T, dir, want string) {
	t.Helper()
	if got, ok := labAnchor(t, dir); !ok || got != want {
		t.Fatalf("anchor of %s = %q (set: %v), want %q", dir, got, ok, want)
	}
}

// labRestoreRig is a restore rig whose appdata location is on the lab's cache
// filesystem and whose service reads and writes the real attribute.
func labRestoreRig(t *testing.T) *restoreRig {
	t.Helper()
	cache := filepath.Join(labDir(t), "mnt", "cache")
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("the lab's cache disk %s is not present: %v", cache, err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(filepath.Join(cache, "appdata"))
		_ = os.RemoveAll(filepath.Join(cache, appdataStagingDir))
	})
	rig := newAppdataRigIn(t, cache)
	rig.svc.Attrs = nil
	return newRestoreRigOn(t, rig)
}

func TestLabAppdataAnchor_BackupAnchorsAndARestoreRefusesAnotherAppsDirectory(t *testing.T) {
	rig := labRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	alpha := resolved(t, rig.dir)
	requireLabAnchor(t, rig.dir, alpha)

	other := newOtherAppdata()
	beta := rig.addApp(t, "beta", "radarr", "exited", other.files)
	if err := rig.run(t, "beta"); err != nil {
		t.Fatalf("backup of beta: %v\n%s", err, rig.out)
	}
	betaPath := resolved(t, beta)
	requireLabAnchor(t, beta, betaPath)

	aside := rig.dir + ".aside"
	if err := os.Rename(rig.dir, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(beta, rig.dir); err != nil {
		t.Fatal(err)
	}

	err := rig.restore(t)
	if !errors.Is(err, ErrPreRestoreSnapshot) {
		t.Fatalf("Restore = %v, want ErrPreRestoreSnapshot\n%s", err, rig.out)
	}
	for _, want := range []string{alpha, betaPath} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Restore error %q does not name %s", err, want)
		}
	}
	other.requireIntactAt(t, rig.dir)
	if got := filesUnder(t, rig.dir); strings.Join(got, ",") != "data,sub/state" {
		t.Fatalf("files in the other app's directory = %v", got)
	}
	requireLabAnchor(t, rig.dir, betaPath)
	if got := readFile(t, filepath.Join(aside, "config")); got != "v2-live" {
		t.Fatalf("the directory the restore started from: config = %q, want v2-live", got)
	}
	rig.requireNoPreRestoreSnapshot(t)
	alphaState, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alphaState.State != "running" {
		t.Fatalf("alpha = %s after the refused restore, want it started again", alphaState.State)
	}

	if err := os.Rename(rig.dir, beta); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(aside, rig.dir); err != nil {
		t.Fatal(err)
	}
	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore of the container's own directory: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	requireLabAnchor(t, rig.dir, alpha)
}

func TestLabAppdataAnchor_ARestoreAcceptsAndAnchorsADirectoryWithoutAnAnchor(t *testing.T) {
	rig := labRestoreRig(t)
	if err := unix.Removexattr(rig.dir, appdataAnchorAttr); err != nil {
		t.Fatalf("removing the anchor: %v", err)
	}

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	requireLabAnchor(t, rig.dir, resolved(t, rig.dir))
}
