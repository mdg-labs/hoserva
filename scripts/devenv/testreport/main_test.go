package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func renderDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run(append(args, dir), &out, &errb); code != 0 {
		t.Fatalf("run exited %d: %s", code, errb.String())
	}
	return out.String()
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func mustNotContain(t *testing.T, out string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(out, w) {
			t.Errorf("output contains %q:\n%s", w, out)
		}
	}
}

func TestAllPass(t *testing.T) {
	out := renderDir(t, "testdata/allpass")
	mustContain(t, out,
		"**5 tests: 5 passed, 0 failed, 0 skipped** in 4.8s across 2 reports",
		"| go-unit | 3 | 3 | 0 | 0 | 1.5s |",
		"| packaging-test | 2 | 2 | 0 | 0 | 3.3s |",
		"<summary>go-unit: 2 suites</summary>",
		"| github.com/mdg-labs/hoserva/internal/pool | 2 | 2 | 0 | 0 | 1.0s |",
	)
	mustNotContain(t, out, "### Failures", "could not be read", "are empty")
}

func TestFailuresKeepMessages(t *testing.T) {
	out := renderDir(t, "testdata/failures")
	mustContain(t, out,
		"**5 tests: 1 passed, 4 failed, 0 skipped**",
		"| lab-go-internal-parity | 3 | 1 | **2** | 0 | 9.0s |",
		"### Failures (4)",
		"internal/parity / TestLabGuard",
		"expected 200 got 500 ",
		"scripts/test-gh-rest.sh",
		"exit status 1: not ok 4",
		"not ok 4",
	)
}

func TestLongFailureIsTruncated(t *testing.T) {
	out := renderDir(t, "testdata/failures")
	mustContain(t, out, "… (truncated, ")
	mustContain(t, out, "long message begins m")
	mustNotContain(t, out, "mmmmm ends", "line 399 of the failing output")
	if strings.Count(out, "x") > 2*maxDetailRunes {
		t.Errorf("failure detail was not bounded")
	}
}

func TestFailureTextIsEscapedAndFenceGrows(t *testing.T) {
	out := renderDir(t, "testdata/failures")
	mustContain(t, out,
		"<summary><code>internal/parity / TestLabEscape&lt;T&gt;&#124;&#96;x&#96;</code>",
		"````text\ngot <nil> & \"want\" | ``` fence\n\na | b <script>alert(1)</script> ``` end\n````\n",
	)
	mustNotContain(t, out, "TestLabEscape<T>")
}

func TestSkipped(t *testing.T) {
	out := renderDir(t, "testdata/skipped")
	mustContain(t, out,
		"**3 tests: 2 passed, 0 failed, 1 skipped**",
		"| web-vitest | 3 | 2 | 0 | 1 | 2.0s |",
	)
	mustNotContain(t, out, "### Failures")
}

func TestEmptyReportIsShown(t *testing.T) {
	out := renderDir(t, "testdata/empty")
	mustContain(t, out,
		"| go-corpus | **empty report** | | | | |",
		"**1 report(s) are empty and contain no tests.**",
		"**0 tests: 0 passed, 0 failed, 0 skipped**",
	)
}

func TestMalformedReportIsShownNotDropped(t *testing.T) {
	out := renderDir(t, "testdata/malformed")
	mustContain(t, out,
		"| lab | **unreadable report** | | | | |",
		"**1 report(s) could not be read; their tests are not counted above.**",
		"### Unreadable report: lab.xml",
		"XML syntax error",
	)
}

func TestMalformedBesideGoodReports(t *testing.T) {
	dir := t.TempDir()
	for _, src := range []string{"allpass/go-unit.xml", "malformed/lab.xml"} {
		data, err := os.ReadFile(filepath.Join("testdata", src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := renderDir(t, dir)
	mustContain(t, out,
		"**3 tests: 3 passed, 0 failed, 0 skipped**",
		"| go-unit | 3 | 3 | 0 | 0 | 1.5s |",
		"| lab | **unreadable report** |",
	)
}

func TestWrongRootElementIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "html.xml"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blank.xml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out := renderDir(t, dir)
	mustContain(t, out,
		"unexpected root element <html>",
		"no XML element found",
		"**2 report(s) could not be read",
	)
}

func TestErrorElementCountsAsFailure(t *testing.T) {
	dir := t.TempDir()
	doc := `<testsuite name="s"><testcase classname="s" name="boom" time="1"><error message="panic: boom">stack</error></testcase></testsuite>`
	if err := os.WriteFile(filepath.Join(dir, "s.xml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out := renderDir(t, dir)
	mustContain(t, out, "**1 tests: 0 passed, 1 failed, 0 skipped**", "### Failures (1)", "panic: boom")
}

func TestNoReports(t *testing.T) {
	out := renderDir(t, t.TempDir())
	mustContain(t, out, "**No test reports were found.**")
	out = renderDir(t, filepath.Join(t.TempDir(), "missing"))
	mustContain(t, out, "**No test reports were found.**")
}

func TestNestedArtifactDirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "test-results-web")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/skipped/web-vitest.xml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "web-vitest.xml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	mustContain(t, renderDir(t, dir), "| test-results-web/web-vitest | 3 | 2 | 0 | 1 | 2.0s |")
}

func TestNeedsListsEveryJob(t *testing.T) {
	needs := filepath.Join(t.TempDir(), "needs.json")
	doc := `{"lint-and-unit":{"result":"failure","outputs":{}},"dco":{"result":"success"},"deb":{"result":"skipped"},"lab":{"result":"cancelled"}}`
	if err := os.WriteFile(needs, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out := renderDir(t, "testdata/allpass", "-needs", needs)
	mustContain(t, out,
		"### Jobs",
		"| dco | success |",
		"| deb | skipped |",
		"| lab | **cancelled** |",
		"| lint-and-unit | **failure** |",
	)
}

func TestUsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Errorf("no args: exit %d, want 2", code)
	}
	if code := run([]string{"-needs", filepath.Join(t.TempDir(), "absent.json"), "testdata/allpass"}, &out, &errb); code != 2 {
		t.Errorf("unreadable -needs: exit %d, want 2", code)
	}
}

func TestFailuresListIsCapped(t *testing.T) {
	dir := t.TempDir()
	var doc strings.Builder
	doc.WriteString(`<testsuite name="many">`)
	for i := 0; i < maxFailuresShown+7; i++ {
		fmt.Fprintf(&doc, `<testcase classname="many" name="T%03d"><failure message="bad"></failure></testcase>`, i)
	}
	doc.WriteString(`</testsuite>`)
	if err := os.WriteFile(filepath.Join(dir, "many.xml"), []byte(doc.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out := renderDir(t, dir)
	mustContain(t, out, "### Failures (107)", "T099", "**7 more failures are not shown.**", "| many | 107 | 0 | **107** |")
	mustNotContain(t, out, "T100")
}

func TestSuiteTimeFallsBackToTheCases(t *testing.T) {
	dir := t.TempDir()
	doc := `<testsuite name="s"><testcase classname="s" name="a" time="1.5"/><testcase classname="s" name="b" time="2.0"/></testsuite>`
	if err := os.WriteFile(filepath.Join(dir, "s.xml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	mustContain(t, renderDir(t, dir), "| s | 2 | 2 | 0 | 0 | 3.5s |")
}
