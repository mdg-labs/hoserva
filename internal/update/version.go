package update

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	releaseVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(~beta\.[0-9]+)?$`)
	debVersionRE     = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~-]*$`)
	debFileNameRE    = regexp.MustCompile(`^hoserva_([^_]+)_([a-z0-9]+)\.deb$`)
)

// isReleaseVersion reports whether v is a version the release job
// produces: X.Y.Z, or X.Y.Z~beta.N for a beta (scripts/release/lib.sh).
func isReleaseVersion(v string) bool {
	return releaseVersionRE.MatchString(v)
}

func channelForVersion(v string) Channel {
	if strings.Contains(v, "~") {
		return ChannelBeta
	}
	return ChannelStable
}

// tagForVersion is the git tag a release version is built from: a
// beta's '~' is a '-' in the tag, and every tag has a leading 'v'.
func tagForVersion(v string) string {
	return "v" + strings.Replace(v, "~beta.", "-beta.", 1)
}

// debFileVersion returns the version and architecture a release package
// file name (hoserva_<version>_<arch>.deb) declares.
func debFileVersion(name string) (version, arch string, err error) {
	m := debFileNameRE.FindStringSubmatch(name)
	if m == nil || !isReleaseVersion(m[1]) {
		return "", "", fmt.Errorf("%w: %q is not a release package file name", ErrReleaseMismatch, name)
	}
	return m[1], m[2], nil
}

// compareDebianVersions compares two Debian package versions with dpkg's
// ordering (upstream version, then revision; '~' sorts before
// everything). A version carrying an epoch or characters Debian policy
// does not allow is an error, so a comparison never passes on input it
// could not read.
func compareDebianVersions(a, b string) (int, error) {
	for _, v := range []string{a, b} {
		if !debVersionRE.MatchString(v) {
			return 0, fmt.Errorf("update: %q is not a comparable Debian version", v)
		}
	}
	au, ar := splitRevision(a)
	bu, br := splitRevision(b)
	if c := compareDebianPart(au, bu); c != 0 {
		return c, nil
	}
	return compareDebianPart(ar, br), nil
}

func splitRevision(v string) (upstream, revision string) {
	if i := strings.LastIndex(v, "-"); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

func debOrder(c byte) int {
	switch {
	case c == '~':
		return -1
	case c >= '0' && c <= '9':
		return 0
	case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		return int(c)
	default:
		return int(c) + 256
	}
}

func compareDebianPart(a, b string) int {
	for len(a) > 0 || len(b) > 0 {
		for (len(a) > 0 && !isDigit(a[0])) || (len(b) > 0 && !isDigit(b[0])) {
			var ac, bc int
			if len(a) > 0 && !isDigit(a[0]) {
				ac = debOrder(a[0])
			}
			if len(b) > 0 && !isDigit(b[0]) {
				bc = debOrder(b[0])
			}
			if ac != bc {
				if ac < bc {
					return -1
				}
				return 1
			}
			if len(a) > 0 && !isDigit(a[0]) {
				a = a[1:]
			}
			if len(b) > 0 && !isDigit(b[0]) {
				b = b[1:]
			}
		}
		var an, bn string
		an, a = leadingDigits(a)
		bn, b = leadingDigits(b)
		an = strings.TrimLeft(an, "0")
		bn = strings.TrimLeft(bn, "0")
		if len(an) != len(bn) {
			if len(an) < len(bn) {
				return -1
			}
			return 1
		}
		if an != bn {
			if an < bn {
				return -1
			}
			return 1
		}
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func leadingDigits(s string) (digits, rest string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i], s[i:]
}
