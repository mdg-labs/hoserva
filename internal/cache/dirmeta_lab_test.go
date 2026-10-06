//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host, the same way entry_lab_test.go does. It proves what the unit
// tests over a temp directory cannot: that a share relocation in either
// direction recreates a tree's directories with their owner, group and
// mode — here the UID 99, GID 100 and setgid 2775 a migrated share carries
// (doc 05 §4, Q26) — across real XFS filesystems and through the real
// array-only mergerfs mount (#626).

package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/mdg-labs/hoserva/internal/beneath"
	"github.com/mdg-labs/hoserva/internal/parity"
)

const (
	shareUID = 99
	shareGID = 100
)

var shareDirs = map[string]os.FileMode{
	"documents":         os.ModeDir | os.ModeSetgid | 0o775,
	"documents/Reports": os.ModeDir | 0o750,
}

func labBuildShareTree(t *testing.T, root string) {
	t.Helper()
	mustWrite(t, filepath.Join(root, "documents/Reports/Q3.txt"), "quarterly numbers")
	for rel, mode := range shareDirs {
		p := filepath.Join(root, rel)
		if err := os.Chown(p, shareUID, shareGID); err != nil {
			t.Fatalf("chown %s: %v", rel, err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatalf("chmod %s: %v", rel, err)
		}
	}
	if err := os.Chown(filepath.Join(root, "documents/Reports/Q3.txt"), shareUID, shareGID); err != nil {
		t.Fatal(err)
	}
}

func labAssertShareDirs(t *testing.T, root string) {
	t.Helper()
	for rel, want := range shareDirs {
		info, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("lstat %s: %v", filepath.Join(root, rel), err)
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != shareUID || st.Gid != shareGID || info.Mode() != want {
			t.Errorf("%s: owner %d:%d mode %v, want %d:%d mode %v", filepath.Join(root, rel), st.Uid, st.Gid, info.Mode(), shareUID, shareGID, want)
		}
	}
}

func TestLabRelocateToArray_KeepsDirectoryOwnershipAndMode(t *testing.T) {
	top := bringUpLabMoverTopology(t, "dirmetatoarray")
	share := top.cacheShare()
	labBuildShareTree(t, share.CachePath)

	if _, err := RelocateToArray(context.Background(), share, Config{VerifyChecksum: true}, Deps{}, RunHooks{}, nil); err != nil {
		t.Fatalf("RelocateToArray: %v", err)
	}
	landed := 0
	for _, d := range top.dataDisks {
		if _, err := os.Lstat(filepath.Join(d, share.Name, "documents/Reports/Q3.txt")); err != nil {
			continue
		}
		landed++
		labAssertShareDirs(t, filepath.Join(d, share.Name))
	}
	if landed != 1 {
		t.Fatalf("the file landed on %d data disks, want the one mergerfs chose", landed)
	}
}

func TestLabRelocateToCache_KeepsDirectoryOwnershipAndMode(t *testing.T) {
	top := bringUpLabMoverTopology(t, "dirmetatocache")
	share := top.cacheShare()
	labBuildShareTree(t, share.Branches[0])

	deps := Deps{Sync: func(context.Context, []parity.ManifestEntry) error { return nil }}
	if _, err := RelocateToCache(context.Background(), share, Config{VerifyChecksum: true}, deps, RunHooks{}, nil); err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	labAssertShareDirs(t, share.CachePath)
}

// Run as root: a share user who swaps a directory of the target tree for a
// symlink to a host directory must not get a directory created there, owned
// by the share's user, by the copy that runs as root (#626).
func TestLabCopyMoveFile_ASymlinkedParentCreatesNothingOutsideTheTarget(t *testing.T) {
	base := t.TempDir()
	srcRoot, dstRoot, outside := filepath.Join(base, "src"), filepath.Join(base, "dst"), filepath.Join(base, "outside")
	labBuildShareTree(t, srcRoot)
	for _, d := range []string{dstRoot, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dstRoot, "documents")); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcRoot, "documents/Reports/Q3.txt")
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}

	err = copyMoveFile(src, filepath.Join(dstRoot, "documents/Reports/Q3.txt"), dstRoot, info, Config{VerifyChecksum: true}, Deps{}.withDefaults())
	if !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("copyMoveFile through a symlinked parent = %v, want beneath.ErrSymlink", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("%d entries were created outside the target tree", len(entries))
	}
	if _, err := os.Lstat(src); err != nil {
		t.Fatalf("the source was touched: %v", err)
	}
}
