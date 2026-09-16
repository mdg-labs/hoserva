// Package golden implements golden-file testing for Hoserva's generated
// configs (doc 06 §2): render state to text, compare it against a checked-in
// file, and require an explicit flag before that file ever changes.
package golden

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var update = flag.Bool("update", false, "update .golden files to match the current output instead of comparing against them")

// TB is the subset of testing.TB that Compare needs. *testing.T and
// *testing.B satisfy it.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Compare renders got against the golden file at path.
//
// Without -update, a mismatch fails the test and path is never touched.
// With -update, a mismatch is written to path and logged as a diff — a
// golden change is always visible, never silent, and a plain test run
// never takes this branch.
func Compare(t TB, path string, got []byte) {
	t.Helper()

	want, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading golden %s: %v", path, err)
		return
	}

	if bytes.Equal(want, got) {
		return
	}

	if !*update {
		t.Fatalf("golden mismatch for %s (run the test with -update to review and accept):\n%s", path, unifiedDiff(want, got))
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating directory for golden %s: %v", path, err)
		return
	}
	if err := os.WriteFile(path, got, 0o644); err != nil {
		t.Fatalf("writing golden %s: %v", path, err)
		return
	}
	t.Logf("updated golden %s:\n%s", path, unifiedDiff(want, got))
}

// unifiedDiff is a minimal line-oriented diff — enough to show a reviewer
// what changed in a generated config without a diff library.
func unifiedDiff(want, got []byte) string {
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(string(got), "\n")

	max := len(wantLines)
	if len(gotLines) > max {
		max = len(gotLines)
	}

	var b strings.Builder
	for i := 0; i < max; i++ {
		var w, g string
		haveW, haveG := i < len(wantLines), i < len(gotLines)
		if haveW {
			w = wantLines[i]
		}
		if haveG {
			g = gotLines[i]
		}
		if haveW && haveG && w == g {
			continue
		}
		if haveW {
			fmt.Fprintf(&b, "-%s\n", w)
		}
		if haveG {
			fmt.Fprintf(&b, "+%s\n", g)
		}
	}
	return b.String()
}
