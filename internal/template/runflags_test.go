package template

import (
	"reflect"
	"strings"
	"testing"
)

func TestShellWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{``, nil},
		{`  `, nil},
		{`--a --b=c`, []string{"--a", "--b=c"}},
		{`--health-cmd='curl -f http://x || exit 1'`, []string{"--health-cmd=curl -f http://x || exit 1"}},
		{`--x="a b" --y='c d'`, []string{"--x=a b", "--y=c d"}},
		{`--x=a\ b`, []string{"--x=a b"}},
		{`"" x`, []string{"", "x"}},
		{"--a\n--b", []string{"--a", "--b"}},
		{`--x="say \"hi\""`, []string{`--x=say "hi"`}},
		{`--x=a#b`, []string{"--x=a#b"}},
	}
	for _, tc := range cases {
		got, err := shellWords(tc.in)
		if err != nil {
			t.Errorf("shellWords(%q): %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("shellWords(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellWordsRefusesWhatAShellWouldActOn(t *testing.T) {
	for _, in := range []string{
		`a; b`, `a && b`, `a | b`, `a > f`, `a < f`, "a `b`", `a $(b)`, `a $B`, `a "$B"`, `a "` + "`b`" + `"`,
		`a *`, `a ?`, `a [x]`, `a {x,y}`, `#c`, `~/x`, `a 'b`, `a "b`, `a\`, `a (b)`,
	} {
		if got, err := shellWords(in); err == nil {
			t.Errorf("shellWords(%q) = %q, want an error", in, got)
		}
	}
}

func TestShowWordsKeepsAWordOnOneLine(t *testing.T) {
	got := showWords("--a=b", "two words", "line\nbreak", "tab\t", "quo\"te")
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("showWords = %q has a control character", got)
	}
	want := `--a=b "two words" "line\nbreak" "tab\t" "quo\"te"`
	if got != want {
		t.Errorf("showWords = %q, want %q", got, want)
	}
}

func TestParseRunFlags(t *testing.T) {
	type flag struct {
		Name, Value string
		Known       bool
	}
	cases := []struct {
		in   string
		want []flag
	}{
		{"--memory=1g", []flag{{"--memory", "1g", true}}},
		{"--memory 1g", []flag{{"--memory", "1g", true}}},
		{"-m1g", []flag{{"--memory", "1g", true}}},
		{"-m=1g", []flag{{"--memory", "1g", true}}},
		{"-m 1g", []flag{{"--memory", "1g", true}}},
		{"-it", []flag{{"--interactive", "", true}, {"--tty", "", true}}},
		{"-ti", []flag{{"--tty", "", true}, {"--interactive", "", true}}},
		{"-itu 99", []flag{{"--interactive", "", true}, {"--tty", "", true}, {"--user", "99", true}}},
		{"--init --read-only", []flag{{"--init", "", true}, {"--read-only", "", true}}},
		{"--user -1", []flag{{"--user", "-1", true}}},
		{"--unknown", []flag{{"--unknown", "", false}}},
		{"--unknown value --restart=no", []flag{{"--unknown", "", false}, {"--restart", "no", true}}},
		{"--unknown --restart=no", []flag{{"--unknown", "", false}, {"--restart", "no", true}}},
		{"-d", []flag{{"-d", "", false}}},
		{"-itd", []flag{{"--interactive", "", true}, {"--tty", "", true}, {"-d", "", false}}},
		{"word", []flag{{"word", "", false}}},
		{"-- a b", []flag{{"--", "", false}}},
	}
	for _, tc := range cases {
		words, err := shellWords(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		var got []flag
		for _, f := range parseRunFlags(words, runFlagTable) {
			got = append(got, flag{f.Name, f.Value, f.Known})
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseRunFlags(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseRunFlagsMarksAMissingValue(t *testing.T) {
	for _, in := range []string{"--memory", "-m", "-itm"} {
		words, _ := shellWords(in)
		flags := parseRunFlags(words, runFlagTable)
		if last := flags[len(flags)-1]; !last.Missing || last.Name != "--memory" {
			t.Errorf("%q: last flag = %+v, want a missing --memory value", in, last)
		}
	}
}

func TestParseRunFlagsBooleanFlagsNeverTakeTheNextWord(t *testing.T) {
	words, _ := shellWords("--init false")
	flags := parseRunFlags(words, runFlagTable)
	if len(flags) != 2 || flags[0].HasValue || flags[1].Name != "false" {
		t.Errorf("flags = %+v: a boolean flag took the next word as its value", flags)
	}
}

func TestRunFlagTableCoversDoc04Section5(t *testing.T) {
	want := []string{
		"--restart", "--memory", "--memory-swap", "--cpus", "--pids-limit", "--user", "--workdir", "--hostname",
		"--group-add", "--entrypoint", "--interactive", "--tty", "--init", "--read-only", "--device", "--cap-add",
		"--cap-drop", "--security-opt", "--sysctl", "--ulimit", "--dns", "--add-host", "--tmpfs", "--shm-size",
		"--runtime", "--gpus", "--log-opt", "--stop-timeout", "--health-cmd", "--health-interval", "--health-timeout",
		"--health-retries", "--health-start-period", "--no-healthcheck", "--volume", "--mount", "--publish", "--env",
		"--pid", "--cgroupns", "--device-cgroup-rule",
	}
	for _, f := range want {
		if _, ok := runFlagTable[f]; !ok {
			t.Errorf("%s is in doc 04 §5's translate table but not in the converter", f)
		}
	}
	if len(runFlagTable) != len(want) {
		t.Errorf("the converter translates %d flags, doc 04 §5 lists %d", len(runFlagTable), len(want))
	}
	for _, notInTable := range []string{"--label", "--env-file", "--privileged", "--cpu-shares", "--stop-signal"} {
		if _, ok := runFlagTable[notInTable]; ok {
			t.Errorf("%s is translated, but doc 04 §5 says it is an unknown flag", notInTable)
		}
	}
}
