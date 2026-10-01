package template

import (
	"os"
	"path/filepath"
	"strings"
)

// HostTimezone is the host's time zone name, or "" when it cannot be told:
// /etc/timezone, then the zoneinfo path /etc/localtime points to. It only
// reads.
func HostTimezone() string {
	return timezoneIn("/etc")
}

func timezoneIn(etc string) string {
	if data, err := os.ReadFile(filepath.Join(etc, "timezone")); err == nil {
		if tz := strings.TrimSpace(string(data)); tz != "" {
			return tz
		}
	}
	if target, err := os.Readlink(filepath.Join(etc, "localtime")); err == nil {
		if _, tz, ok := strings.Cut(target, "zoneinfo/"); ok {
			return tz
		}
	}
	return ""
}
