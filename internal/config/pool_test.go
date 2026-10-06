package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config/golden"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"
)

// poolMountTestdataDir is package-owned (internal/config/testdata/pool-mounts),
// mirroring internal/pool/testdata/pool-mounts's own precedent: the shared
// testdata/configs/ root is walked unconditionally by TestRenderSnapraidConf
// for SnapraidState's own shape, so a state.json in PoolState's shape
// collides with it there (#141).
const poolMountTestdataDir = "testdata/pool-mounts"

func loadPoolState(t *testing.T) PoolState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(poolMountTestdataDir, "state.json"))
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}
	var state PoolState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("parsing state.json: %v", err)
	}
	return state
}

// TestWritePoolMounts exercises WritePoolMounts end to end: given a small
// pool state, it writes the catch-all, each share's own mount, each
// non-cache-only share's mover write target and the branch bind of every
// data disk those use, and every written file matches a checked-in golden
// fixture (CLAUDE.md: "Golden files change only deliberately").
func TestWritePoolMounts(t *testing.T) {
	state := loadPoolState(t)
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)
	const revision = 7
	const command = "array start"

	if err := g.WritePoolMounts(ctx, state, command, revision, now); err != nil {
		t.Fatalf("WritePoolMounts: %v", err)
	}

	wheres := []string{
		pool.CatchAllPath,
		pool.SharePath("movies"),
		pool.MoverTargetPath("movies"),
		pool.SharePath("appdata"),
		disk.BranchBindFor("/mnt/disk1").Where,
		disk.BranchBindFor("/mnt/disk2").Where,
	}
	for _, where := range wheres {
		path := mountUnitPath(where)
		got, err := os.ReadFile(filepath.Join(g.Root, path))
		if err != nil {
			t.Fatalf("reading written unit for %s: %v", where, err)
		}
		golden.Compare(t, filepath.Join(poolMountTestdataDir, unitFileName(where)+".golden"), got)
	}

	// appdata is cache-only: no mover write target exists for it
	// (pool/share.go), so WritePoolMounts must not have written one.
	if _, err := os.Stat(filepath.Join(g.Root, mountUnitPath(pool.MoverTargetPath("appdata")))); !os.IsNotExist(err) {
		t.Fatalf("mover target mount written for cache-only share appdata: err = %v", err)
	}
}

