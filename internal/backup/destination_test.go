package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
)

var destinationNow = time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)

func destinationSource(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), archiveName(testInstallation, destinationNow, ReasonNone, 0))
	if err := os.WriteFile(src, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	return src
}

// symlinkedDestination returns a destination directly under poolRoot whose
// directory has been replaced by a symlink to elsewhere, which holds one old
// archive of this installation.
func symlinkedDestination(t *testing.T) (dest Destination, poolRoot, elsewhere, oldArchive string) {
	t.Helper()
	root := t.TempDir()
	elsewhere = filepath.Join(root, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	old := destinationNow.AddDate(0, -3, 0)
	oldArchive = writeArchiveAt(t, elsewhere, old, ReasonNone)
	link := filepath.Join(root, "hoserva-backups")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	return Destination{ID: "pool", Path: link, Enabled: true, Retention: Retention{Daily: 1}}, root, elsewhere, oldArchive
}

func assertUntouched(t *testing.T, dir, archive string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != archive {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%s holds %v, want only %s", dir, names, archive)
	}
}

func TestWriteArchive_RefusesSymlinkedDestination(t *testing.T) {
	dest, poolRoot, elsewhere, old := symlinkedDestination(t)

	err := writeArchiveUnder(poolRoot, dest, destinationSource(t))
	if !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("writeArchive = %v, want beneath.ErrSymlink", err)
	}
	assertUntouched(t, elsewhere, old)
}

func TestWriteArchive_RefusesSymlinkInsideDestinationPath(t *testing.T) {
	dest, poolRoot, elsewhere, old := symlinkedDestination(t)
	dest.Path = filepath.Join(dest.Path, "nested")

	err := writeArchiveUnder(poolRoot, dest, destinationSource(t))
	if !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("writeArchive = %v, want beneath.ErrSymlink", err)
	}
	assertUntouched(t, elsewhere, old)
}

func TestPruneTarget_RefusesSymlinkedDestination(t *testing.T) {
	dest, poolRoot, elsewhere, old := symlinkedDestination(t)
	owner := archiveOwner{installation: testInstallation}
	justWritten := archiveName(testInstallation, destinationNow, ReasonNone, 0)

	err := pruneTarget(context.Background(), localTarget{dest: dest, poolRoot: poolRoot}, owner, dest.Retention, destinationNow, justWritten)
	if !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("pruneTarget = %v, want beneath.ErrSymlink", err)
	}
	assertUntouched(t, elsewhere, old)
}

func TestLocalTarget_RefusesSymlinkedDestination(t *testing.T) {
	dest, poolRoot, elsewhere, old := symlinkedDestination(t)
	target := localTarget{dest: dest, poolRoot: poolRoot}
	ctx := context.Background()

	if _, err := target.list(ctx); !errors.Is(err, beneath.ErrSymlink) {
		t.Errorf("list = %v, want beneath.ErrSymlink", err)
	}
	if _, err := target.files(ctx); !errors.Is(err, beneath.ErrSymlink) {
		t.Errorf("files = %v, want beneath.ErrSymlink", err)
	}
	if _, err := target.readBack(ctx, old); !errors.Is(err, beneath.ErrSymlink) {
		t.Errorf("readBack = %v, want beneath.ErrSymlink", err)
	}
	if err := target.fetch(ctx, old, filepath.Join(t.TempDir(), "out")); !errors.Is(err, beneath.ErrSymlink) {
		t.Errorf("fetch = %v, want beneath.ErrSymlink", err)
	}
	if err := target.remove(ctx, old); !errors.Is(err, beneath.ErrSymlink) {
		t.Errorf("remove = %v, want beneath.ErrSymlink", err)
	}
	assertUntouched(t, elsewhere, old)
}

func TestWriteArchive_RefusesDirectorySwappedForSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "hoserva-backups")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	src := destinationSource(t)
	dest := Destination{ID: "pool", Path: dir, Enabled: true, Retention: Retention{Daily: 1}}
	if err := writeArchiveUnder(root, dest, src); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}

	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	other := writeArchiveAt(t, elsewhere, destinationNow.AddDate(0, -3, 0), ReasonNone)
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}

	if err := writeArchiveUnder(root, dest, src); !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("writeArchive after the swap = %v, want beneath.ErrSymlink", err)
	}
	assertUntouched(t, elsewhere, other)
}

