package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	testRunURL = "https://github.com/mdg-labs/hoserva/actions/runs/1234567"
	testSHA    = "0123456789abcdef0123456789abcdef01234567"
	testTime   = "2026-10-07T05:54:58Z"
)

func renderCommentFile(t *testing.T, summary string, args ...string) (string, string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-summary.md")
	if err := os.WriteFile(path, []byte(summary), 0o600); err != nil {
		t.Fatal(err)
	}
	if args == nil {
		args = []string{"-run-url", testRunURL, "-sha", testSHA, "-time", testTime}
	}
	var out, errb bytes.Buffer
	code := run(append([]string{"-comment", path}, args...), &out, &errb)
	return out.String(), errb.String(), code
}

func TestCommentHasMarkerAndHeader(t *testing.T) {
	out, _, code := renderCommentFile(t, "## Test report\n\n**5 tests: 5 passed**\n")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(out, commentMarker+"\n") {
		t.Errorf("comment does not start with the marker:\n%s", out)
	}
	if n := strings.Count(out, commentMarker); n != 1 {
		t.Errorf("marker appears %d times, want 1", n)
	}
	mustContain(t, out,
		"`0123456`",
		"["+"workflow run"+"]("+testRunURL+")",
		"2026-10-07 05:54 UTC",
		"**5 tests: 5 passed**",
	)
}

func TestCommentNeutralizesMentions(t *testing.T) {
	out, _, code := renderCommentFile(t, "failed: @octocat and @mdg-labs/maintainers, mail a@b.c, @ alone\n```\n@inside_fence\n```\n")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	mustContain(t, out,
		"@\u200doctocat",
		"@\u200dmdg-labs/maintainers",
		"@\u200dinside_fence",
		"@ alone",
	)
	mustNotContain(t, out, "@octocat", "@mdg-labs", "@inside_fence")
}

func TestCommentCannotForgeTheMarker(t *testing.T) {
	out, _, code := renderCommentFile(t, "x "+commentMarker+" y <!-- hoserva-test<!-- hoserva-test-report -->-report -->\n")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if n := strings.Count(out, commentMarker); n != 1 {
		t.Errorf("marker appears %d times, want 1:\n%s", n, out)
	}
}

func TestCommentIsTruncatedWithALinkAndNoOpenBlock(t *testing.T) {
	var summary strings.Builder
	summary.WriteString("## Test report\n\n### Failures (400)\n\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&summary, "<details><summary><code>T%03d</code></summary>\n\n```text\n%s\n```\n\n</details>\n\n", i, strings.Repeat("line of failing output ", 20))
	}
	if summary.Len() < 3*65536 {
		t.Fatalf("fixture too small: %d", summary.Len())
	}
	out, _, code := renderCommentFile(t, summary.String())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if n := utf8.RuneCountInString(out); n > 65536 || n > maxCommentRunes {
		t.Errorf("comment is %d characters, over the limit", n)
	}
	mustContain(t, out, "[full run summary]("+testRunURL+")", "T000", "`0123456`")
	mustNotContain(t, out, "T399")
	if o, c := strings.Count(out, "<details>"), strings.Count(out, "</details>"); o != c {
		t.Errorf("%d <details> opened, %d closed", o, c)
	}
	fences := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "```") {
			fences++
		}
	}
	if fences%2 != 0 {
		t.Errorf("a code fence is left open (%d fence lines)", fences)
	}
}

func TestShortCommentIsNotTruncated(t *testing.T) {
	out, _, _ := renderCommentFile(t, "## Test report\n\nall good\n")
	mustNotContain(t, out, "full run summary")
}

func TestSafePrefixStopsBeforeAnOpenFence(t *testing.T) {
	body := "intro\n\n```text\n" + strings.Repeat("a\n\n", 50) + "```\n\nafter\n"
	got, cut := safePrefix(body, 40)
	if !cut || got != "intro\n\n" {
		t.Errorf("safePrefix = %q, cut=%v; want the text before the fence", got, cut)
	}
}

func TestCommentRejectsBadInputs(t *testing.T) {
	cases := map[string][]string{
		"not a run url":    {"-run-url", "https://example.com/x", "-sha", testSHA, "-time", testTime},
		"url with a quote": {"-run-url", testRunURL + ")[x](y", "-sha", testSHA, "-time", testTime},
		"uppercase sha":    {"-run-url", testRunURL, "-sha", "ABCDEF1", "-time", testTime},
		"bad time":         {"-run-url", testRunURL, "-sha", testSHA, "-time", "yesterday"},
	}
	for name, args := range cases {
		out, _, code := renderCommentFile(t, "x", args...)
		if code != 2 || out != "" {
			t.Errorf("%s: exit %d, output %q; want exit 2 and no output", name, code, out)
		}
	}
	var o, e bytes.Buffer
	if code := run([]string{"-comment", filepath.Join(t.TempDir(), "absent.md"), "-run-url", testRunURL, "-sha", testSHA, "-time", testTime}, &o, &e); code != 2 {
		t.Errorf("missing file: exit %d, want 2", code)
	}
	if code := run([]string{"-comment", "x.md", "testdata/allpass"}, &o, &e); code != 2 {
		t.Errorf("-comment with a directory: exit %d, want 2", code)
	}
}

func TestCommentReadsOnlyTheFirstBytes(t *testing.T) {
	big := strings.Repeat("a line\n\n", maxSummaryBytes/8+1000)
	out, _, code := renderCommentFile(t, big)
	if code != 0 || utf8.RuneCountInString(out) > maxCommentRunes {
		t.Errorf("exit %d, %d characters", code, utf8.RuneCountInString(out))
	}
}

func TestRunSummaryModeIsUnchangedByTheCommentFlags(t *testing.T) {
	out := renderDir(t, "testdata/allpass")
	mustNotContain(t, out, commentMarker)
	if _, err := time.Parse(time.RFC3339, testTime); err != nil {
		t.Fatal(err)
	}
}
