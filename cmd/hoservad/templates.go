package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"
)

// catalogDirName is the catalog directory inside the state directory
// (doc 04 §7): /var/lib/hoserva/catalog on an installed system, the dev
// daemon's own state directory otherwise.
const catalogDirName = "catalog"

// catalogSourcesDirName holds one directory per user-added catalog source
// inside the state directory (doc 04 §4), next to the curated catalog's.
const catalogSourcesDirName = "catalog-sources"

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
	catalog := template.CatalogStore{Dir: filepath.Join(stateDir, catalogDirName)}
	if _, err := catalog.Seed(archive, sig); err != nil {
		return fmt.Errorf("installing the embedded catalog snapshot: %w", err)
	}
	return nil
}

// curatedCatalog is the curated catalog's on-disk copy, the one source
// registered and on by default (doc 04 §4). Its archive is only ever
// installed after its signature verified against the compiled-in key, so its
// entries carry the curated, signed badge.
func curatedCatalog(stateDir string) template.Catalog {
	return template.DirCatalog{
		Root:   filepath.Join(stateDir, catalogDirName),
		Source: template.SourceCurated,
		Kind:   store.CatalogSourceCurated,
		Signed: true,
	}
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
// on-disk copies and need no Docker service, /catalog/refresh
// (Handler.CatalogRefresh) runs the check that replaces the curated copy,
// and /catalog-sources and /stacks/{name}/template-update
// (Handler.CatalogSources) add, refresh, list and remove the user's own
// sources. With sourceStore the catalog is the curated one plus those
// sources, and the curated source has its row; with none, only the curated
// catalog is served and the source operations stay 501.
func wireCatalog(handler *api.Handler, stateDir string, notifier catalogPublisher, sourceStore template.SourceStore) {
	curated := curatedCatalog(stateDir)
	refresher := newCatalogRefresher(stateDir, notifier)
	handler.Catalog = curated
	handler.CatalogRefresh = refresher
	if sourceStore == nil {
		return
	}
	sources := &template.Sources{
		Store:          sourceStore,
		Dir:            filepath.Join(stateDir, catalogSourcesDirName),
		Curated:        curated,
		CuratedRefresh: refresher,
	}
	if err := sources.EnsureCurated(context.Background()); err != nil {
		log.Printf("hoservad: recording the curated catalog source: %v — the sources list will not show it until the next start", err)
	}
	handler.Catalog = sources
	handler.CatalogSources = sources
}

// wireCatalogChecks is what makes the automatic catalog checks and their
// settings reachable: every finished check, whatever started it, is published
// on Handler.CatalogChecks for /api/v1/events; /settings/catalog
// (Handler.CatalogSettings) reads and writes the settings; and listing the
// catalog starts the check-on-open check (Handler.CatalogOpen). It returns the
// loop main.go runs for the background interval. With no settings store there
// is no automatic check at all, and nil is returned.
func wireCatalogChecks(handler *api.Handler, settings api.CatalogSettingsStore) *template.AutoRefresher {
	hub := template.NewCheckHub()
	handler.CatalogChecks = hub
	refresher, ok := handler.CatalogRefresh.(*template.Refresher)
	if !ok {
		return nil
	}
	refresher.Finished = func(r template.CheckResult) {
		hub.Publish(r)
		if sources, ok := handler.CatalogSources.(*template.Sources); ok {
			sources.CuratedChecked(r)
		}
	}
	if settings == nil {
		return nil
	}
	handler.CatalogSettings = settings
	auto := &template.AutoRefresher{Refresher: refresher, Settings: settings}
	handler.CatalogOpen = auto
	return auto
}

// startTemplates is what main.go calls: it seeds the on-disk catalog from
// the embedded snapshot, then wires the catalog operations (with the catalog
// check, which publishes through notifier, and the automatic checks that
// follow settings, and the user-added sources in sourceStore) and template
// install over it. A
// seed that fails (a build with no snapshot, a snapshot that does not verify)
// is logged and does not stop the daemon: whatever catalog an earlier start or
// refresh left stays in use, and with none the catalog list is
// catalog_unavailable and every template is template_not_found.
func startTemplates(handler *api.Handler, stateDir string, apps *appServices, shares func(ctx context.Context) ([]string, error), notifier catalogPublisher, settings api.CatalogSettingsStore, sourceStore template.SourceStore) *template.AutoRefresher {
	if err := seedCatalog(stateDir); err != nil {
		log.Printf("hoservad: %v — template installs use only the catalog already in %s", err, filepath.Join(stateDir, catalogDirName))
	}
	wireCatalog(handler, stateDir, notifier, sourceStore)
	wireTemplateInstall(handler, stateDir, apps, shares)
	return wireCatalogChecks(handler, settings)
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
	catalog := handler.Catalog
	if catalog == nil {
		catalog = curatedCatalog(stateDir)
	}
	handler.TemplateInstall = &template.Installer{
		Catalog:  catalog,
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
