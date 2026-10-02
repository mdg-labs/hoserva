package api

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

func errAppNotFound(id string) error {
	return &apiError{code: "app_not_found", statusCode: 404, message: fmt.Sprintf("no container %q", id)}
}

// unavailableApps is doc 04 §3's own normal state: Docker not installed or
// not reachable. It is reported through available=false, never an HTTP
// error — a missing prerequisite is Apps' expected state on a fresh
// install, not a failed request (CLAUDE.md's UI-states rule: a failed
// request must never look like "no containers", and the reverse holds
// here too — "no containers" from a real Engine must never look like a
// failed request).
func unavailableAppsMessage(err error) apiv1.OptString {
	return apiv1.NewOptString(fmt.Sprintf("Docker is not installed or not reachable: %v", err))
}

func (h *Handler) ListApps(ctx context.Context) (*apiv1.ListAppsOK, error) {
	if h.Container == nil {
		return &apiv1.ListAppsOK{Available: false, Message: apiv1.NewOptString("Docker is not configured on this daemon")}, nil
	}
	containers, err := h.Container.List(ctx)
	if err != nil {
		if errors.Is(err, container.ErrUnavailable) {
			return &apiv1.ListAppsOK{Available: false, Message: unavailableAppsMessage(err)}, nil
		}
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	stacks, err := h.managingStacks(ctx, containers)
	if err != nil {
		return nil, err
	}
	disks, err := h.arrayDisks(ctx)
	if err != nil {
		return nil, err
	}
	apps := make([]apiv1.App, 0, len(containers))
	for _, c := range containers {
		apps = append(apps, withMountLocations(withStack(containerToAPI(c), stacks[c.ID]), disks))
	}
	return &apiv1.ListAppsOK{Available: true, Apps: apps}, nil
}

// managingStacks maps container ID to the installed stack that owns it. With
// no stack service no stack can have started anything, so nothing is
// managed; a stack service that cannot say fails the request, so an error
// never reads as "every container is unmanaged".
func (h *Handler) managingStacks(ctx context.Context, containers []container.Container) (map[string]string, error) {
	if h.Stacks == nil {
		return nil, nil
	}
	stacks, err := h.Stacks.ManagingStacks(ctx, containers)
	if err != nil {
		return nil, fmt.Errorf("finding which containers a stack manages: %w", err)
	}
	return stacks, nil
}

// arrayDisks is the stored array topology that classifies a mount's storage,
// read from the database, never from a disk. With no array, or no store, no
// disk is known, so only the pool's own path classifies; a store that cannot
// answer fails the request, so an error never reads as "outside the array".
func (h *Handler) arrayDisks(ctx context.Context) ([]store.ArrayDisk, error) {
	if h.ArrayStore == nil {
		return nil, nil
	}
	_, disks, err := h.ArrayStore.GetArray(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading the array topology to place container mounts: %w", err)
	}
	return disks, nil
}

// withMountLocations sets each mount that has a host path to the storage it
// lies on. It builds a new mounts slice, leaving the one app came with alone.
func withMountLocations(app apiv1.App, disks []store.ArrayDisk) apiv1.App {
	mounts := make([]apiv1.AppMount, len(app.Mounts))
	for i, m := range app.Mounts {
		if src, ok := m.Source.Get(); ok && src != "" {
			m.Location = apiv1.NewOptAppMountLocation(mountLocationToAPI(container.ClassifyMount(src, disks)))
		}
		mounts[i] = m
	}
	app.Mounts = mounts
	return app
}

func mountLocationToAPI(l container.MountLocation) apiv1.AppMountLocation {
	out := apiv1.AppMountLocation{Kind: apiv1.AppMountLocationKind(l.Kind)}
	if l.Share != "" {
		out.Share = apiv1.NewOptString(l.Share)
	}
	if l.Disk != 0 {
		out.Disk = apiv1.NewOptInt(l.Disk)
	}
	return out
}

func withStack(app apiv1.App, stack string) apiv1.App {
	if stack != "" {
		app.Stack = apiv1.NewOptString(stack)
	}
	return app
}

func (h *Handler) GetApp(ctx context.Context, params apiv1.GetAppParams) (*apiv1.App, error) {
	if h.Container == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "Docker is not configured on this daemon"}
	}
	c, err := h.Container.Inspect(ctx, params.ID)
	if err != nil {
		return nil, mapInspectError(params.ID, err)
	}
	rt, err := h.Container.Runtime(ctx, c.ID)
	if err != nil {
		return nil, mapInspectError(params.ID, err)
	}
	stacks, err := h.managingStacks(ctx, []container.Container{c})
	if err != nil {
		return nil, err
	}
	disks, err := h.arrayDisks(ctx)
	if err != nil {
		return nil, err
	}
	app := withRuntime(withMountLocations(withStack(containerToAPI(c), stacks[c.ID]), disks), rt)
	return &app, nil
}

