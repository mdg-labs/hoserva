package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/config/golden"
)

// poolTestdataDir mirrors internal/config's own testdataDir convention
// (doc 06 §2): one directory per case, a state.json plus *.golden files
// rendered from it — the golden files this issue's acceptance criteria
// ask for, one per cache mode.
const poolTestdataDir = "../../testdata/configs/pool-mounts"

// shareState is state.json's own per-share shape.
type shareState struct {
	Name         string `json:"name"`
	CacheMode    string `json:"cache_mode"`
	CreatePolicy string `json:"create_policy"`
}

type poolState struct {
	DataDisks []string     `json:"data_disks"`
	CachePath string       `json:"cache_path"`
	Shares    []shareState `json:"shares"`
}

// unitFileName mirrors disk.UnitFileName's own systemd-escape rule
// (CLAUDE.md: kept consistent in style, not shared code, since these
// are different packages rendering different kinds of units).
func unitFileName(where string) string {
	return strings.ReplaceAll(strings.Trim(where, "/"), "/", "-") + ".mount"
}

// TestRenderPoolMounts golden-tests every mount doc 02 §1's topology
// describes — the catch-all, one per-share mount per cache mode, and
// each share's own mover write target — against testdata/configs/
// pool-mounts, so a change to any of them is a reviewed diff, never a
// silent regeneration (CLAUDE.md: "Golden files change only
// deliberately").
func TestRenderPoolMounts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(poolTestdataDir, "state.json"))
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}
	var state poolState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("parsing state.json: %v", err)
	}

	opts := DefaultOptions()

	catchAll, err := CatchAllMount(state.DataDisks, opts)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	golden.Compare(t, filepath.Join(poolTestdataDir, unitFileName(catchAll.Where)+".golden"), []byte(catchAll.Render()))

	for _, s := range state.Shares {
		share := Share{Name: s.Name, CacheMode: CacheMode(s.CacheMode), CreatePolicy: CreatePolicy(s.CreatePolicy)}

		shareMount, err := ShareMount(share, state.DataDisks, state.CachePath, opts)
		if err != nil {
			t.Fatalf("ShareMount(%s): %v", s.Name, err)
		}
		golden.Compare(t, filepath.Join(poolTestdataDir, unitFileName(shareMount.Where)+".golden"), []byte(shareMount.Render()))

		moverMount, err := MoverTargetMount(share, state.DataDisks, opts)
		if err != nil {
			t.Fatalf("MoverTargetMount(%s): %v", s.Name, err)
		}
		golden.Compare(t, filepath.Join(poolTestdataDir, unitFileName(moverMount.Where)+".golden"), []byte(moverMount.Render()))
	}
}
