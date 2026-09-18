package parity

import (
	"reflect"
	"testing"
)

// These are pure argv-construction tests: no exec, no shell — every
// operation's own tail is checked as a plain []string (CLAUDE.md's
// "never interpolate user or template input into a shell command",
// confirmed structurally: there is no string concatenation anywhere in
// this package's command-building path).

func TestSyncArgv(t *testing.T) {
	if got, want := syncArgv(false), []string{"sync"}; !reflect.DeepEqual(got, want) {
		t.Errorf("syncArgv(false) = %v, want %v", got, want)
	}
	if got, want := syncArgv(true), []string{"-E", "sync"}; !reflect.DeepEqual(got, want) {
		t.Errorf("syncArgv(true) = %v, want %v", got, want)
	}
}

func TestScrubArgv(t *testing.T) {
	got := scrubArgv(8, DefaultScrubOlderThanDays)
	want := []string{"-p", "8", "-o", "10", "scrub"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scrubArgv(8, 10) = %v, want %v", got, want)
	}
}

func TestFixArgv(t *testing.T) {
	cases := []struct {
		name string
		opts FixOpts
		want []string
	}{
		{"whole disk", FixOpts{Disk: "d2"}, []string{"-d", "d2", "fix"}},
		{"single path", FixOpts{Path: "/docs/b.bin"}, []string{"-f", "/docs/b.bin", "fix"}},
		{"errors only", FixOpts{ErrorsOnly: true}, []string{"-e", "fix"}},
		{"disk plus errors only", FixOpts{Disk: "d3", ErrorsOnly: true}, []string{"-d", "d3", "-e", "fix"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fixArgv(c.opts); !reflect.DeepEqual(got, c.want) {
				t.Errorf("fixArgv(%+v) = %v, want %v", c.opts, got, c.want)
			}
		})
	}
}

func TestCheckArgv(t *testing.T) {
	cases := []struct {
		name string
		opts CheckOpts
		want []string
	}{
		{"whole array", CheckOpts{}, []string{"check"}},
		{"one disk audit-only", CheckOpts{Disk: "d1", AuditOnly: true}, []string{"-d", "d1", "-a", "check"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := checkArgv(c.opts); !reflect.DeepEqual(got, c.want) {
				t.Errorf("checkArgv(%+v) = %v, want %v", c.opts, got, c.want)
			}
		})
	}
}

func TestDiffStatusTouchArgv(t *testing.T) {
	if got, want := diffArgv(), []string{"diff"}; !reflect.DeepEqual(got, want) {
		t.Errorf("diffArgv() = %v, want %v", got, want)
	}
	if got, want := statusArgv(), []string{"status"}; !reflect.DeepEqual(got, want) {
		t.Errorf("statusArgv() = %v, want %v", got, want)
	}
	if got, want := touchArgv(), []string{"touch"}; !reflect.DeepEqual(got, want) {
		t.Errorf("touchArgv() = %v, want %v", got, want)
	}
}
