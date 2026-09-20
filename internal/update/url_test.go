package update

import (
	"errors"
	"net/url"
	"testing"
)

func TestAllowURL_RejectsSubstringAndAPIHosts(t *testing.T) {
	e := &Engine{IndexURL: DefaultIndexURL}
	ok := []string{
		DefaultIndexURL,
		"https://github.com/mdg-labs/hoserva/releases/download/v0.2.0/hoserva_0.2.0_amd64.deb",
		"https://github.com/mdg-labs/hoserva/releases/download/v0.2.0/SHA256SUMS",
	}
	for _, raw := range ok {
		if err := e.allowURL(raw); err != nil {
			t.Errorf("allowURL(%q) = %v, want nil", raw, err)
		}
	}
	denied := []string{
		"https://attacker.example/github.com/x/releases/download/pkg.deb",
		"https://github.com.attacker.example/mdg-labs/hoserva/releases/download/v0.2.0/pkg.deb",
		"https://api.github.com/repos/mdg-labs/hoserva/releases",
		"https://api.github.com.attacker.example/releases/download/pkg.deb",
		"http://github.com/mdg-labs/hoserva/releases/download/v0.2.0/pkg.deb",
		"https://github.com/mdg-labs/hoserva/archive/refs/tags/v0.2.0.tar.gz",
		"https://objects.githubusercontent.com/github-production-release-asset/pkg.deb",
	}
	for _, raw := range denied {
		err := e.allowURL(raw)
		if err == nil || !errors.Is(err, ErrIndexURL) {
			t.Errorf("allowURL(%q) = %v, want ErrIndexURL", raw, err)
		}
	}
}

func TestAllowFetchHop_AllowsGitHubCDNAndSameHost(t *testing.T) {
	orig := mustURL(t, "https://github.com/mdg-labs/hoserva/releases/download/v0.2.0/pkg.deb")
	index := mustURL(t, DefaultIndexURL)

	ok := []struct {
		next string
		from *url.URL
	}{
		{"https://objects.githubusercontent.com/github-production-release-asset/pkg.deb", orig},
		{"https://release-assets.githubusercontent.com/github-production-release-asset/pkg.deb", orig},
		{"https://github.com/mdg-labs/hoserva/releases/download/v0.2.0/pkg.deb", orig},
		{DefaultIndexURL, index},
		{"https://hoserva.dev/releases/index.json?v=2", index},
	}
	for _, tc := range ok {
		if err := allowFetchHop(mustURL(t, tc.next), tc.from); err != nil {
			t.Errorf("allowFetchHop(%q) = %v, want nil", tc.next, err)
		}
	}

	denied := []struct {
		next string
		from *url.URL
	}{
		{"https://api.github.com/repos/mdg-labs/hoserva/releases", orig},
		{"https://attacker.example/pkg.deb", orig},
		{"http://objects.githubusercontent.com/pkg.deb", orig},
		{"https://api.github.com/repos/x", index},
	}
	for _, tc := range denied {
		err := allowFetchHop(mustURL(t, tc.next), tc.from)
		if err == nil || !errors.Is(err, ErrIndexURL) {
			t.Errorf("allowFetchHop(%q) = %v, want ErrIndexURL", tc.next, err)
		}
	}
}

func TestRefuseAPIGitHub_ParsesHost(t *testing.T) {
	if err := refuseAPIGitHub("https://github.com/mdg-labs/hoserva/releases/download/v0.2.0/pkg.deb"); err != nil {
		t.Fatalf("github.com download refused: %v", err)
	}
	if err := refuseAPIGitHub("https://attacker.example/api.github.com/x"); err != nil {
		t.Fatalf("path containing api.github.com refused: %v", err)
	}
	if err := refuseAPIGitHub("https://api.github.com/repos/x"); !errors.Is(err, ErrIndexURL) {
		t.Fatalf("api.github.com host = %v, want ErrIndexURL", err)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
