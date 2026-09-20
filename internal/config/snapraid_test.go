package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/config/golden"
)

// testdataDir holds one directory per case (doc 06 §2): state.json plus the
// *.golden files rendered from it.
const testdataDir = "../../testdata/configs"

func TestRenderSnapraidConf(t *testing.T) {
	cases, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("reading %s: %v", testdataDir, err)
	}

	for _, c := range cases {
		if !c.IsDir() {
			continue
		}

		name := c.Name()
		dir := filepath.Join(testdataDir, name)
		if _, err := os.Stat(filepath.Join(dir, "snapraid.conf.golden")); err != nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatalf("reading state.json: %v", err)
			}

			var state SnapraidState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatalf("parsing state.json: %v", err)
			}

			got := RenderSnapraidConf(state)
			golden.Compare(t, filepath.Join(dir, "snapraid.conf.golden"), []byte(got))
		})
	}
}
