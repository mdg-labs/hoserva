package golden

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTB records what Compare does instead of driving a real *testing.T, so
// these tests can assert on Compare's own pass/fail/log behaviour without
// nesting real subtests.
type fakeTB struct {
	failed bool
	fatal  string
	logs   []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.failed = true
	f.fatal = fmt.Sprintf(format, args...)
}

func (f *fakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

func setUpdate(v bool) func() {
	orig := *update
	*update = v
	return func() { *update = orig }
}

func TestCompareMatchDoesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.golden")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tb := &fakeTB{}
	Compare(tb, path, []byte("same\n"))

	if tb.failed {
		t.Fatalf("Compare failed on a match: %s", tb.fatal)
	}
	if len(tb.logs) != 0 {
		t.Fatalf("Compare logged on a match: %v", tb.logs)
	}
}

func TestCompareMismatchFailsAndPrintsDiffWithoutRewriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.golden")
	if err := os.WriteFile(path, []byte("old line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tb := &fakeTB{}
	Compare(tb, path, []byte("new line\n"))

	if !tb.failed {
		t.Fatal("Compare did not fail on a mismatch")
	}
	if !strings.Contains(tb.fatal, "-old line") || !strings.Contains(tb.fatal, "+new line") {
		t.Fatalf("failure message did not include a diff: %s", tb.fatal)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old line\n" {
		t.Fatalf("a plain comparison rewrote the golden file: got %q", got)
	}
}

func TestCompareUpdateRewritesAndLogsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.golden")
	if err := os.WriteFile(path, []byte("old line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	defer setUpdate(true)()

	tb := &fakeTB{}
	Compare(tb, path, []byte("new line\n"))

	if tb.failed {
		t.Fatalf("Compare failed under -update: %s", tb.fatal)
	}
	if len(tb.logs) == 0 || !strings.Contains(tb.logs[0], "-old line") || !strings.Contains(tb.logs[0], "+new line") {
		t.Fatalf("-update did not log a diff: %v", tb.logs)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new line\n" {
		t.Fatalf("-update did not rewrite the golden file: got %q", got)
	}
}

func TestCompareUpdateCreatesMissingGolden(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "example.golden")

	defer setUpdate(true)()

	tb := &fakeTB{}
	Compare(tb, path, []byte("first\n"))

	if tb.failed {
		t.Fatalf("Compare failed creating a new golden: %s", tb.fatal)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\n" {
		t.Fatalf("-update did not create the golden with the rendered content: got %q", got)
	}
}
