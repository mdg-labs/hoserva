package container

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/distribution/reference"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
)

// engineTimeout bounds every Engine API call this package makes, so a
// stalled or hung daemon cannot hold an API request open forever —
// matching internal/api's own doctorProbeTimeout convention.
const engineTimeout = 8 * time.Second

// EngineClient is the real Provider: a Docker Engine API client with its
// API version negotiated at runtime (Q38, doc 04 §3), never a fixed
// "Engine 24+" pin. Construction never talks to the Engine — it only
// resolves DOCKER_HOST (or the default Unix socket) and builds an HTTP
// client — so hoservad can wire this unconditionally at startup and let
// Docker's absence surface as ErrUnavailable from the first real call
// (doc 04 §3: "hoservad starts regardless").
type EngineClient struct {
	cli *dockerclient.Client
}

// NewEngineClient returns an EngineClient talking to DOCKER_HOST, or the
// Engine's own default Unix socket when it is unset.
func NewEngineClient() (*EngineClient, error) {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("container: building docker client: %w", err)
	}
	return &EngineClient{cli: cli}, nil
}

// wrapEngineErr maps a connection failure (Docker not installed, not
// running, or the socket unreachable) to ErrUnavailable, so every caller
// can distinguish "Docker is not there" from a genuine request error
// (doc 04 §3) with a single errors.Is check, never a string match.
func wrapEngineErr(err error) error {
	if err == nil {
		return nil
	}
	if dockerclient.IsErrConnectionFailed(err) {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return err
}

// Version reports EngineVersion.APIVersion as the client's own negotiated
// API version (cli.ClientVersion(), Q38) — never v.APIVersion from
// ServerVersion's response, which is the server's own maximum supported
// version, not what NewEngineClient's WithAPIVersionNegotiation() actually
// settled on for every other call this Provider makes. ServerVersion's
// request is itself what triggers that negotiation (its first call resolves
// the API path through the same version check every other request does),
// so cli.ClientVersion() is already final once it returns here.
func (c *EngineClient) Version(ctx context.Context) (EngineVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	v, err := c.cli.ServerVersion(ctx)
	if err != nil {
		return EngineVersion{}, wrapEngineErr(err)
	}
	return EngineVersion{Version: v.Version, APIVersion: c.cli.ClientVersion(), MinAPIVersion: v.MinAPIVersion}, nil
}

func (c *EngineClient) List(ctx context.Context) ([]Container, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	summaries, err := c.cli.ContainerList(ctx, dockercontainer.ListOptions{All: true})
	if err != nil {
		return nil, wrapEngineErr(err)
	}
	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, containerFromSummary(s))
	}
	return out, nil
}

// Inspect matches id against every container's own ID or name, the same
// exact-match rule FakeProvider uses — a full Engine ID or the container's
// name (doc 03 §5.1's own /apps/[name] route), never a prefix.
func (c *EngineClient) Inspect(ctx context.Context, id string) (Container, error) {
	containers, err := c.List(ctx)
	if err != nil {
		return Container{}, err
	}
	for _, ct := range containers {
		if ct.ID == id || ct.Name == id {
			return ct, nil
		}
	}
	return Container{}, ErrNotFound
}

func (c *EngineClient) Images(ctx context.Context) ([]Image, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	summaries, err := c.cli.ImageList(ctx, dockerimage.ListOptions{})
	if err != nil {
		return nil, wrapEngineErr(err)
	}
	out := make([]Image, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, imageFromSummary(s))
	}
	return out, nil
}

var _ Provider = (*EngineClient)(nil)

// containerFromSummary maps one Engine container listing into Container —
// a pure function, so it is unit-testable against SDK struct literals
// without a live daemon.
func containerFromSummary(s dockercontainer.Summary) Container {
	name := ""
	if len(s.Names) > 0 {
		name = strings.TrimPrefix(s.Names[0], "/")
	}
	repo, tag := splitImageRef(s.Image)
	ports := make([]Port, 0, len(s.Ports))
	for _, p := range s.Ports {
		ports = append(ports, Port{
			HostIP:        p.IP,
			HostPort:      p.PublicPort,
			ContainerPort: p.PrivatePort,
			Protocol:      p.Type,
		})
	}
	mounts := make([]Mount, 0, len(s.Mounts))
	for _, m := range s.Mounts {
		mounts = append(mounts, Mount{
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        m.Mode,
			ReadWrite:   m.RW,
		})
	}
	return Container{
		ID:     s.ID,
		Name:   name,
		Image:  repo,
		Tag:    tag,
		State:  string(s.State),
		Status: s.Status,
		Ports:  ports,
		Mounts: mounts,
	}
}

// imageFromSummary maps one Engine image listing into Image.
func imageFromSummary(s dockerimage.Summary) Image {
	return Image{
		ID:       s.ID,
		RepoTags: s.RepoTags,
		Size:     s.Size,
		Created:  time.Unix(s.Created, 0).UTC(),
	}
}

// splitImageRef splits a Docker image reference (as the Engine reports it
// on a container or image listing) into its repository and tag, using the
// same reference parser the Engine and Compose themselves use rather than
// a hand-rolled split on ':' — which would mis-parse a registry host with
// a port, e.g. "registry.example.com:5000/app:latest". An unparseable or
// digest-only reference (no tag) returns the original string as the
// repository and an empty tag, never an error: this is display data, not
// something Inspect can refuse over.
func splitImageRef(image string) (repo, tag string) {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return image, ""
	}
	repo = reference.FamiliarName(ref)
	if tagged, ok := ref.(reference.NamedTagged); ok {
		tag = tagged.Tag()
	}
	return repo, tag
}
