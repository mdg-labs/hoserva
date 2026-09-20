package update

import (
	"fmt"
	"net/url"
	"strings"
)

func parseHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: %s", ErrIndexURL, raw)
	}
	return u, nil
}

func hostnameIs(u *url.URL, host string) bool {
	return strings.EqualFold(u.Hostname(), host)
}

// refuseAPIGitHub rejects a URL whose host is api.github.com. It parses
// the host instead of matching a substring, so api.github.com.example is
// not confused with the API and a path that merely contains the name is
// not treated as the API.
func refuseAPIGitHub(raw string) error {
	u, err := parseHTTPSURL(raw)
	if err != nil {
		return err
	}
	if hostnameIs(u, "api.github.com") {
		return fmt.Errorf("%w: %s", ErrIndexURL, raw)
	}
	return nil
}

// allowFetchHop validates a redirect target. GitHub Releases download
// URLs 302 onto *.githubusercontent.com; requiring github.com on every
// hop would refuse a real package. The hop must be HTTPS, must not be
// api.github.com, and must stay on github.com, a githubusercontent.com
// CDN host, or the original request's host (the signed index).
func allowFetchHop(next, original *url.URL) error {
	if next == nil || next.Scheme != "https" || next.Hostname() == "" {
		raw := ""
		if next != nil {
			raw = next.String()
		}
		return fmt.Errorf("%w: %s", ErrIndexURL, raw)
	}
	host := strings.ToLower(next.Hostname())
	if host == "api.github.com" {
		return fmt.Errorf("%w: %s", ErrIndexURL, next.String())
	}
	if host == "github.com" || strings.HasSuffix(host, ".githubusercontent.com") {
		return nil
	}
	if original != nil && strings.EqualFold(original.Hostname(), host) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrIndexURL, next.String())
}