func mapInspectError(id string, err error) error {
	if errors.Is(err, container.ErrNotFound) {
		return errAppNotFound(id)
	}
	if errors.Is(err, container.ErrUnavailable) {
		return &apiError{code: "docker_unavailable", statusCode: 503, message: fmt.Sprintf("Docker is not installed or not reachable: %v", err)}
	}
	return fmt.Errorf("inspecting container %q: %w", id, err)
}

func withRuntime(app apiv1.App, rt container.Runtime) apiv1.App {
	app.CreatedAt = apiv1.NewOptDateTime(rt.CreatedAt)
	if !rt.StartedAt.IsZero() {
		app.StartedAt = apiv1.NewOptDateTime(rt.StartedAt)
	}
	app.RestartCount = apiv1.NewOptInt(rt.RestartCount)
	return app
}

func (h *Handler) ListAppImages(ctx context.Context) (*apiv1.ListAppImagesOK, error) {
	if h.Container == nil {
		return &apiv1.ListAppImagesOK{Available: false, Message: apiv1.NewOptString("Docker is not configured on this daemon")}, nil
	}
	images, err := h.Container.Images(ctx)
	if err != nil {
		if errors.Is(err, container.ErrUnavailable) {
			return &apiv1.ListAppImagesOK{Available: false, Message: unavailableAppsMessage(err)}, nil
		}
		return nil, fmt.Errorf("listing images: %w", err)
	}
	out := make([]apiv1.AppImage, 0, len(images))
	for _, img := range images {
		out = append(out, imageToAPI(img))
	}
	return &apiv1.ListAppImagesOK{Available: true, Images: out}, nil
}

func (h *Handler) ListDockerNetworks(ctx context.Context) (*apiv1.ListDockerNetworksOK, error) {
	if h.Container == nil {
		return &apiv1.ListDockerNetworksOK{Available: false, Networks: []apiv1.DockerNetwork{}, Message: apiv1.NewOptString("Docker is not configured on this daemon")}, nil
	}
	networks, err := h.Container.Networks(ctx)
	if err != nil {
		if errors.Is(err, container.ErrUnavailable) {
			return &apiv1.ListDockerNetworksOK{Available: false, Networks: []apiv1.DockerNetwork{}, Message: unavailableAppsMessage(err)}, nil
		}
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	out := make([]apiv1.DockerNetwork, len(networks))
	for i, n := range networks {
		out[i] = apiv1.DockerNetwork{Name: n.Name, Driver: n.Driver}
	}
	return &apiv1.ListDockerNetworksOK{Available: true, Networks: out}, nil
}

func containerToAPI(c container.Container) apiv1.App {
	ports := make([]apiv1.AppPort, 0, len(c.Ports))
	for _, p := range c.Ports {
		port := apiv1.AppPort{
			ContainerPort: int(p.ContainerPort),
			Protocol:      apiv1.AppPortProtocol(p.Protocol),
		}
		if p.HostIP != "" {
			port.HostIP = apiv1.NewOptString(p.HostIP)
		}
		if p.HostPort != 0 {
			port.HostPort = apiv1.NewOptInt(int(p.HostPort))
		}
		ports = append(ports, port)
	}
	mounts := make([]apiv1.AppMount, 0, len(c.Mounts))
	for _, m := range c.Mounts {
		mount := apiv1.AppMount{Destination: m.Destination, ReadWrite: m.ReadWrite}
		if m.Source != "" {
			mount.Source = apiv1.NewOptString(m.Source)
		}
		if m.Mode != "" {
			mount.Mode = apiv1.NewOptString(m.Mode)
		}
		mounts = append(mounts, mount)
	}
	return apiv1.App{
		ID:     c.ID,
		Name:   c.Name,
		Image:  c.Image,
		Tag:    c.Tag,
		State:  apiv1.AppState(c.State),
		Status: c.Status,
		Health: appHealth(c.Health),
		Ports:  ports,
		Mounts: mounts,
	}
}

// appHealth reports a container with no health information as "none",
// the Engine's own word for it.
func appHealth(h string) apiv1.AppHealth {
	if h == "" {
		return apiv1.AppHealthNone
	}
	return apiv1.AppHealth(h)
}

func imageToAPI(img container.Image) apiv1.AppImage {
	out := apiv1.AppImage{ID: img.ID, RepoTags: img.RepoTags, SizeBytes: img.Size}
	if !img.Created.IsZero() {
		out.CreatedAt = apiv1.NewOptDateTime(img.Created)
	}
	return out
}
