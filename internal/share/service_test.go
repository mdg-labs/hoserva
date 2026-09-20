package share

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/config"
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

	exportsPath := filepath.Join(svc.Gen.Root, "exports")
	exports, err := os.ReadFile(exportsPath)
	if err != nil {
		t.Fatalf("reading generated exports: %v", err)
	}
	if strings.Contains(string(exports), "/mnt/user/media") {
		t.Fatalf("NFS-disabled share must not appear in exports:\n%s", exports)
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

func TestBrowse_RefusesSymlinkEscape(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	if err := os.MkdirAll(shareRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(shareRoot, "external")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Browse(ctx, "media", "external"); !errors.Is(err, ErrPathEscapes) {
		t.Fatalf("browse through symlink = %v, want ErrPathEscapes", err)
	}
}

func TestCreate_ApplyFailureRollsBackRow(t *testing.T) {
	ctx, svc, _, mounter := testService(t)
	mounter.mountErr = errors.New("mount failed")
	if _, err := svc.Create(ctx, CreateInput{Name: "media", CacheMode: pool.ArrayOnly}); err == nil {
		t.Fatal("Create must fail when apply fails")
	}
	if _, err := svc.Get(ctx, "media"); !errors.Is(err, store.ErrShareNotFound) {
		t.Fatalf("Get after failed create = %v, want not found", err)
	}
}

func TestCreate_ApplyFailureRestoresGeneratedFiles(t *testing.T) {
	ctx, svc, _, mounter := testService(t)
	createTestShare(t, svc, "existing", pool.ArrayOnly)
	before := snapshotGenerated(t, svc.Gen.Root)
	mounter.mountErr = errors.New("mount failed")
	if _, err := svc.Create(ctx, CreateInput{Name: "media", CacheMode: pool.ArrayOnly}); err == nil {
		t.Fatal("Create must fail when apply fails")
	}
	if _, err := svc.Get(ctx, "media"); !errors.Is(err, store.ErrShareNotFound) {
		t.Fatalf("Get after failed create = %v, want not found", err)
	}
	after := snapshotGenerated(t, svc.Gen.Root)
	assertGeneratedUnchanged(t, before, after)
}

func TestCreate_ExistingHostExportsDoesNotWriteSiblings(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	exportsPath := filepath.Join(svc.Gen.Root, config.PathNFS)
	original := "/export/media *(ro,sync,no_subtree_check)\n"
	if err := os.MkdirAll(svc.Gen.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exportsPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(ctx, CreateInput{Name: "media", CacheMode: pool.ArrayOnly})
	if !errors.Is(err, config.ErrExistingHostFile) {
		t.Fatalf("Create = %v, want ErrExistingHostFile", err)
	}
	if _, err := svc.Get(ctx, "media"); !errors.Is(err, store.ErrShareNotFound) {
		t.Fatalf("Get after refused create = %v, want not found", err)
	}
	if _, err := os.Stat(filepath.Join(svc.Gen.Root, config.PathSamba)); !os.IsNotExist(err) {
		t.Fatal("smb.conf must not be written when exports is unimported")
	}
	if _, err := os.Stat(filepath.Join(svc.Gen.Root, "systemd", "system")); !os.IsNotExist(err) {
		t.Fatal("pool mount units must not be written when exports is unimported")
	}
	got, err := os.ReadFile(exportsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("unimported exports changed:\n%s", got)
	}
}

func TestUpdate_ApplyFailureRestoresPreviousRow(t *testing.T) {
	ctx, svc, _, mounter := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	mounter.mountErr = errors.New("mount failed")
	mode := pool.CacheThenMove
	if _, err := svc.Update(ctx, "media", UpdateInput{CacheMode: &mode}); err == nil {
		t.Fatal("Update must fail when apply fails")
	}
	got, err := svc.Get(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheMode != pool.ArrayOnly {
		t.Fatalf("cache mode after failed update = %q, want array-only", got.CacheMode)
	}
}

func TestUpdate_UnmanagedExportsLeavesSMBAndMountsUnchanged(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	created, err := svc.Create(ctx, CreateInput{
		Name:      "media",
		CacheMode: pool.ArrayOnly,
		SMB:       &SMB{Enabled: true, Browseable: true, Guest: false},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.SMB.Guest {
		t.Fatal("fixture share must start with guest disabled")
	}
	if err := svc.Gen.KeepUnmanaged(ctx, config.PathNFS); err != nil {
		t.Fatalf("KeepUnmanaged exports: %v", err)
	}

	before := snapshotGenerated(t, svc.Gen.Root)
	if _, ok := before[config.PathSamba]; !ok {
		t.Fatal("expected smb.conf after create")
	}
	if strings.Contains(before[config.PathSamba], "guest ok = yes") {
		t.Fatalf("pre-update smb.conf already has guest ok:\n%s", before[config.PathSamba])
	}

	guest := SMB{Enabled: true, Browseable: true, Guest: true}
	_, err = svc.Update(ctx, "media", UpdateInput{SMB: &guest})
	if !errors.Is(err, config.ErrUnmanaged) && !errors.Is(err, config.ErrExistingHostFile) {
		t.Fatalf("Update = %v, want ErrUnmanaged or ErrExistingHostFile", err)
	}

	got, err := svc.Get(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if got.SMB.Guest {
		t.Fatal("share row must stay at the previous SMB settings")
	}

	after := snapshotGenerated(t, svc.Gen.Root)
	assertGeneratedUnchanged(t, before, after)
}

func TestUpdate_ApplyFailureRestoresGeneratedFiles(t *testing.T) {
	ctx, svc, _, mounter := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	before := snapshotGenerated(t, svc.Gen.Root)
	mounter.mountErr = errors.New("mount failed")
	mode := pool.CacheThenMove
	if _, err := svc.Update(ctx, "media", UpdateInput{CacheMode: &mode}); err == nil {
		t.Fatal("Update must fail when apply fails")
	}
	got, err := svc.Get(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheMode != pool.ArrayOnly {
		t.Fatalf("cache mode after failed update = %q, want array-only", got.CacheMode)
	}
	after := snapshotGenerated(t, svc.Gen.Root)
	assertGeneratedUnchanged(t, before, after)
}

func TestDelete_UnmanagedExportsRestoresRowAndFiles(t *testing.T) {
	ctx, svc, _, mounter := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	if err := svc.Gen.KeepUnmanaged(ctx, config.PathNFS); err != nil {
		t.Fatalf("KeepUnmanaged exports: %v", err)
	}
	before := snapshotGenerated(t, svc.Gen.Root)
	unmountsBefore := len(mounter.unmounts)
	if err := svc.Delete(ctx, "media", true); !errors.Is(err, config.ErrUnmanaged) && !errors.Is(err, config.ErrExistingHostFile) {
		t.Fatalf("Delete = %v, want ErrUnmanaged or ErrExistingHostFile", err)
	}
	if _, err := svc.Get(ctx, "media"); err != nil {
		t.Fatalf("definition should remain after refused delete: %v", err)
	}
	if len(mounter.unmounts) != unmountsBefore {
		t.Fatalf("refused delete unmounted %v", mounter.unmounts[unmountsBefore:])
	}
	after := snapshotGenerated(t, svc.Gen.Root)
	assertGeneratedUnchanged(t, before, after)
}

func TestUpdate_ApplyFailureRemountsPrevious(t *testing.T) {
	ctx, svc, _, mounter := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	mounter.failMounts = 1
	mode := pool.CacheThenMove
	if _, err := svc.Update(ctx, "media", UpdateInput{CacheMode: &mode}); err == nil {
		t.Fatal("Update must fail when apply fails")
	}
	got, err := svc.Get(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheMode != pool.ArrayOnly {
		t.Fatalf("cache mode after failed update = %q, want array-only", got.CacheMode)
	}
	shareMounts, moverMounts := 0, 0
	for _, w := range mounter.mounts {
		if w == pool.SharePath("media") {
			shareMounts++
		}
		if w == pool.MoverTargetPath("media") {
			moverMounts++
		}
	}
	if shareMounts < 2 || moverMounts < 2 {
		t.Fatalf("rollback must remount previous share and mover topology, mounts=%v", mounter.mounts)
	}
}

func TestUpdate_CacheOnlyRollbackUnmountsMover(t *testing.T) {
	ctx, svc, _, mounter := testService(t)
	createTestShare(t, svc, "media", pool.CacheOnly)
	mounter.failMounts = 1
	mode := pool.CacheThenMove
	if _, err := svc.Update(ctx, "media", UpdateInput{CacheMode: &mode}); err == nil {
		t.Fatal("Update must fail when apply fails")
	}
	got, err := svc.Get(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheMode != pool.CacheOnly {
		t.Fatalf("cache mode after failed update = %q, want cache-only", got.CacheMode)
	}
	foundMoverUnmount := false
	for _, w := range mounter.unmounts {
		if w == pool.MoverTargetPath("media") {
			foundMoverUnmount = true
		}
	}
	if !foundMoverUnmount {
		t.Fatalf("restoring cache-only must drop the mover mount, unmounts=%v mounts=%v", mounter.unmounts, mounter.mounts)
	}
}

func TestUpdate_CacheOnlyUnmountsMoverTarget(t *testing.T) {
	ctx, svc, _, mounter := testService(t)
	createTestShare(t, svc, "media", pool.CacheThenMove)
	mode := pool.CacheOnly
	if _, err := svc.Update(ctx, "media", UpdateInput{CacheMode: &mode}); err != nil {
		t.Fatalf("Update to cache-only: %v", err)
	}
	found := false
	for _, w := range mounter.unmounts {
		if w == pool.MoverTargetPath("media") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unmounts = %v, want mover target", mounter.unmounts)
	}
}

func TestDelete_UnmountFailureLeavesDefinition(t *testing.T) {
	ctx, svc, _, mounter := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	mounter.unmountErr = errors.New("target is busy")
	if err := svc.Delete(ctx, "media", true); err == nil {
		t.Fatal("Delete must fail when unmount fails")
	}
	if _, err := svc.Get(ctx, "media"); err != nil {
		t.Fatalf("definition should remain after unmount failure: %v", err)
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

func TestCreate_NFSWritesExports(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	got, err := svc.Create(ctx, CreateInput{
		Name:      "media",
		CacheMode: pool.ArrayOnly,
		NFS: &NFS{
			Enabled: true,
			Hosts:   []string{"192.168.1.0/24", "10.0.0.5"},
			Squash:  "root_squash",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !got.NFS.Enabled || len(got.NFS.Hosts) != 2 {
		t.Fatalf("NFS = %+v", got.NFS)
	}
	body, err := os.ReadFile(filepath.Join(svc.Gen.Root, "exports"))
	if err != nil {
		t.Fatalf("reading generated exports: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "/mnt/user/media") || !strings.Contains(text, "192.168.1.0/24(rw,sync,no_subtree_check,root_squash)") {
		t.Fatalf("exports missing share line:\n%s", text)
	}
}

func TestCreate_InvalidNFSHost(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	for _, host := range []string{"*", "not a host", "192.168.1.0/99", "$(reboot)", "-bad.example"} {
		_, err := svc.Create(ctx, CreateInput{
			Name:      "media",
			CacheMode: pool.ArrayOnly,
			NFS:       &NFS{Enabled: true, Hosts: []string{host}, Squash: "root_squash"},
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("host %q: err = %v, want ErrInvalidInput", host, err)
		}
	}
}

func TestCreate_NFSEnabledRequiresHost(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	_, err := svc.Create(ctx, CreateInput{
		Name:      "media",
		CacheMode: pool.ArrayOnly,
		NFS:       &NFS{Enabled: true, Squash: "root_squash"},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("enabled NFS without hosts = %v, want ErrInvalidInput", err)
	}
}
