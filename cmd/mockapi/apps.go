package main

import (
	"context"
	"fmt"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// mockApps is deterministic per instance (doc 06 §8): every screen must be
// reachable with no real Docker daemon behind this mock. Both containers
// here are, honestly, unmanaged — this part builds no stacks table (#278
// is where "managed" starts meaning something), so nothing in this list
// claims otherwise.
func mockApps() []apiv1.App {
	return []apiv1.App{
		{
			ID:     "3f2a9c1e4b5d",
			Name:   "jellyfin",
			Image:  "lscr.io/linuxserver/jellyfin",
			Tag:    "10.9.7",
			State:  apiv1.AppStateRunning,
			Status: "Up 3 hours",
			Ports: []apiv1.AppPort{
				{
					HostIP:        apiv1.NewOptString("0.0.0.0"),
					HostPort:      apiv1.NewOptInt(8096),
					ContainerPort: 8096,
					Protocol:      apiv1.AppPortProtocolTCP,
				},
			},
			Mounts: []apiv1.AppMount{
				{
					Source:      apiv1.NewOptString("/mnt/cache/appdata/jellyfin"),
					Destination: "/config",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
				},
				{
					Source:      apiv1.NewOptString("/mnt/user/media"),
					Destination: "/data/media",
					Mode:        apiv1.NewOptString("ro"),
					ReadWrite:   false,
				},
			},
		},
		{
			ID:     "9b1d7e2f6a3c",
			Name:   "portainer",
			Image:  "portainer/portainer-ce",
			Tag:    "2.21.4",
			State:  apiv1.AppStateExited,
			Status: "Exited (0) 2 days ago",
			Ports:  []apiv1.AppPort{},
			Mounts: []apiv1.AppMount{
				{
					Source:      apiv1.NewOptString("/var/run/docker.sock"),
					Destination: "/var/run/docker.sock",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
				},
			},
		},
	}
}

func mockAppImages() []apiv1.AppImage {
	return []apiv1.AppImage{
		{
			ID:        "sha256:1a2b3c4d5e6f",
			RepoTags:  []string{"lscr.io/linuxserver/jellyfin:10.9.7"},
			SizeBytes: 456 * 1024 * 1024,
			CreatedAt: apiv1.NewOptDateTime(time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)),
		},
		{
			ID:        "sha256:6f5e4d3c2b1a",
			RepoTags:  []string{"portainer/portainer-ce:2.21.4"},
			SizeBytes: 289 * 1024 * 1024,
			CreatedAt: apiv1.NewOptDateTime(time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)),
		},
	}
}

func (h *handler) ListApps(ctx context.Context) (*apiv1.ListAppsOK, error) {
	return &apiv1.ListAppsOK{Available: true, Apps: mockApps()}, nil
}

func (h *handler) GetApp(ctx context.Context, params apiv1.GetAppParams) (*apiv1.App, error) {
	for _, app := range mockApps() {
		if app.ID == params.ID || app.Name == params.ID {
			return &app, nil
		}
	}
	return nil, &mockError{code: "app_not_found", statusCode: 404, message: fmt.Sprintf("no container %q", params.ID)}
}

func (h *handler) ListAppImages(ctx context.Context) (*apiv1.ListAppImagesOK, error) {
	return &apiv1.ListAppImagesOK{Available: true, Images: mockAppImages()}, nil
}
