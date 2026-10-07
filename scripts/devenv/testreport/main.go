// Command testreport renders every JUnit XML file under a directory as one
// Markdown test summary (doc 06 §7). CI's test-report job appends its output
// to the run page's $GITHUB_STEP_SUMMARY.
//
// Usage: testreport [-needs needs.json] <dir>
//
//	testreport -comment summary.md -run-url URL -sha SHA -time RFC3339
//
// -needs names a file holding the GitHub Actions `needs` context
// (toJSON(needs)); each job's conclusion is then listed, so a job that failed
// before it wrote a report, or one that runs no tests, still has a row.
//
// -comment instead wraps an already rendered summary as the pull-request
// comment (comment.go): a hidden marker, a header with the run link, commit
// and time, mentions neutralized, and the text cut to GitHub's comment size
// limit with a link to the full run summary.
//
// A file that is empty or cannot be parsed is shown as such, never dropped.
// The exit status is 0 whenever a summary was written: the suites themselves
// own the gate, this command only reports.
package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxMessageRunes = 500
	maxDetailRunes  = 3000
	// A run page's step summary is capped at 1 MiB; the failures listed
	// stay far below it, and the total in the heading still counts them all.
	maxFailuresShown = 100
	maxSkippedShown  = 100
)

type status int

const (
	passed status = iota
	failed
	skipped
)

type testCase struct {
	Suite   string
	Class   string
	Name    string
	Seconds float64
	Status  status
	Message string
	Detail  string
}

type suite struct {
	Name    string
	Seconds float64
	Cases   []testCase
}

type report struct {
	Path   string
	Err    error
	Suites []suite
}

type counts struct {
	Tests, Failed, Skipped int
	Seconds                float64
}

func (c counts) passedN() int { return c.Tests - c.Failed - c.Skipped }

func (c *counts) add(o counts) {
	c.Tests += o.Tests
	c.Failed += o.Failed
	c.Skipped += o.Skipped
	c.Seconds += o.Seconds
}

func (s suite) counts() counts {
	var c counts
	var caseSeconds float64
	for _, tc := range s.Cases {
		c.Tests++
		caseSeconds += tc.Seconds
		switch tc.Status {
		case failed:
			c.Failed++
		case skipped:
			c.Skipped++
		}
	}
	// A suite's own time is wall-clock; the cases' sum double-counts a Go
	// parent test and its subtests, so it is only the fallback.
	c.Seconds = s.Seconds
	if c.Seconds <= 0 {
		c.Seconds = caseSeconds
	}
	return c
}

func (r report) counts() counts {
	var c counts
	for _, s := range r.Suites {
		c.add(s.counts())
	}
	return c
}

func (r report) empty() bool { return r.Err == nil && r.counts().Tests == 0 }

type xmlProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

type xmlCase struct {
	Class   string      `xml:"classname,attr"`
	Name    string      `xml:"name,attr"`
	Time    string      `xml:"time,attr"`
	Failure *xmlProblem `xml:"failure"`
	Error   *xmlProblem `xml:"error"`
	Skipped *xmlProblem `xml:"skipped"`
}

type xmlSuite struct {
	Name  string    `xml:"name,attr"`
	Time  string    `xml:"time,attr"`
	Cases []xmlCase `xml:"testcase"`
}

type xmlSuites struct {
	Suites []xmlSuite `xml:"testsuite"`
}

func parseJUnit(data []byte) ([]suite, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var root xml.StartElement
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("no XML element found")
			}
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			root = se
			break
		}
	}

	var raw []xmlSuite
	switch root.Name.Local {
	case "testsuites":
		var doc xmlSuites
		if err := dec.DecodeElement(&doc, &root); err != nil {
			return nil, err
		}
		raw = doc.Suites
	case "testsuite":
		var s xmlSuite
		if err := dec.DecodeElement(&s, &root); err != nil {
			return nil, err
		}
		raw = []xmlSuite{s}
	default:
		return nil, fmt.Errorf("unexpected root element <%s>, want <testsuites> or <testsuite>", root.Name.Local)
	}

	suites := make([]suite, 0, len(raw))
	for _, rs := range raw {
		s := suite{Name: rs.Name}
		s.Seconds, _ = strconv.ParseFloat(strings.TrimSpace(rs.Time), 64)
		for _, rc := range rs.Cases {
			tc := testCase{Suite: rs.Name, Class: rc.Class, Name: rc.Name}
			tc.Seconds, _ = strconv.ParseFloat(strings.TrimSpace(rc.Time), 64)
			switch {
			case rc.Failure != nil:
				tc.Status, tc.Message, tc.Detail = failed, rc.Failure.Message, strings.TrimSpace(rc.Failure.Body)
			case rc.Error != nil:
				tc.Status, tc.Message, tc.Detail = failed, rc.Error.Message, strings.TrimSpace(rc.Error.Body)
			case rc.Skipped != nil:
				tc.Status, tc.Message = skipped, rc.Skipped.Message
			}
			s.Cases = append(s.Cases, tc)
		}
		suites = append(suites, s)
	}
	return suites, nil
}

