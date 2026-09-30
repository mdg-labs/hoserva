package container

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	dockerimage "github.com/moby/moby/api/types/image"
	dockerclient "github.com/moby/moby/client"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestEngineClient_Version_ReportsClientNegotiatedAPIVersion proves
// EngineVersion.APIVersion is the client's own version — cli.ClientVersion()
// — never the server's ApiVersion field from the /version response, which
// is the server's own maximum, not what this client actually negotiated
// (Q38). Fixed via WithAPIVersion rather than a full negotiation handshake:
// that alone is enough to show Version() reads the client's version, not
// the server's, since a manual override never changes to match the server.
func TestEngineClient_Version_ReportsClientNegotiatedAPIVersion(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"Version":"27.3.1","ApiVersion":"1.56","MinAPIVersion":"1.24"}`
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	cli, err := dockerclient.New(
		dockerclient.WithHTTPClient(&http.Client{Transport: rt}),
		dockerclient.WithAPIVersion("1.51"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := &EngineClient{cli: cli}

	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.APIVersion != "1.51" {
		t.Fatalf("APIVersion = %q, want the client's own version 1.51, not the server's maximum 1.56", v.APIVersion)
	}
	if v.Version != "27.3.1" {
		t.Fatalf("Version = %q, want 27.3.1", v.Version)
	}
}

// TestEngineClient_Version_NegotiatesDownToTheEngine drives the client
// NewEngineClient builds (no fixed version) through the negotiation
// handshake: the Engine's /_ping advertises its API version and the client
// settles on the lower of that and its own maximum, for the Engines at and
// above the doctor floor (29.5.1) and for older ones still within the
// client's supported range, never erroring for an Engine older than the
// client.
func TestEngineClient_Version_NegotiatesDownToTheEngine(t *testing.T) {
	cases := []struct {
		name       string
		pingAPI    string
		wantAPI    string
		wantEngine string
	}{
		{"29.5.1 floor engine", "1.54", "1.54", "29.5.1"},
		{"older engine below the floor", "1.44", "1.44", "24.0.7"},
		{"oldest supported engine", dockerclient.MinAPIVersion, dockerclient.MinAPIVersion, "19.03.15"},
		{"engine newer than the client", "1.99", dockerclient.MaxAPIVersion, "99.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pinged bool
			rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				h := http.Header{"Content-Type": []string{"application/json"}}
				body := ""
				switch {
				case strings.HasSuffix(req.URL.Path, "/_ping"):
					pinged = true
					h.Set("Api-Version", tc.pingAPI)
				case strings.HasSuffix(req.URL.Path, "/version"):
					if want := "/v" + tc.wantAPI + "/version"; req.URL.Path != want {
						t.Errorf("/version requested at %q, want the negotiated %q", req.URL.Path, want)
					}
					body = `{"Version":"` + tc.wantEngine + `","ApiVersion":"` + tc.pingAPI + `"}`
				default:
					t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: h}, nil
			})
			cli, err := dockerclient.New(dockerclient.WithHTTPClient(&http.Client{Transport: rt}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			c := &EngineClient{cli: cli}
			v, err := c.Version(context.Background())
			if err != nil {
				t.Fatalf("Version: %v", err)
			}
			if !pinged {
				t.Fatal("the client never negotiated: no /_ping request was made")
			}
			if v.APIVersion != tc.wantAPI || v.Version != tc.wantEngine {
				t.Fatalf("Version() = %+v, want API %s on Engine %s", v, tc.wantAPI, tc.wantEngine)
			}
		})
	}
}

func TestNewEngineClient_NeverDials(t *testing.T) {
	// Construction only resolves DOCKER_HOST (or the default socket) and
	// builds an HTTP client — it never talks to the Engine — so a caller
	// can wire this unconditionally at startup with Docker not installed
	// (doc 04 §3: "hoservad starts regardless"). If this dialed, it would
	// fail immediately in this sandbox, where no Docker socket exists.
	c, err := NewEngineClient()
	if err != nil {
		t.Fatalf("NewEngineClient: %v", err)
	}
	if c == nil || c.cli == nil {
		t.Fatal("NewEngineClient returned a nil client")
	}
}

func TestContainerFromSummary(t *testing.T) {
	s := dockercontainer.Summary{
		ID:     "abc123",
		Names:  []string{"/jellyfin"},
		Image:  "lscr.io/linuxserver/jellyfin:10.9.7",
		State:  dockercontainer.StateRunning,
		Status: "Up 3 hours",
		Ports: []dockercontainer.PortSummary{
			{IP: netip.MustParseAddr("0.0.0.0"), PrivatePort: 8096, PublicPort: 8096, Type: "tcp"},
			{PrivatePort: 53, Type: "udp"},
		},
		Mounts: []dockercontainer.MountPoint{
			{Source: "/mnt/cache/appdata/jellyfin", Destination: "/config", Mode: "rw", RW: true},
		},
		Labels: map[string]string{composeProjectLabel: "jellyfin"},
	}

	got := containerFromSummary(s)
	if got.Labels[composeProjectLabel] != "jellyfin" {
		t.Fatalf("containerFromSummary().Labels = %v, want the Compose project label", got.Labels)
	}

	want := Container{
		ID:     "abc123",
		Name:   "jellyfin",
		Image:  "lscr.io/linuxserver/jellyfin",
		Tag:    "10.9.7",
		State:  "running",
		Status: "Up 3 hours",
		Ports: []Port{
			{HostIP: "0.0.0.0", HostPort: 8096, ContainerPort: 8096, Protocol: "tcp"},
			{ContainerPort: 53, Protocol: "udp"},
		},
		Mounts: []Mount{
			{Source: "/mnt/cache/appdata/jellyfin", Destination: "/config", Mode: "rw", ReadWrite: true},
		},
	}
	if got.ID != want.ID || got.Name != want.Name || got.Image != want.Image || got.Tag != want.Tag ||
		got.State != want.State || got.Status != want.Status {
		t.Fatalf("containerFromSummary() = %+v, want %+v", got, want)
	}
	if len(got.Ports) != 2 || got.Ports[0] != want.Ports[0] || got.Ports[1] != want.Ports[1] {
		t.Fatalf("containerFromSummary().Ports = %+v, want %+v", got.Ports, want.Ports)
	}
	if len(got.Mounts) != 1 || got.Mounts[0] != want.Mounts[0] {
		t.Fatalf("containerFromSummary().Mounts = %+v, want %+v", got.Mounts, want.Mounts)
	}
}

func TestContainerFromSummary_NoName(t *testing.T) {
	got := containerFromSummary(dockercontainer.Summary{ID: "x", Image: "alpine"})
	if got.Name != "" {
		t.Fatalf("Name = %q, want empty for a container with no Names", got.Name)
	}
	if got.Image != "alpine" || got.Tag != "" {
		t.Fatalf("Image/Tag = %q/%q, want alpine/\"\" (no tag defaults to latest, never invented here)", got.Image, got.Tag)
	}
}

func TestImageFromSummary(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got := imageFromSummary(dockerimage.Summary{
		ID:       "sha256:deadbeef",
		RepoTags: []string{"alpine:3.20"},
		Size:     1234,
		Created:  created.Unix(),
	})
	if got.ID != "sha256:deadbeef" || got.Size != 1234 || len(got.RepoTags) != 1 || got.RepoTags[0] != "alpine:3.20" {
		t.Fatalf("imageFromSummary() = %+v", got)
	}
	if !got.Created.Equal(created) {
		t.Fatalf("Created = %v, want %v", got.Created, created)
	}
}

func TestSplitImageRef(t *testing.T) {
	cases := []struct {
		image    string
		wantRepo string
		wantTag  string
	}{
		{"alpine:3.20", "alpine", "3.20"},
		{"lscr.io/linuxserver/jellyfin:10.9.7", "lscr.io/linuxserver/jellyfin", "10.9.7"},
		{"registry.example.com:5000/app:latest", "registry.example.com:5000/app", "latest"},
		{"alpine", "alpine", ""},
	}
	for _, tc := range cases {
		repo, tag := splitImageRef(tc.image)
		if repo != tc.wantRepo || tag != tc.wantTag {
			t.Errorf("splitImageRef(%q) = (%q, %q), want (%q, %q)", tc.image, repo, tag, tc.wantRepo, tc.wantTag)
		}
	}
}

func TestSplitImageRef_Unparseable(t *testing.T) {
	// An invalid reference (uppercase is not a legal repository component)
	// must never be treated as an error the caller has to handle — this is
	// display data for a listing, never a refusal.
	repo, tag := splitImageRef("Not A Valid Ref!!")
	if repo != "Not A Valid Ref!!" || tag != "" {
		t.Fatalf("splitImageRef(unparseable) = (%q, %q), want the original string back with no tag", repo, tag)
	}
}
