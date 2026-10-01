package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/template"
)

// catalogDirName is the catalog directory inside the state directory
// (doc 04 §7): /var/lib/hoserva/catalog on an installed system, the dev
// daemon's own state directory otherwise.
const catalogDirName = "catalog"

// seedCatalog installs the curated catalog this build embeds as the on-disk
// copy when there is none or the one there is older (doc 04 §7), so a fresh
// install resolves its templates with no network. It runs the swap
// recovery first, verifies the embedded signature against the compiled-in
// catalog key every time, and leaves an equal or newer copy (a later refresh)
// alone.
func seedCatalog(stateDir string) error {
	archive, sig, err := template.EmbeddedSnapshot()
	if err != nil {
		return err
	}
	store := template.CatalogStore{Dir: filepath.Join(stateDir, catalogDirName)}
	if _, err := store.Seed(archive, sig); err != nil {
		return fmt.Errorf("installing the embedded catalog snapshot: %w", err)
	}
	return nil
}

// curatedCatalog is the curated catalog's on-disk copy, the one source
// registered and on by default (doc 04 §4).
func curatedCatalog(stateDir string) template.Catalog {
	return template.DirCatalog{Root: filepath.Join(stateDir, catalogDirName), Source: template.SourceCurated}
}

// wireCatalog is what makes the catalog operations reachable: /catalog,
// /catalog/{id} and /catalog/{id}/icon (Handler.Catalog). They read only the
// on-disk copy and need no Docker service.
func wireCatalog(handler *api.Handler, stateDir string) {
	handler.Catalog = curatedCatalog(stateDir)
}

// startTemplates is what main.go calls: it seeds the on-disk catalog from
// the embedded snapshot, then wires the catalog operations and template
// install over it. A seed that fails (a build with no snapshot, a snapshot
// that does not verify) is logged and does not stop the daemon: whatever
// catalog an earlier start or refresh left stays in use, and with none the
// catalog list is catalog_unavailable and every template is
// template_not_found.
func startTemplates(handler *api.Handler, stateDir string, apps *appServices, shares func(ctx context.Context) ([]string, error)) {
	if err := seedCatalog(stateDir); err != nil {
		log.Printf("hoservad: %v — template installs use only the catalog already in %s", err, filepath.Join(stateDir, catalogDirName))
	}
	wireCatalog(handler, stateDir)
	wireTemplateInstall(handler, stateDir, apps, shares)
}

// wireTemplateInstall is what main.go calls to make template install
// reachable: /templates/{id}/preview and /install (Handler.TemplateInstall).
// It installs through Handler.Stacks, so wireStacks has run first, and it
// reads the published ports of the same Engine the Apps operations use and of
// the stacks it installs into, so with no Docker service (apps nil) the
// operations stay 501 rather than treating every port as free.
func wireTemplateInstall(handler *api.Handler, stateDir string, apps *appServices, shares func(ctx context.Context) ([]string, error)) {
	if apps == nil || handler.Stacks == nil {
		return
	}
	handler.TemplateInstall = &template.Installer{
		Catalog:  curatedCatalog(stateDir),
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