func collect(dir string) []report {
	var reports []report
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		if err != nil {
			if path == dir && errors.Is(err, fs.ErrNotExist) {
				return err
			}
			reports = append(reports, report{Path: filepath.ToSlash(rel), Err: err})
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".xml") {
			return nil
		}
		r := report{Path: filepath.ToSlash(rel)}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			r.Err = readErr
		} else {
			r.Suites, r.Err = parseJUnit(data)
		}
		reports = append(reports, r)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		reports = append(reports, report{Path: ".", Err: err})
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Path < reports[j].Path })
	return reports
}

func cell(s string) string {
	s = html.EscapeString(strings.Join(strings.Fields(s), " "))
	s = strings.ReplaceAll(s, "|", "&#124;")
	return strings.ReplaceAll(s, "`", "&#96;")
}

func seconds(s float64) string {
	if s < 60 {
		return strconv.FormatFloat(s, 'f', 1, 64) + "s"
	}
	m := int(s) / 60
	return fmt.Sprintf("%dm %ds", m, int(s)-m*60)
}

func truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:limit]) + fmt.Sprintf("\n… (truncated, %d more characters)", len(r)-limit)
}

func fence(body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	return f + "text\n" + body + "\n" + f + "\n"
}

func suiteLabel(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path))
}

func render(reports []report, needs map[string]string, downloadFailed bool) string {
	var w strings.Builder
	var total counts
	var broken, empties int
	for _, r := range reports {
		switch {
		case r.Err != nil:
			broken++
		case r.empty():
			empties++
		}
		total.add(r.counts())
	}

	fmt.Fprint(&w, "## Test report\n\n")
	if downloadFailed {
		fmt.Fprint(&w, "**The test results could not be downloaded, so this report is incomplete.**\n\n")
	}
	switch {
	case len(reports) == 0:
		fmt.Fprint(&w, "**No test reports were found.**\n\n")
	default:
		fmt.Fprintf(&w, "**%d tests: %d passed, %d failed, %d skipped** in %s across %d reports\n\n",
			total.Tests, total.passedN(), total.Failed, total.Skipped, seconds(total.Seconds), len(reports))
	}
	if broken > 0 {
		fmt.Fprintf(&w, "**%d report(s) could not be read; their tests are not counted above.**\n\n", broken)
	}
	if empties > 0 {
		fmt.Fprintf(&w, "**%d report(s) are empty and contain no tests.**\n\n", empties)
	}

	if len(needs) > 0 {
		jobs := make([]string, 0, len(needs))
		for j := range needs {
			jobs = append(jobs, j)
		}
		sort.Strings(jobs)
		fmt.Fprint(&w, "### Jobs\n\n| Job | Result |\n|---|---|\n")
		for _, j := range jobs {
			res := needs[j]
			if res != "success" && res != "skipped" {
				res = "**" + res + "**"
			}
			fmt.Fprintf(&w, "| %s | %s |\n", cell(j), cell(res))
		}
		fmt.Fprintln(&w)
	}

	if len(reports) > 0 {
		fmt.Fprint(&w, "### Suites\n\n| Suite | Tests | Passed | Failed | Skipped | Time |\n|---|---:|---:|---:|---:|---:|\n")
		for _, r := range reports {
			name := cell(suiteLabel(r.Path))
			switch {
			case r.Err != nil:
				fmt.Fprintf(&w, "| %s | **unreadable report** | | | | |\n", name)
			case r.empty():
				fmt.Fprintf(&w, "| %s | **empty report** | | | | |\n", name)
			default:
				c := r.counts()
				failedCell := strconv.Itoa(c.Failed)
				if c.Failed > 0 {
					failedCell = "**" + failedCell + "**"
				}
				fmt.Fprintf(&w, "| %s | %d | %d | %s | %d | %s |\n", name, c.Tests, c.passedN(), failedCell, c.Skipped, seconds(c.Seconds))
			}
		}
		fmt.Fprintln(&w)
	}

	for _, r := range reports {
		if r.Err == nil {
			continue
		}
		fmt.Fprintf(&w, "### Unreadable report: %s\n\n", cell(r.Path))
		fmt.Fprint(&w, fence(truncate(r.Err.Error(), maxMessageRunes)))
		fmt.Fprintln(&w)
	}

	for _, r := range reports {
		if r.Err != nil || len(r.Suites) < 2 {
			continue
		}
		fmt.Fprintf(&w, "<details><summary>%s: %d suites</summary>\n\n", cell(suiteLabel(r.Path)), len(r.Suites))
		fmt.Fprint(&w, "| Suite | Tests | Passed | Failed | Skipped | Time |\n|---|---:|---:|---:|---:|---:|\n")
		for _, s := range r.Suites {
			c := s.counts()
			fmt.Fprintf(&w, "| %s | %d | %d | %d | %d | %s |\n", cell(s.Name), c.Tests, c.passedN(), c.Failed, c.Skipped, seconds(c.Seconds))
		}
		fmt.Fprint(&w, "\n</details>\n\n")
	}

	var failures []string
	for _, r := range reports {
		for _, s := range r.Suites {
			for _, tc := range s.Cases {
				if tc.Status != failed {
					continue
				}
				var b strings.Builder
				title := tc.Name
				if tc.Suite != "" {
					title = tc.Suite + " / " + tc.Name
				}
				fmt.Fprintf(&b, "<details><summary><code>%s</code> (%s)</summary>\n\n", cell(title), cell(suiteLabel(r.Path)))
				msg := truncate(strings.TrimSpace(tc.Message), maxMessageRunes)
				detail := truncate(tc.Detail, maxDetailRunes)
				switch {
				case detail == "" && msg == "":
					b.WriteString("No failure message was recorded.\n")
				case detail == "":
					b.WriteString(fence(msg))
				case msg == "" || strings.HasPrefix(tc.Detail, strings.TrimSpace(tc.Message)):
					b.WriteString(fence(detail))
				default:
					b.WriteString(fence(msg + "\n\n" + detail))
				}
				b.WriteString("\n</details>\n\n")
				failures = append(failures, b.String())
			}
		}
	}
	if len(failures) > 0 {
		fmt.Fprintf(&w, "### Failures (%d)\n\n", len(failures))
		for i, f := range failures {
			if i == maxFailuresShown {
				fmt.Fprintf(&w, "**%d more failures are not shown.** The `test-results-*` artifacts hold every one.\n\n", len(failures)-maxFailuresShown)
				break
			}
			fmt.Fprint(&w, f)
		}
	}

	var skips []string
	for _, r := range reports {
		for _, s := range r.Suites {
			for _, tc := range s.Cases {
				if tc.Status != skipped {
					continue
				}
				title := tc.Name
				if tc.Suite != "" {
					title = tc.Suite + " / " + tc.Name
				}
				skips = append(skips, fmt.Sprintf("| %s | %s | %s |\n", cell(suiteLabel(r.Path)), cell(title), cell(truncate(tc.Message, maxMessageRunes))))
			}
		}
	}
	if len(skips) > 0 {
		fmt.Fprintf(&w, "<details><summary>Skipped (%d)</summary>\n\n| Report | Test | Reason |\n|---|---|---|\n", len(skips))
		for i, row := range skips {
			if i == maxSkippedShown {
				break
			}
			fmt.Fprint(&w, row)
		}
		if len(skips) > maxSkippedShown {
			fmt.Fprintf(&w, "\n%d more skipped tests are not listed.\n", len(skips)-maxSkippedShown)
		}
		fmt.Fprint(&w, "\n</details>\n\n")
	}
	return w.String()
}

