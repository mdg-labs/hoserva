package container

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	dockercontainer "github.com/moby/moby/api/types/container"
	dockerimage "github.com/moby/moby/api/types/image"
	dockerclient "github.com/moby/moby/client"
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
	cli engineAPI
}

// engineAPI is the part of the Docker SDK client EngineClient calls, so
// the multi-step Recreate can be tested against a scripted Engine.
type engineAPI interface {
	ServerVersion(ctx context.Context, options dockerclient.ServerVersionOptions) (dockerclient.ServerVersionResult, error)
	ClientVersion() string
	ContainerList(ctx context.Context, options dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error)
	ContainerCreate(ctx context.Context, options dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error)
	ContainerStop(ctx context.Context, containerID string, options dockerclient.ContainerStopOptions) (dockerclient.ContainerStopResult, error)
	ContainerRestart(ctx context.Context, containerID string, options dockerclient.ContainerRestartOptions) (dockerclient.ContainerRestartResult, error)
	ContainerRemove(ctx context.Context, containerID string, options dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error)
	ContainerRename(ctx context.Context, containerID string, options dockerclient.ContainerRenameOptions) (dockerclient.ContainerRenameResult, error)
	ContainerLogs(ctx context.Context, containerID string, options dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error)
	ContainerStats(ctx context.Context, containerID string, options dockerclient.ContainerStatsOptions) (dockerclient.ContainerStatsResult, error)
	NetworkList(ctx context.Context, options dockerclient.NetworkListOptions) (dockerclient.NetworkListResult, error)
	ImageList(ctx context.Context, options dockerclient.ImageListOptions) (dockerclient.ImageListResult, error)
	ImagePull(ctx context.Context, refStr string, options dockerclient.ImagePullOptions) (dockerclient.ImagePullResponse, error)
	ImageTag(ctx context.Context, options dockerclient.ImageTagOptions) (dockerclient.ImageTagResult, error)
	ImageRemove(ctx context.Context, imageID string, options dockerclient.ImageRemoveOptions) (dockerclient.ImageRemoveResult, error)
	Events(ctx context.Context, options dockerclient.EventsListOptions) dockerclient.EventsResult
}

var _ engineAPI = (*dockerclient.Client)(nil)

// NewEngineClient returns an EngineClient talking to DOCKER_HOST, or the
// Engine's own default Unix socket when it is unset.
func NewEngineClient() (*EngineClient, error) {
	cli, err := dockerclient.New(dockerclient.FromEnv)
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
// version, not what the client's default API version negotiation actually
// settled on for every other call this Provider makes. ServerVersion's
// request is itself what triggers that negotiation (its first call resolves
// the API path through the same version check every other request does),
// so cli.ClientVersion() is already final once it returns here.
func (c *EngineClient) Version(ctx context.Context) (EngineVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	v, err := c.cli.ServerVersion(ctx, dockerclient.ServerVersionOptions{})
	if err != nil {
		return EngineVersion{}, wrapEngineErr(err)
	}
	return EngineVersion{Version: v.Version, APIVersion: c.cli.ClientVersion(), MinAPIVersion: v.MinAPIVersion}, nil
}

func (c *EngineClient) List(ctx context.Context) ([]Container, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	res, err := c.cli.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return nil, wrapEngineErr(err)
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, containerFromSummary(s))
	}
	return out, nil
}

