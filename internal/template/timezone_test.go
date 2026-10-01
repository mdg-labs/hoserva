package template

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTimezoneIsReadFromEtcTimezoneThenTheLocaltimeLink(t *testing.T) {
	etc := t.TempDir()
	if got := timezoneIn(etc); got != "" {
		t.Errorf("empty /etc: %q", got)
	}
	if err := os.Symlink("/usr/share/zoneinfo/Europe/Vienna", filepath.Join(etc, "localtime")); err != nil {
		t.Fatal(err)
	}
	if got := timezoneIn(etc); got != "Europe/Vienna" {
		t.Errorf("localtime link: %q", got)
	}
	if err := os.WriteFile(filepath.Join(etc, "timezone"), []byte("America/New_York\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := timezoneIn(etc); got != "America/New_York" {
		t.Errorf("/etc/timezone: %q", got)
	}
}
