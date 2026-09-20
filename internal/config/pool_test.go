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
// pool state, it writes the catch-all, each share's own mount, and each
// non-cache-only share's mover write target, and every written file matches
// a checked-in golden fixture (CLAUDE.md: "Golden files change only
// deliberately").
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

	want := Header(command, revision, now) + catchAll.Render()
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