// TestWritePoolMountsBodyMatchesRenderExactly is doc 09 §2's "one placement
// algorithm" made concrete: the body WritePoolMounts writes for the
// catch-all is exactly Mount.Render()'s own output, with only the doc 01
// §2 header added around it — WritePoolMounts never reformats or
// recomputes what internal/pool already computed.
func TestWritePoolMountsBodyMatchesRenderExactly(t *testing.T) {
	state := loadPoolState(t)
	g := NewGenerator(t.TempDir())
	g.StoppedFlagPath = "/srv/hoserva-state/array-stopped"
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)
	const revision = 3
	const command = "array start"

	if err := g.WritePoolMounts(ctx, state, command, revision, now); err != nil {
		t.Fatalf("WritePoolMounts: %v", err)
	}

	catchAll, err := pool.CatchAllMount(state.DataDisks, state.Options)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(g.Root, mountUnitPath(catchAll.Where)))
	if err != nil {
		t.Fatalf("reading written catch-all unit: %v", err)
	}

	want := Header(command, revision, now) + catchAll.Render(g.StoppedFlagPath)
	if string(got) != want {
		t.Fatalf("catch-all unit mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestWritePoolMounts_ForwardsCatchAllCreatePolicy(t *testing.T) {
	state := loadPoolState(t)
	state.CreatePolicy = pool.BalanceAcrossDisks
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)

	if err := g.WritePoolMounts(ctx, state, "array create", 1, now); err != nil {
		t.Fatalf("WritePoolMounts: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(g.Root, mountUnitPath(pool.CatchAllPath)))
	if err != nil {
		t.Fatalf("reading catch-all unit: %v", err)
	}
	if !strings.Contains(string(got), "category.create=mfs") {
		t.Fatalf("catch-all unit missing selected create policy:\n%s", got)
	}
	if strings.Contains(string(got), "category.create=mspmfs") {
		t.Fatalf("catch-all unit still uses the default create policy:\n%s", got)
	}
}

// TestWritePoolMounts_RemovingDiskMarksOnlyThatDiskNC is #359's own
// acceptance test for the catch-all and mover-target halves of doc 09 §4
// step 2: with state.RemovingDisk set, every mount WritePoolMounts writes
// must carry that disk's own branch as NC, and every other disk RW —
// proven against the exact units the generator wrote, not against the
// pool.*MountRemoving builders directly (internal/pool/topology_test.go
// already covers those in isolation). movies is cache-then-move, so its
// own share unit's data-disk branches are already NC regardless of
// RemovingDisk (ShareMountRemoving's own doc comment) — its mover target
// is the one unit this fixture can show the switch on.
func TestWritePoolMounts_RemovingDiskMarksOnlyThatDiskNC(t *testing.T) {
	state := loadPoolState(t)
	state.RemovingDisk = "/mnt/disk1"
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)

	if err := g.WritePoolMounts(ctx, state, "array start", 1, now); err != nil {
		t.Fatalf("WritePoolMounts: %v", err)
	}

	catchAll, err := os.ReadFile(filepath.Join(g.Root, mountUnitPath(pool.CatchAllPath)))
	if err != nil {
		t.Fatalf("reading catch-all unit: %v", err)
	}
	if !strings.Contains(string(catchAll), "/mnt/disk1=NC") {
		t.Fatalf("catch-all unit missing /mnt/disk1=NC:\n%s", catchAll)
	}
	if !strings.Contains(string(catchAll), "/mnt/disk2=RW") {
		t.Fatalf("catch-all unit's other disk must stay RW:\n%s", catchAll)
	}

	mover, err := os.ReadFile(filepath.Join(g.Root, mountUnitPath(pool.MoverTargetPath("movies"))))
	if err != nil {
		t.Fatalf("reading movies mover-target unit: %v", err)
	}
	if !strings.Contains(string(mover), "/run/hoserva/branches/mnt/disk1/movies=NC") {
		t.Fatalf("movies mover-target unit missing disk1's branch as NC:\n%s", mover)
	}
	if !strings.Contains(string(mover), "/run/hoserva/branches/mnt/disk2/movies=RW") {
		t.Fatalf("movies mover-target unit's other disk must stay RW:\n%s", mover)
	}
}

// TestWritePoolMounts_KeepsABindUnitForEveryDataDiskUntilItsMountUnitGoes:
// a branch bind's unit is what binds a mounted bind to its disk (BindsTo=),
// so it is written for every data disk of the pool whether or not a mover
// target uses it — with no share at all, after the last share is deleted,
// once every share is cache-only — and it outlives a disk leaving the
// pool. Only RemoveDiskMount, which the removal job calls once the disk and
// its bind are unmounted, removes it. Removing it while the bind is still
// mounted would leave the disk's filesystem mounted past array stop once
// systemd reloads (#656).
func TestWritePoolMounts_KeepsABindUnitForEveryDataDiskUntilItsMountUnitGoes(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)
	bind1 := mountUnitPath(disk.BranchBindFor("/mnt/disk1").Where)
	bind2 := mountUnitPath(disk.BranchBindFor("/mnt/disk2").Where)
	bound := func(p, diskUnit string) bool {
		body, err := os.ReadFile(filepath.Join(g.Root, p))
		return err == nil && strings.Contains(string(body), "BindsTo="+diskUnit+"\n")
	}
	assertBinds := func(step string, want1, want2 bool) {
		t.Helper()
		if got1, got2 := bound(bind1, "mnt-disk1.mount"), bound(bind2, "mnt-disk2.mount"); got1 != want1 || got2 != want2 {
			t.Fatalf("%s: disk1's bind unit %v, disk2's %v — want %v, %v", step, got1, got2, want1, want2)
		}
	}

	state := PoolState{DataDisks: []string{"/mnt/disk1", "/mnt/disk2"}, CachePath: "/mnt/cache"}
	if err := g.WritePoolMounts(ctx, state, "array start", 1, now); err != nil {
		t.Fatalf("WritePoolMounts: %v", err)
	}
	assertBinds("no share", true, true)

	state.Shares = []PoolShare{{Name: "movies", CacheMode: pool.CacheThenMove, CreatePolicy: pool.KeepFoldersTogether}}
	if err := g.WritePoolMounts(ctx, state, "share", 2, now); err != nil {
		t.Fatalf("WritePoolMounts (a share): %v", err)
	}
	assertBinds("a moved share", true, true)

	state.Shares[0].CacheMode = pool.CacheOnly
	if err := g.WritePoolMounts(ctx, state, "share", 3, now); err != nil {
		t.Fatalf("WritePoolMounts (cache-only): %v", err)
	}
	assertBinds("every share cache-only", true, true)

	state.Shares = nil
	if err := g.WritePoolMounts(ctx, state, "share", 4, now); err != nil {
		t.Fatalf("WritePoolMounts (last share deleted): %v", err)
	}
	assertBinds("the last share deleted", true, true)

	state.DataDisks = []string{"/mnt/disk1"}
	if err := g.WritePoolMounts(ctx, state, "array start", 5, now); err != nil {
		t.Fatalf("WritePoolMounts (disk2 left the pool): %v", err)
	}
	assertBinds("disk2 left the pool", true, true)

	if err := g.RemoveDiskMount(ctx, "/mnt/disk2"); err != nil {
		t.Fatalf("RemoveDiskMount: %v", err)
	}
	assertBinds("disk2's mount unit removed", true, false)
	if err := g.RemoveDiskMount(ctx, "/mnt/disk2"); err != nil {
		t.Fatalf("RemoveDiskMount again: %v", err)
	}
}