func TestWriteArchive_CreatesMissingDirectoriesPrivately(t *testing.T) {
	root := t.TempDir()
	dest := Destination{ID: "pool", Path: filepath.Join(root, "a", "b"), Enabled: true}
	src := destinationSource(t)

	if err := writeArchiveUnder(root, dest, src); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}
	for _, p := range []string{filepath.Join(root, "a"), dest.Path} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, want 0700", p, info.Mode().Perm())
		}
	}
	got := filepath.Join(dest.Path, filepath.Base(src))
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("archive mode = %v, want 0600", info.Mode().Perm())
	}
	if b, _ := os.ReadFile(got); string(b) != "archive" {
		t.Errorf("archive content = %q", b)
	}
	entries, _ := os.ReadDir(dest.Path)
	if len(entries) != 1 {
		t.Errorf("destination holds %d entries, want only the archive", len(entries))
	}
}

func TestWriteArchive_KeepsExistingDirectoryMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeArchiveUnder(filepath.Dir(dir), Destination{Path: dir}, destinationSource(t)); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 kept", info.Mode().Perm())
	}
}

func TestLocalTarget_MissingDirectoryListsEmptyAndRemovesNothing(t *testing.T) {
	target := localTarget{dest: Destination{Path: filepath.Join(t.TempDir(), "absent")}}
	ctx := context.Background()

	if got, err := target.list(ctx); err != nil || len(got) != 0 {
		t.Errorf("list = %v, %v; want nothing", got, err)
	}
	if got, err := target.files(ctx); err != nil || len(got) != 0 {
		t.Errorf("files = %v, %v; want nothing", got, err)
	}
	if err := target.remove(ctx, "hoserva-config-x.tar.zst"); err != nil {
		t.Errorf("remove = %v, want nil", err)
	}
	if _, err := target.readBack(ctx, "x"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("readBack = %v, want not-exist", err)
	}
}

func TestLocalTarget_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := localTarget{dest: Destination{Path: dir}}
	ctx := context.Background()
	src := destinationSource(t)
	name := filepath.Base(src)

	if err := target.write(ctx, src); err != nil {
		t.Fatal(err)
	}
	listed, err := target.list(ctx)
	if err != nil || len(listed) != 1 || listed[0].name != name {
		t.Fatalf("list = %v, %v", listed, err)
	}
	files, err := target.files(ctx)
	if err != nil || len(files) != 1 || files[0].name != name || files[0].size != int64(len("archive")) {
		t.Fatalf("files = %v, %v", files, err)
	}
	if b, err := target.readBack(ctx, name); err != nil || string(b) != "archive" {
		t.Fatalf("readBack = %q, %v", b, err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := target.fetch(ctx, name, out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "archive" {
		t.Fatalf("fetched %q", b)
	}
	if err := target.remove(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := target.remove(ctx, name); err != nil {
		t.Fatalf("removing an absent file = %v, want nil", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("destination still holds %d entries", len(left))
	}
}

func TestService_RunRefusesPoolDestinationReplacedBySymlink(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)

	poolRoot := filepath.Join(root, "mnt", "user")
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	old := writeArchiveAt(t, elsewhere, destinationNow.AddDate(0, -3, 0), ReasonNone)
	poolDest := filepath.Join(poolRoot, "hoserva-backups")
	if err := os.Symlink(elsewhere, poolDest); err != nil {
		t.Fatal(err)
	}

	svc := &Service{
		DB:      db,
		Paths:   paths,
		Secrets: &FakeSecretSource{Passphrase: "backup-pass", HasPass: true},
		Cipher:  FakeSecretCipher{},
		Destinations: []Destination{
			{ID: "pool", Path: poolDest, Enabled: true, Retention: Retention{Daily: 1}},
		},
		Hostname:    "test-host",
		Version:     "0.0.0-test",
		Now:         func() time.Time { return destinationNow },
		PoolRoot:    poolRoot,
		PoolMounted: func(string) (bool, error) { return true, nil },
	}

	if err := svc.Run(ctx); err == nil {
		t.Fatal("Run succeeded although the pool destination is a symlink")
	}
	assertUntouched(t, elsewhere, old)
}

func TestWriteArchive_RefusesSymlinkAtAnyComponentBelowThePoolRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		link string
		dest string
	}{
		{"first", "a", "a/b/c"},
		{"middle", "a/b", "a/b/c"},
		{"last", "a/b/c", "a/b/c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			elsewhere := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.MkdirAll(elsewhere, 0o755); err != nil {
				t.Fatal(err)
			}
			old := writeArchiveAt(t, elsewhere, destinationNow.AddDate(0, -3, 0), ReasonNone)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, tc.link)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(root, tc.link)); err != nil {
				t.Fatal(err)
			}
			dest := Destination{ID: "pool", Path: filepath.Join(root, tc.dest), Enabled: true, Retention: Retention{Daily: 1}}

			if err := writeArchiveUnder(root, dest, destinationSource(t)); !errors.Is(err, beneath.ErrSymlink) {
				t.Fatalf("writeArchive = %v, want beneath.ErrSymlink", err)
			}
			target := localTarget{dest: dest, poolRoot: root}
			if _, err := target.list(context.Background()); !errors.Is(err, beneath.ErrSymlink) {
				t.Errorf("list = %v, want beneath.ErrSymlink", err)
			}
			err := pruneTarget(context.Background(), target, archiveOwner{installation: testInstallation}, dest.Retention, destinationNow,
				archiveName(testInstallation, destinationNow, ReasonNone, 0))
			if !errors.Is(err, beneath.ErrSymlink) {
				t.Errorf("pruneTarget = %v, want beneath.ErrSymlink", err)
			}
			assertUntouched(t, elsewhere, old)
		})
	}
}

