package update

import (
	"errors"
	"testing"
)

func TestCompareDebianVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.1.0", 1},
		{"0.1.0", "0.2.0", -1},
		{"0.2.0", "0.2.0", 0},
		{"0.10.0", "0.9.0", 1},
		{"0.3.0", "0.3.0~beta.2", 1},
		{"0.3.0~beta.1", "0.3.0", -1},
		{"0.3.0~beta.2", "0.3.0~beta.10", -1},
		{"0.3.0~beta.1", "0.2.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.2.0-2", "0.2.0-1", 1},
		{"0.2.0-1", "0.2.0", 1},
		{"0.2.0+b1", "0.2.0", 1},
	} {
		got, err := compareDebianVersions(tc.a, tc.b)
		if err != nil || got != tc.want {
			t.Errorf("compareDebianVersions(%q, %q) = %d, %v; want %d", tc.a, tc.b, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "dev", "v0.2.0", "1:0.2.0", "0.2.0 ", "0.2.0/x"} {
		if _, err := compareDebianVersions(bad, "0.1.0"); err == nil {
			t.Errorf("compareDebianVersions(%q, ...) accepted an unreadable version", bad)
		}
		if _, err := compareDebianVersions("0.1.0", bad); err == nil {
			t.Errorf("compareDebianVersions(..., %q) accepted an unreadable version", bad)
		}
	}
}

func TestReleaseNaming(t *testing.T) {
	for version, tag := range map[string]string{
		"0.2.0":        "v0.2.0",
		"0.3.0~beta.1": "v0.3.0-beta.1",
	} {
		if !isReleaseVersion(version) || tagForVersion(version) != tag {
			t.Errorf("version %q: release=%v tag=%q, want tag %q", version, isReleaseVersion(version), tagForVersion(version), tag)
		}
	}
	for _, bad := range []string{"", "0.2", "0.2.0-1", "0.2.0~rc.1", "v0.2.0", "0.2.0~beta", "0.2.0~beta.1.2", "1:0.2.0"} {
		if isReleaseVersion(bad) {
			t.Errorf("isReleaseVersion(%q) = true", bad)
		}
	}
	if v, a, err := debFileVersion("hoserva_0.3.0~beta.1_arm64.deb"); err != nil || v != "0.3.0~beta.1" || a != "arm64" {
		t.Errorf("debFileVersion = %q, %q, %v", v, a, err)
	}
	for _, bad := range []string{"hoserva_0.2.0_amd64.deb.sig", "other_0.2.0_amd64.deb", "hoserva_0.2_amd64.deb", "hoserva_0.2.0.deb", "package.deb"} {
		if _, _, err := debFileVersion(bad); !errors.Is(err, ErrReleaseMismatch) {
			t.Errorf("debFileVersion(%q) = %v, want ErrReleaseMismatch", bad, err)
		}
	}
}
