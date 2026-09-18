package parity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/config/golden"
)

// layoutTestdataDir holds one directory per case (doc 06 §2): a
// layout.json a Layout unmarshals from, plus the snapraid.conf.golden
// Render must reproduce exactly. It lives under internal/parity/ rather
// than the repo-root testdata/configs/ (#25's config.SnapraidState only
// has one parity field, so it cannot express 2-parity's own directive —
// see this package's doc comment on Layout.Render).
const layoutTestdataDir = "testdata/layouts"

func TestLayout_Render_Golden(t *testing.T) {
	cases, err := os.ReadDir(layoutTestdataDir)
	if err != nil {
		t.Fatalf("reading %s: %v", layoutTestdataDir, err)
	}

	for _, c := range cases {
		if !c.IsDir() {
			continue
		}

		name := c.Name()
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(layoutTestdataDir, name)

			raw, err := os.ReadFile(filepath.Join(dir, "layout.json"))
			if err != nil {
				t.Fatalf("reading layout.json: %v", err)
			}

			var l Layout
			if err := json.Unmarshal(raw, &l); err != nil {
				t.Fatalf("parsing layout.json: %v", err)
			}

			got, err := l.Render()
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			golden.Compare(t, filepath.Join(dir, "snapraid.conf.golden"), []byte(got))
		})
	}
}
