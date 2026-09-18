package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
