package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/notify"
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

// catalogPublisher is the one notify.Service method a failed catalog check
// needs; tests use a fake.
type catalogPublisher interface {
	Publish(ctx context.Context, event notify.EventType, title, message string) error
}

// newCatalogRefresher is the catalog check (doc 04 §7): one conditional
// request to the compiled-in catalog host, installed into the same on-disk
// copy the catalog operations read. A check that fails verification (a bad
// signature, a replayed serial, a malformed archive) publishes
// notify.EventCatalogCheckFailed; a network failure does not. With no
// notifier nothing is published.
func newCatalogRefresher(stateDir string, notifier catalogPublisher) *template.Refresher {
	r := &template.Refresher{Store: template.CatalogStore{Dir: filepath.Join(stateDir, catalogDirName)}}
	if notifier != nil {
		r.Notify = func(ctx context.Context, res template.CheckResult) error {
			return notifier.Publish(ctx, notify.EventCatalogCheckFailed, "Catalog check failed",
				fmt.Sprintf("The catalog update was refused and the installed catalog is unchanged: %s", res.Message))
		}
	}
	return r
}

// wireCatalog is what makes the catalog operations reachable: /catalog,
// /catalog/{id} and /catalog/{id}/icon (Handler.Catalog) read only the
// on-disk copy and need no Docker service, and /catalog/refresh
// (Handler.CatalogRefresh) runs the check that replaces it.
func wireCatalog(handler *api.Handler, stateDir string, notifier catalogPublisher) {
	handler.Catalog = curatedCatalog(stateDir)
	handler.CatalogRefresh = newCatalogRefresher(stateDir, notifier)
}

// startTemplates is what main.go calls: it seeds the on-disk catalog from
// the embedded snapshot, then wires the catalog operations (with the catalog
// check, which publishes through notifier) and template install over it. A
// seed that fails (a build with no snapshot, a snapshot that does not verify)
// is logged and does not stop the daemon: whatever catalog an earlier start or
// refresh left stays in use, and with none the catalog list is
// catalog_unavailable and every template is template_not_found.
func startTemplates(handler *api.Handler, stateDir string, apps *appServices, shares func(ctx context.Context) ([]string, error), notifier catalogPublisher) {
	if err := seedCatalog(stateDir); err != nil {
		log.Printf("hoservad: %v — template installs use only the catalog already in %s", err, filepath.Join(stateDir, catalogDirName))
	}
	wireCatalog(handler, stateDir, notifier)
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