// A bind unit taken over by hand is kept, and the disk's removal refused,
// as for the disk's own unit.
func TestRemoveDiskMount_RefusesAnUnmanagedBindUnit(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	state := PoolState{DataDisks: []string{"/mnt/disk1"}}
	if err := g.WritePoolMounts(ctx, state, "array start", 1, time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)); err != nil {
		t.Fatalf("WritePoolMounts: %v", err)
	}
	bind := mountUnitPath(disk.BranchBindFor("/mnt/disk1").Where)
	if err := g.KeepUnmanaged(ctx, bind); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}
	if err := g.RemoveDiskMount(ctx, "/mnt/disk1"); err == nil {
		t.Fatal("RemoveDiskMount = nil, want a refusal for the hand-kept bind unit")
	}
	if _, err := os.Stat(filepath.Join(g.Root, bind)); err != nil {
		t.Fatalf("the hand-kept bind unit is gone: %v", err)
	}
}

func TestCanWriteShareFiles_RefusesAnUnmanagedBindUnit(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	state := PoolState{
		DataDisks: []string{"/mnt/disk1"},
		Shares:    []PoolShare{{Name: "media", CacheMode: pool.ArrayOnly, CreatePolicy: pool.KeepFoldersTogether}},
	}
	if err := g.WritePoolMounts(ctx, state, "share", 1, now); err != nil {
		t.Fatalf("WritePoolMounts: %v", err)
	}
	if err := g.KeepUnmanaged(ctx, mountUnitPath(disk.BranchBindFor("/mnt/disk1").Where)); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}
	if err := g.CanWriteShareFiles(ctx, state); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("CanWriteShareFiles = %v, want ErrUnmanaged", err)
	}
}

// TestWritePoolMounts_NoRemovingDisk_MatchesGoldenFiles proves the flip
// side of the test above: with RemovingDisk left empty — every existing
// caller before #359 — WritePoolMounts's own output is unaffected. This
// is TestWritePoolMounts itself, so a regression here already fails that
// test's own golden comparison; this test exists only to name the
// invariant explicitly for anyone reading this file for #359's own
// change.
func TestWritePoolMounts_NoRemovingDisk_MatchesGoldenFiles(t *testing.T) {
	state := loadPoolState(t)
	if state.RemovingDisk != "" {
		t.Fatalf("fixture state.json unexpectedly sets removing_disk = %q", state.RemovingDisk)
	}
}

// TestUnitFileName_EscapesLiteralHyphens is systemd's own path-escaping
// rule (systemd-escape --path): ValidateShareName allows a hyphen in a
// share name, so a share mounted at "/mnt/user/tv-shows" must not collide
// with the unit name a path with an extra "/" segment would produce — the
// literal hyphen has to be escaped as \x2d, distinct from the "-" a "/"
// becomes.
func TestUnitFileName_EscapesLiteralHyphens(t *testing.T) {
	got := unitFileName("/mnt/user/tv-shows")
	want := `mnt-user-tv\x2dshows.mount`
	if got != want {
		t.Fatalf("unitFileName(/mnt/user/tv-shows): got %q, want %q", got, want)
	}
}