func loadNeeds(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]string, len(raw))
	for job, v := range raw {
		out[job] = v.Result
	}
	return out, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("testreport", flag.ContinueOnError)
	fl.SetOutput(stderr)
	needsPath := fl.String("needs", "", "file holding the GitHub Actions `needs` context as JSON")
	commentPath := fl.String("comment", "", "render the run summary in `file` as a pull-request comment instead of reading a directory")
	runURL := fl.String("run-url", "", "with -comment: the workflow run's URL")
	sha := fl.String("sha", "", "with -comment: the commit the run tested")
	when := fl.String("time", "", "with -comment: when the run finished, RFC 3339")
	downloadFailed := fl.Bool("download-failed", false, "say that downloading the reports failed, so the summary is incomplete")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *commentPath != "" {
		if fl.NArg() != 0 || *needsPath != "" || *downloadFailed {
			_, _ = fmt.Fprintln(stderr, "usage: testreport -comment summary.md -run-url URL -sha SHA -time RFC3339")
			return 2
		}
		if err := runComment(*commentPath, *runURL, *sha, *when, stdout); err != nil {
			_, _ = fmt.Fprintln(stderr, "testreport: -comment:", err)
			return 2
		}
		return 0
	}
	if fl.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: testreport [-needs needs.json] [-download-failed] <dir>")
		return 2
	}
	var needs map[string]string
	if *needsPath != "" {
		var err error
		if needs, err = loadNeeds(*needsPath); err != nil {
			_, _ = fmt.Fprintln(stderr, "testreport: -needs:", err)
			return 2
		}
	}
	if _, err := io.WriteString(stdout, render(collect(fl.Arg(0)), needs, *downloadFailed)); err != nil {
		_, _ = fmt.Fprintln(stderr, "testreport:", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