func TestLocalTarget_DestinationOutsideThePoolFollowsASymlinkedAncestor(t *testing.T) {
	base := t.TempDir()
	poolRoot := filepath.Join(base, "pool")
	if err := os.Mkdir(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := filepath.Join(base, "srv")
	if err := os.Symlink(data, srv); err != nil {
		t.Fatal(err)
	}
	dest := Destination{ID: "ext", Path: filepath.Join(srv, "hoserva-backups"), Enabled: true, Retention: Retention{Daily: 1}}
	target := localTarget{dest: dest, poolRoot: poolRoot}
	ctx := context.Background()

	src := destinationSource(t)
	if err := target.write(ctx, src); err != nil {
		t.Fatalf("write = %v, want it to follow the link outside the pool", err)
	}
	real := filepath.Join(data, "hoserva-backups")
	if _, err := os.Stat(filepath.Join(real, filepath.Base(src))); err != nil {
		t.Fatalf("archive not written through the link: %v", err)
	}
	listed, err := target.list(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v, %v", listed, err)
	}
	old := writeArchiveAt(t, real, destinationNow.AddDate(0, -3, 0), ReasonNone)
	err = pruneTarget(ctx, target, archiveOwner{installation: testInstallation}, dest.Retention, destinationNow, filepath.Base(src))
	if err != nil {
		t.Fatalf("pruneTarget = %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, old)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old archive still present after prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, filepath.Base(src))); err != nil {
		t.Errorf("newest archive pruned: %v", err)
	}
}

func TestLocalTarget_RefusesNonRegularArchive(t *testing.T) {
	dir := t.TempDir()
	target := localTarget{dest: Destination{Path: dir}}
	ctx := context.Background()
	src := destinationSource(t)
	name := filepath.Base(src)
	fifo := filepath.Join(dir, name)
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Releases a reader a regression would leave blocked in open.
		if fd, err := unix.Open(fifo, unix.O_RDWR|unix.O_NONBLOCK, 0); err == nil {
			_ = unix.Close(fd)
		}
	})

	within := func(what string, fn func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- fn() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s of a FIFO succeeded, want an error", what)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s of a FIFO blocked", what)
		}
	}
	within("fetch", func() error { return target.fetch(ctx, name, filepath.Join(t.TempDir(), "out")) })
	within("readBack", func() error { _, err := target.readBack(ctx, name); return err })

	other := filepath.Join(t.TempDir(), archiveName(testInstallation, destinationNow.Add(time.Hour), ReasonNone, 0))
	if err := os.WriteFile(other, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- target.write(ctx, other) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("write beside a FIFO = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write beside a FIFO blocked")
	}
}