// TestWritePoolMounts_RemovesUnitForARemovedShare is doc 01 §2's own
// drift-free promise made concrete for pool mounts: a share dropped from
// state must not leave its old mount unit (and, since it had a
// non-cache-only mode, its old mover target unit) behind on the next
// regeneration.
func TestWritePoolMounts_RemovesUnitForARemovedShare(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)

	full := PoolState{
		DataDisks: []string{"/mnt/disk1", "/mnt/disk2"},
		CachePath: "/mnt/cache",
		Shares: []PoolShare{
			{Name: "movies", CacheMode: pool.CacheThenMove, CreatePolicy: pool.KeepFoldersTogether},
		},
	}
	if err := g.WritePoolMounts(ctx, full, "array start", 1, now); err != nil {
		t.Fatalf("WritePoolMounts (full): %v", err)
	}

	shareUnit := mountUnitPath(pool.SharePath("movies"))
	moverUnit := mountUnitPath(pool.MoverTargetPath("movies"))
	for _, p := range []string{shareUnit, moverUnit} {
		if _, err := os.Stat(filepath.Join(g.Root, p)); err != nil {
			t.Fatalf("expected %s to exist after the first write: %v", p, err)
		}
	}

	shrunk := PoolState{
		DataDisks: full.DataDisks,
		CachePath: full.CachePath,
	}
	if err := g.WritePoolMounts(ctx, shrunk, "array start", 2, now); err != nil {
		t.Fatalf("WritePoolMounts (shrunk): %v", err)
	}

	for _, p := range []string{shareUnit, moverUnit} {
		if _, err := os.Stat(filepath.Join(g.Root, p)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed once movies left state, got err = %v", p, err)
		}
		if status, err := g.Check(ctx, p); err != nil || status != StatusUnknown {
			t.Fatalf("Check(%s) after removal: got (%v, %v), want (StatusUnknown, nil)", p, status, err)
		}
	}

	// The catch-all itself, still desired, must survive.
	if _, err := os.Stat(filepath.Join(g.Root, mountUnitPath(pool.CatchAllPath))); err != nil {
		t.Fatalf("catch-all unit missing after reconciliation: %v", err)
	}
}

// TestWritePoolMounts_RemovesMoverUnitOnCacheOnlyTransition covers the
// other reconciliation case CodeRabbit's review named: a share that moves
// to CacheOnly keeps its own share mount but must lose the mover
// write-target unit it no longer has (pool/share.go: CacheOnly data is
// never moved).
func TestWritePoolMounts_RemovesMoverUnitOnCacheOnlyTransition(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)

	state := PoolState{
		DataDisks: []string{"/mnt/disk1", "/mnt/disk2"},
		CachePath: "/mnt/cache",
		Shares: []PoolShare{
			{Name: "movies", CacheMode: pool.CacheThenMove, CreatePolicy: pool.KeepFoldersTogether},
		},
	}
	if err := g.WritePoolMounts(ctx, state, "array start", 1, now); err != nil {
		t.Fatalf("WritePoolMounts (cache-then-move): %v", err)
	}

	shareUnit := mountUnitPath(pool.SharePath("movies"))
	moverUnit := mountUnitPath(pool.MoverTargetPath("movies"))

	state.Shares[0].CacheMode = pool.CacheOnly
	if err := g.WritePoolMounts(ctx, state, "array start", 2, now); err != nil {
		t.Fatalf("WritePoolMounts (cache-only): %v", err)
	}

	if _, err := os.Stat(filepath.Join(g.Root, shareUnit)); err != nil {
		t.Fatalf("share unit missing after transitioning to cache-only: %v", err)
	}
	if _, err := os.Stat(filepath.Join(g.Root, moverUnit)); !os.IsNotExist(err) {
		t.Fatalf("expected mover unit to be removed after transitioning to cache-only, got err = %v", err)
	}
}

// TestWritePoolMounts_ReconciliationPreservesUnmanagedUnits is
// RemoveManaged's own safety property, exercised through WritePoolMounts:
// a unit a human took over with KeepUnmanaged must survive even after its
// share leaves state, exactly like Write already refuses to overwrite it.
func TestWritePoolMounts_ReconciliationPreservesUnmanagedUnits(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)

	state := PoolState{
		DataDisks: []string{"/mnt/disk1", "/mnt/disk2"},
		CachePath: "/mnt/cache",
		Shares: []PoolShare{
			{Name: "movies", CacheMode: pool.ArrayOnly, CreatePolicy: pool.KeepFoldersTogether},
		},
	}
	if err := g.WritePoolMounts(ctx, state, "array start", 1, now); err != nil {
		t.Fatalf("WritePoolMounts: %v", err)
	}

	shareUnit := mountUnitPath(pool.SharePath("movies"))
	if err := g.KeepUnmanaged(ctx, shareUnit); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}

	state.Shares = nil
	if err := g.WritePoolMounts(ctx, state, "array start", 2, now); err != nil {
		t.Fatalf("WritePoolMounts (shares removed): %v", err)
	}

	if _, err := os.Stat(filepath.Join(g.Root, shareUnit)); err != nil {
		t.Fatalf("unmanaged share unit was removed by reconciliation: %v", err)
	}
	if status, err := g.Check(ctx, shareUnit); err != nil || status != StatusUnmanaged {
		t.Fatalf("Check(%s): got (%v, %v), want (StatusUnmanaged, nil)", shareUnit, status, err)
	}
}

