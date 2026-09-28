package api

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
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
	apps := make([]apiv1.App, 0, len(containers))
	for _, c := range containers {
		apps = append(apps, containerToAPI(c))
	}
	return &apiv1.ListAppsOK{Available: true, Apps: apps}, nil
}

func (h *Handler) GetApp(ctx context.Context, params apiv1.GetAppParams) (*apiv1.App, error) {
	if h.Container == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "Docker is not configured on this daemon"}
	}
	c, err := h.Container.Inspect(ctx, params.ID)
	if err != nil {
		if errors.Is(err, container.ErrNotFound) {
			return nil, errAppNotFound(params.ID)
		}
		if errors.Is(err, container.ErrUnavailable) {
			return nil, &apiError{code: "docker_unavailable", statusCode: 503, message: fmt.Sprintf("Docker is not installed or not reachable: %v", err)}
		}
		return nil, fmt.Errorf("inspecting container %q: %w", params.ID, err)
	}
	app := containerToAPI(c)
	return &app, nil
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
