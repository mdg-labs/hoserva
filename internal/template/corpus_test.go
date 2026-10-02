package template

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const corpusDir = "../../testdata/unraid-templates"

// Outcomes of converting one corpus template.
const (
	outcomeClean    = "clean"
	outcomeWarnings = "warnings"
	outcomeFailed   = "failed"
)

type corpusResult struct {
	outcome string
	detail  string
}

// convertCorpusFile converts one template and classifies the result by Q36.
// A converter that panics on a template is a failed conversion of that
// template, never a crash of the run.
func convertCorpusFile(convert func([]byte, ConvertOptions) (*Conversion, error), data []byte) (res corpusResult) {
	defer func() {
		if r := recover(); r != nil {
			res = corpusResult{outcomeFailed, fmt.Sprintf("panic: %v", r)}
		}
	}()
	conv, err := convert(data, ConvertOptions{})
	if err != nil {
		return corpusResult{outcomeFailed, err.Error()}
	}
	if conv.Clean() {
		return corpusResult{outcome: outcomeClean}
	}
	counts := map[string]int{}
	for _, w := range conv.Warnings {
		if w.Class != WarnNote && w.Class != WarnWritableLayer {
			counts[w.Class]++
		}
	}
	classes := make([]string, 0, len(counts))
	for class, n := range counts {
		classes = append(classes, fmt.Sprintf("%s x%d", class, n))
	}
	sort.Strings(classes)
	return corpusResult{outcomeWarnings, strings.Join(classes, ", ")}
}

type corpusSummary struct {
	total, clean, warnings, failed int
}

func (s corpusSummary) String() string {
	return fmt.Sprintf("%d of %d templates convert cleanly (%.1f%%); %d with warnings, %d failed", s.clean, s.total, 100*float64(s.clean)/float64(s.total), s.warnings, s.failed)
}

func summarize(results map[string]corpusResult) corpusSummary {
	s := corpusSummary{total: len(results)}
	for _, r := range results {
		switch r.outcome {
		case outcomeClean:
			s.clean++
		case outcomeWarnings:
			s.warnings++
		default:
			s.failed++
		}
	}
	return s
}

// regressed reports whether the rate got.clean/got.total is below the
// recorded rate, compared exactly without floating point.
func regressed(got, recorded corpusSummary) bool {
	return got.clean*recorded.total < recorded.clean*got.total
}

// readManifest reads manifest.txt: one "<file> <outcome>" per line, and the
// "# recorded clean=N total=M" line that holds the last recorded rate.
func readManifest(path string) (map[string]string, corpusSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, corpusSummary{}, err
	}
	expected := map[string]string{}
	var recorded corpusSummary
	seenRecord := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "# recorded "):
			for _, kv := range strings.Fields(strings.TrimPrefix(line, "# recorded ")) {
				k, v, _ := strings.Cut(kv, "=")
				n, err := strconv.Atoi(v)
				if err != nil {
					return nil, corpusSummary{}, fmt.Errorf("manifest line %q: %w", line, err)
				}
				switch k {
				case "clean":
					recorded.clean = n
				case "total":
					recorded.total = n
				}
			}
			seenRecord = recorded.total > 0
		case strings.HasPrefix(line, "#"):
		default:
			name, outcome, ok := strings.Cut(line, " ")
			if !ok {
				return nil, corpusSummary{}, fmt.Errorf("manifest line %q: want \"<file> <outcome>\"", line)
			}
			expected[name] = strings.TrimSpace(outcome)
		}
	}
	if !seenRecord {
		return nil, corpusSummary{}, errors.New("manifest has no \"# recorded clean=N total=M\" line")
	}
	return expected, recorded, nil
}

// TestUnraidCorpus is what `make test-corpus` runs: it converts every
// template in testdata/unraid-templates/, reports the clean-conversion rate
// (Q36, doc 06 §2) and fails when that rate is below the recorded one or a
// template no longer converts the way the manifest says.
func TestUnraidCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no templates in %s", corpusDir)
	}
	expected, recorded, err := readManifest(filepath.Join(corpusDir, "manifest.txt"))
	if err != nil {
		t.Fatal(err)
	}

	results := map[string]corpusResult{}
	for _, file := range files {
		name := filepath.Base(file)
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		results[name] = convertCorpusFile(ConvertUnraid, data)
	}

	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r := results[name]
		line := fmt.Sprintf("%-8s %s", r.outcome, name)
		if r.detail != "" {
			line += "  (" + r.detail + ")"
		}
		t.Log(line)
		want, listed := expected[name]
		switch {
		case !listed:
			t.Errorf("%s is not in manifest.txt", name)
		case r.outcome != want:
			t.Errorf("%s converted as %s, manifest says %s (%s)", name, r.outcome, want, r.detail)
		}
	}
	for name := range expected {
		if _, ok := results[name]; !ok {
			t.Errorf("manifest.txt lists %s, which is not in the corpus", name)
		}
	}

	got := summarize(results)
	t.Logf("clean-conversion rate: %s", got)
	t.Logf("recorded rate: %d of %d", recorded.clean, recorded.total)
	if regressed(got, recorded) {
		t.Errorf("the clean-conversion rate regressed: %d of %d is below the recorded %d of %d", got.clean, got.total, recorded.clean, recorded.total)
	}
	if regressed(recorded, got) {
		t.Logf("the rate improved: update the \"# recorded\" line of manifest.txt to clean=%d total=%d", got.clean, got.total)
	}
}

func TestCorpusMalformedTemplatesFailWithoutPanic(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(corpusDir, "malformed-*.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("the corpus has no malformed-*.xml template")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ConvertUnraid(data, ConvertOptions{}); !errors.Is(err, ErrInvalidUnraidTemplate) {
			t.Errorf("%s: err = %v, want ErrInvalidUnraidTemplate", filepath.Base(file), err)
		}
		if res := convertCorpusFile(ConvertUnraid, data); res.outcome != outcomeFailed {
			t.Errorf("%s: outcome = %s, want %s", filepath.Base(file), res.outcome, outcomeFailed)
		}
	}
}

func TestCorpusRunSurvivesAPanickingConversion(t *testing.T) {
	panicking := func([]byte, ConvertOptions) (*Conversion, error) { panic("boom") }
	res := convertCorpusFile(panicking, []byte("<Container/>"))
	if res.outcome != outcomeFailed || !strings.Contains(res.detail, "boom") {
		t.Fatalf("result = %+v, want a failed conversion naming the panic", res)
	}
	got := summarize(map[string]corpusResult{"a.xml": res, "b.xml": {outcome: outcomeClean}})
	if got.total != 2 || got.clean != 1 || got.failed != 1 {
		t.Fatalf("summary = %+v, want one clean and one failed of two", got)
	}
}

func TestCorpusRegressionCheck(t *testing.T) {
	recorded := corpusSummary{total: 20, clean: 12}
	for _, tc := range []struct {
		name string
		got  corpusSummary
		want bool
	}{
		{"same", corpusSummary{total: 20, clean: 12}, false},
		{"one fewer clean", corpusSummary{total: 20, clean: 11}, true},
		{"more clean", corpusSummary{total: 20, clean: 13}, false},
		{"same count over a larger corpus", corpusSummary{total: 24, clean: 12}, true},
		{"same rate over a larger corpus", corpusSummary{total: 30, clean: 18}, false},
	} {
		if got := regressed(tc.got, recorded); got != tc.want {
			t.Errorf("%s: regressed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