func TestCanWriteShareFiles_RefusesUnmanagedExports(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	state := PoolState{
		DataDisks: []string{"/mnt/disk1"},
		CachePath: "/mnt/cache",
		Shares: []PoolShare{
			{Name: "media", CacheMode: pool.ArrayOnly, CreatePolicy: pool.KeepFoldersTogether},
		},
	}
	if err := g.WritePoolMounts(ctx, state, "share", 1, now); err != nil {
		t.Fatalf("WritePoolMounts: %v", err)
	}
	if err := g.WriteSamba(ctx, []SambaShare{{Name: "media"}}, "share", 1, now); err != nil {
		t.Fatalf("WriteSamba: %v", err)
	}
	if err := g.WriteNFS(ctx, nil, "share", 1, now); err != nil {
		t.Fatalf("WriteNFS: %v", err)
	}
	if err := g.KeepUnmanaged(ctx, PathNFS); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}
	if err := g.CanWriteShareFiles(ctx, state); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("CanWriteShareFiles = %v, want ErrUnmanaged", err)
	}
}

// A pending Unraid migration's catch-all is written read-only, whatever else
// the state says.
func TestWriteCatchAllMount_ReadOnlyForAPendingMigration(t *testing.T) {
	g := NewGenerator(t.TempDir())
	state := PoolState{DataDisks: []string{"/mnt/disk1", "/mnt/disk2"}, Options: pool.Options{MinFreeSpace: "50G"}, ReadOnly: true}
	if err := g.WriteCatchAllMount(context.Background(), state, "array create", 1, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(g.Root, "systemd", "system", "mnt-user.mount"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"What=/mnt/disk1=RO:/mnt/disk2=RO", ",ro\n"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("mnt-user.mount:\n%s\nwant %q", body, want)
		}
	}
	state.ReadOnly = false
	if err := g.WriteCatchAllMount(context.Background(), state, "array create", 2, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(filepath.Join(g.Root, "systemd", "system", "mnt-user.mount"))
	if strings.Contains(string(body), "=RO") || strings.Contains(string(body), ",ro\n") {
		t.Errorf("the ordinary catch-all is read-only:\n%s", body)
	}
}

// A pending migration writes the catch-all read-only and no unit for a share or
// its mover target, however the shares are configured: /mnt/user/<share> is a
// directory of the read-only pool.
func TestWritePoolMounts_OnlyTheReadOnlyCatchAllForAPendingMigration(t *testing.T) {
	g := NewGenerator(t.TempDir())
	state := PoolState{
		DataDisks: []string{"/mnt/disk1", "/mnt/disk2"},
		Options:   pool.Options{MinFreeSpace: "50G"},
		ReadOnly:  true,
		Shares: []PoolShare{
			{Name: "media", CacheMode: pool.ArrayOnly, CreatePolicy: pool.FillDisksInOrder, MinFreeSpace: "2000K"},
			{Name: "appdata", CacheMode: pool.CacheOnly, CreatePolicy: pool.BalanceAcrossDisks},
			{Name: "docs", CacheMode: pool.CacheThenMove, CreatePolicy: pool.BalanceAcrossDisks},
		},
	}
	if err := g.WritePoolMounts(context.Background(), state, "share", 1, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	units, _ := filepath.Glob(filepath.Join(g.Root, "systemd", "system", "*.mount"))
	if len(units) != 1 || filepath.Base(units[0]) != "mnt-user.mount" {
		t.Fatalf("units = %v, want only the catch-all", units)
	}
	body, _ := os.ReadFile(units[0])
	if !strings.Contains(string(body), "What=/mnt/disk1=RO:/mnt/disk2=RO\n") || !strings.Contains(string(body)+"\n", ",ro\n") {
		t.Errorf("the catch-all is not read-only:\n%s", body)
	}

	// The same state once the migration has passed its point of no return
	// writes the shares' own mounts, and prunes none of the catch-all's.
	state.ReadOnly = false
	state.CachePath = "/mnt/cache"
	if err := g.WritePoolMounts(context.Background(), state, "share", 2, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	media, err := os.ReadFile(filepath.Join(g.Root, "systemd", "system", "mnt-user-media.mount"))
	if err != nil || !strings.Contains(string(media), "minfreespace=2000K,") || !strings.Contains(string(media), "/media=RW") {
		t.Errorf("mnt-user-media.mount = %s, %v", media, err)
	}
}