// Networks lists the Engine's networks by name and driver.
func (c *EngineClient) Networks(ctx context.Context) ([]Network, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	res, err := c.cli.NetworkList(ctx, dockerclient.NetworkListOptions{})
	if err != nil {
		return nil, wrapEngineErr(err)
	}
	out := make([]Network, 0, len(res.Items))
	for _, n := range res.Items {
		out = append(out, Network{Name: n.Name, Driver: n.Driver})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
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

// ConfiguredImage reads the reference from the container's own inspection,
// where it is always what the container was created with. It inspects id
// directly, without listing every container, since the update check calls it
// for each one; a match on an ID prefix, which the Engine accepts, is refused
// as Inspect refuses it.
func (c *EngineClient) ConfiguredImage(ctx context.Context, id string) (ConfiguredImage, error) {
	info, err := c.inspectEngine(ctx, id)
	if err != nil {
		return ConfiguredImage{}, err
	}
	name := strings.TrimPrefix(info.Name, "/")
	if info.ID != id && name != id {
		return ConfiguredImage{}, ErrNotFound
	}
	if info.Config == nil || info.Config.Image == "" {
		return ConfiguredImage{}, fmt.Errorf("container: the Engine returned no image reference for %q", name)
	}
	return configuredImage(info.Config.Image), nil
}

// ConfiguredPorts reads the host ports from the container's own inspection,
// where a stopped container still has the bindings it binds when it starts. A
// host port range expands to one Port per host port.
func (c *EngineClient) ConfiguredPorts(ctx context.Context, id string) ([]Port, error) {
	info, err := c.inspectEngine(ctx, id)
	if err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(info.Name, "/")
	if info.ID != id && name != id {
		return nil, ErrNotFound
	}
	if info.HostConfig == nil {
		return nil, fmt.Errorf("container: the Engine returned no host configuration for %q", name)
	}
	var out []Port
	for private, bindings := range info.HostConfig.PortBindings {
		for _, b := range bindings {
			lo, hi, err := parseHostPortRange(b.HostPort)
			if err != nil {
				return nil, fmt.Errorf("container: reading the published ports of %q: %w", name, err)
			}
			hostIP := ""
			if b.HostIP.IsValid() {
				hostIP = b.HostIP.String()
			}
			for p := lo; p <= hi; p++ {
				out = append(out, Port{HostIP: hostIP, HostPort: uint16(p), ContainerPort: private.Num(), Protocol: string(private.Proto())})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostPort != out[j].HostPort {
			return out[i].HostPort < out[j].HostPort
		}
		return out[i].ContainerPort < out[j].ContainerPort
	})
	return out, nil
}

// parseHostPortRange reads a binding's host port: empty or "0" (the Engine
// chooses one), a port, or a range "8000-8010". Both ends are 0 when the
// Engine chooses.
func parseHostPortRange(s string) (lo, hi int, err error) {
	first, last, isRange := strings.Cut(s, "-")
	if first == "" {
		return 0, 0, nil
	}
	if lo, err = strconv.Atoi(first); err != nil || lo < 0 || lo > 65535 {
		return 0, 0, fmt.Errorf("unexpected host port %q", s)
	}
	hi = lo
	if isRange {
		if hi, err = strconv.Atoi(last); err != nil || hi < lo || hi > 65535 {
			return 0, 0, fmt.Errorf("unexpected host port range %q", s)
		}
	}
	return lo, hi, nil
}

func (c *EngineClient) Images(ctx context.Context) ([]Image, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	res, err := c.cli.ImageList(ctx, dockerclient.ImageListOptions{})
	if err != nil {
		return nil, wrapEngineErr(err)
	}
	out := make([]Image, 0, len(res.Items))
	for _, s := range res.Items {
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
		hostIP := ""
		if p.IP.IsValid() {
			hostIP = p.IP.String()
		}
		ports = append(ports, Port{
			HostIP:        hostIP,
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
		ID:      s.ID,
		Name:    name,
		Image:   repo,
		Tag:     tag,
		ImageID: s.ImageID,
		Pinned:  isDigestPinned(s.Image),
		State:   string(s.State),
		Status:  s.Status,
		Health:  healthFromStatus(s.Status),
		Ports:   ports,
		Mounts:  mounts,
		Labels:  s.Labels,
	}
}

// imageFromSummary maps one Engine image listing into Image.
func imageFromSummary(s dockerimage.Summary) Image {
	return Image{
		ID:          s.ID,
		RepoTags:    s.RepoTags,
		RepoDigests: s.RepoDigests,
		Size:        s.Size,
		Created:     time.Unix(s.Created, 0).UTC(),
	}
}

// isDigestPinned reports whether an image reference names a digest, with or
// without a tag.
func isDigestPinned(image string) bool {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false
	}
	_, ok := ref.(reference.Digested)
	return ok
}

// splitImageRef splits a Docker image reference (as the Engine reports it
// on a container or image listing) into its repository and tag, using the
// same reference parser the Engine and Compose themselves use rather than
// a hand-rolled split on ':' — which would mis-parse a registry host with
// a port, e.g. "registry.example.com:5000/app:latest". An unparseable or
// digest-only reference has an empty tag (the digest is dropped from the
// repository); a reference that does not parse comes back unchanged as the
// repository with an empty tag. Never an error: this is display data, not
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
