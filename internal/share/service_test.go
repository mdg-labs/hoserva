package share

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

func TestCreate_MakesBranchDirsAndMounts(t *testing.T) {
	ctx, svc, layout, mounter := testService(t)
	got, err := svc.Create(ctx, CreateInput{
		Name:      "media",
		CacheMode: pool.CacheThenMove,
		SMB:       &SMB{Enabled: true, Browseable: true, Guest: true},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Path() != pool.SharePath("media") {
		t.Fatalf("path = %s", got.Path())
	}
	for _, dir := range []string{
		filepath.Join(layout.cache, "media"),
		filepath.Join(layout.dataDisks[0], "media"),
		filepath.Join(layout.dataDisks[1], "media"),
	} {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Fatalf("branch dir %s: %v", dir, err)
		}
	}
	if len(mounter.mounts) < 1 {
		t.Fatal("Create did not mount the share")
	}
	foundShare, foundMover := false, false
	for _, w := range mounter.mounts {
		if w == pool.SharePath("media") {
			foundShare = true
		}
		if w == pool.MoverTargetPath("media") {
			foundMover = true
		}
	}
	if !foundShare || !foundMover {
		t.Fatalf("mounts = %v, want share and mover target", mounter.mounts)
	}

	smbPath := filepath.Join(svc.Gen.Root, "samba", "smb.conf")
	body, err := os.ReadFile(smbPath)
	if err != nil {
		t.Fatalf("reading generated smb.conf: %v", err)
	}
	if !strings.Contains(string(body), "[media]") || !strings.Contains(string(body), "guest ok = yes") {
		t.Fatalf("smb.conf missing guest share:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(body)), "include = /etc/hoserva/smb.custom.conf") {
		t.Fatalf("smb.conf must end with the user include:\n%s", body)
	}
}

func TestCreate_CacheOnlyHasNoMoverMount(t *testing.T) {
	ctx, svc, layout, mounter := testService(t)
	if _, err := svc.Create(ctx, CreateInput{Name: "appdata", CacheMode: pool.CacheOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(layout.cache, "appdata")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(layout.dataDisks[0], "appdata")); !os.IsNotExist(err) {
		t.Fatal("cache-only share must not mkdir on data disks")
	}
	for _, w := range mounter.mounts {
		if w == pool.MoverTargetPath("appdata") {
			t.Fatal("cache-only share must not mount a mover target")
		}
	}
}

func TestDelete_RemovesDefinitionLeavesData(t *testing.T) {
	ctx, svc, layout, mounter := testService(t)
	writeFile(t, filepath.Join(layout.dataDisks[0], "media", "film.mkv"), "movie")
	createTestShare(t, svc, "media", pool.ArrayOnly)

	if err := svc.Delete(ctx, "media", false); err == nil {
		t.Fatal("delete without confirm must fail")
	}
	if err := svc.Delete(ctx, "media", true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, "media"); !errors.Is(err, store.ErrShareNotFound) {
		t.Fatalf("Get after definition delete = %v, want not found", err)
	}
	if got := readFile(t, filepath.Join(layout.dataDisks[0], "media", "film.mkv")); got != "movie" {
		t.Fatalf("data after definition delete = %q", got)
	}
	foundUnmount := false
	for _, w := range mounter.unmounts {
		if w == pool.SharePath("media") {
			foundUnmount = true
		}
	}
	if !foundUnmount {
		t.Fatalf("unmounts = %v, want share path", mounter.unmounts)
	}
}

func TestBrowse_ReturnsHoldingDisk(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	writeFile(t, filepath.Join(shareRoot, "film.mkv"), "movie")
	if err := os.Mkdir(filepath.Join(shareRoot, "shows"), 0o755); err != nil {
		t.Fatal(err)
	}
	holding := filepath.Join(layout.dataDisks[0], "media")
	svc.FS = xattrFS{xattr: map[string]string{
		filepath.Join(shareRoot, "film.mkv"): holding,
	}}

	rel, entries, err := svc.Browse(ctx, "media", "")
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if rel != "" {
		t.Fatalf("rel = %q", rel)
	}
	byName := map[string]BrowseEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	film, ok := byName["film.mkv"]
	if !ok || film.Directory || film.Disk != holding || film.SizeBytes != 5 {
		t.Fatalf("film = %+v", film)
	}
	shows, ok := byName["shows"]
	if !ok || !shows.Directory {
		t.Fatalf("shows = %+v", shows)
	}

	if _, _, err := svc.Browse(ctx, "media", "../appdata"); err == nil {
		t.Fatal("browse must refuse path escape")
	}
}

func TestCreate_DuplicateName(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	if _, err := svc.Create(ctx, CreateInput{Name: "media", CacheMode: pool.ArrayOnly}); err == nil {
		t.Fatal("duplicate create must fail")
	}
}

func TestCreate_TimeMachineRequiresMaxSize(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	_, err := svc.Create(ctx, CreateInput{
		Name:      "tm",
		CacheMode: pool.ArrayOnly,
		SMB:       &SMB{Enabled: true, TimeMachine: true},
	})
	if err == nil {
		t.Fatal("Time Machine without max size must fail (Q73)")
	}
}
