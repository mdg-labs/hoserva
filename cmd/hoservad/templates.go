package main

import (
	"context"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/template"
)

// catalogDirName is the catalog directory inside the state directory
// (doc 04 §7): /var/lib/hoserva/catalog on an installed system, the dev
// daemon's own state directory otherwise.
const catalogDirName = "catalog"

// wireTemplateInstall is what main.go calls to make template install
// reachable: /templates/{id}/preview and /install (Handler.TemplateInstall).
// It installs through Handler.Stacks, so wireStacks has run first, and it
// reads the published ports of the same Engine the Apps operations use, so
// with no Docker service (apps nil) the operations stay 501 rather than
// treating every port as free.
func wireTemplateInstall(handler *api.Handler, stateDir string, apps *appServices, shares func(ctx context.Context) ([]string, error)) {
	if apps == nil || handler.Stacks == nil {
		return
	}
	handler.TemplateInstall = &template.Installer{
		Catalog:  template.DirCatalog{Root: filepath.Join(stateDir, catalogDirName), Source: template.SourceCurated},
		Stacks:   handler.Stacks,
		Ports:    template.HostPorts{Containers: apps.Lifecycle.Provider},
		Shares:   shares,
		GPU:      template.HostGPU{},
		Timezone: template.HostTimezone,
	}
}

// shareNames lists the names of the existing shares.
func shareNames(shares *share.Service) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		list, err := shares.List(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, len(list))
		for i, s := range list {
			names[i] = s.Name
		}
		return names, nil
	}
}
