package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"
)

const (
	// mockCuratedSourceURL is the curated source's address as the sources
	// list shows it.
	mockCuratedSourceURL = "https://catalog.hoserva.dev"
	// mockDownHost is a catalog host the mock cannot reach, so a page built
	// on the mock meets a failed add.
	mockDownHost = "down.example.org"
	// mockExtrasSourceID and mockExtrasSourceURL are the user-added source
	// the mock starts with: unsigned, so a page built on the mock can show
	// that badge on a real entry.
	mockExtrasSourceID  = "src-0000000000"
	mockExtrasSourceURL = "https://extras.example.org/catalog"
)

// mockSourceKey is the one key the mock's imaginary publishers sign with: a
// source added with this public key is verified and badged signed, one added
// with any other key is refused because its signature cannot verify, and one
// added with no key is unsigned. The seed is public on purpose, so the
// production handler in the contract rig can serve archives signed with it.
var mockSourceKey = ed25519.NewKeyFromSeed([]byte("hoserva-mock-source-signing-seed"))

// mockSourcePublicKey is mockSourceKey's public half, as a source is added
// with it.
func mockSourcePublicKey() string {
	return base64.StdEncoding.EncodeToString(mockSourceKey.Public().(ed25519.PublicKey))
}

// mockSeededSources is the source list a new mock starts with.
func mockSeededSources() []apiv1.CatalogSource {
	return []apiv1.CatalogSource{{
		ID:              mockExtrasSourceID,
		URL:             mockExtrasSourceURL,
		Kind:            apiv1.CatalogSourceKindUserAdded,
		Signed:          false,
		Serial:          apiv1.NewOptInt64(1),
		LastRefreshedAt: apiv1.NewOptDateTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)),
	}}
}

func mockSourceError(code string, status int, err error) error {
	return &mockError{code: code, statusCode: status, message: err.Error()}
}

func mockCuratedSource(h *handler) apiv1.CatalogSource {
	src := apiv1.CatalogSource{ID: template.SourceCurated, URL: mockCuratedSourceURL, Kind: apiv1.CatalogSourceKindCurated, Signed: true, Serial: apiv1.NewOptInt64(1)}
	h.catalogMu.Lock()
	defer h.catalogMu.Unlock()
	if last := h.catalogLast; last != nil && last.Outcome != apiv1.CatalogCheckOutcomeFailed {
		src.LastRefreshedAt = apiv1.NewOptDateTime(last.CheckedAt)
	}
	return src
}

func (h *handler) ListCatalogSources(context.Context) (*apiv1.CatalogSourceList, error) {
	out := &apiv1.CatalogSourceList{Sources: []apiv1.CatalogSource{mockCuratedSource(h)}}
	h.sourcesMu.Lock()
	defer h.sourcesMu.Unlock()
	out.Sources = append(out.Sources, h.catalogSources...)
	return out, nil
}

func (h *handler) AddCatalogSource(_ context.Context, req *apiv1.AddCatalogSourceRequest) (*apiv1.CatalogSource, error) {
	canonical, err := template.ParseSourceURL(req.URL)
	if err != nil {
		return nil, mockSourceError("invalid_catalog_source", 400, err)
	}
	if k, set := req.PublicKey.Get(); set && strings.TrimSpace(k) == "" {
		return nil, mockSourceError("invalid_catalog_source", 400, fmt.Errorf("%w: the public key is empty; leave it out to add an unsigned source", template.ErrInvalidSource))
	}
	key, err := template.ParseSourceKey(req.PublicKey.Or(""))
	if err != nil {
		return nil, mockSourceError("invalid_catalog_source", 400, err)
	}
	h.sourcesMu.Lock()
	defer h.sourcesMu.Unlock()
	taken := canonical == mockCuratedSourceURL
	for _, s := range h.catalogSources {
		taken = taken || s.URL == canonical
	}
	if taken {
		return nil, mockSourceError("catalog_source_exists", 409, fmt.Errorf("%w: %s", template.ErrSourceExists, canonical))
	}
	if u, err := url.Parse(canonical); err == nil && u.Hostname() == mockDownHost {
		return nil, mockSourceError("catalog_source_unreachable", 502, fmt.Errorf("fetching %s/catalog.tar.zst: the mock's scripted network failure", canonical))
	}
	if key != nil && !key.Equal(mockSourceKey.Public().(ed25519.PublicKey)) {
		return nil, mockSourceError("catalog_source_rejected", 422, fmt.Errorf("%w: the signature does not verify against the public key", template.ErrBadSignature))
	}
	h.nextSourceID++
	src := apiv1.CatalogSource{
		ID:              fmt.Sprintf("src-%010x", h.nextSourceID),
		URL:             canonical,
		Kind:            apiv1.CatalogSourceKindUserAdded,
		Signed:          key != nil,
		Serial:          apiv1.NewOptInt64(1),
		LastRefreshedAt: apiv1.NewOptDateTime(time.Now().UTC().Truncate(time.Second)),
	}
	h.catalogSources = append(h.catalogSources, src)
	return &src, nil
}

func (h *handler) RemoveCatalogSource(_ context.Context, params apiv1.RemoveCatalogSourceParams) error {
	if params.ID == template.SourceCurated {
		return mockSourceError("catalog_source_curated", 409, template.ErrSourceCurated)
	}
	h.sourcesMu.Lock()
	defer h.sourcesMu.Unlock()
	for i, s := range h.catalogSources {
		if s.ID == params.ID {
			h.catalogSources = append(h.catalogSources[:i], h.catalogSources[i+1:]...)
			return nil
		}
	}
	return mockSourceError("catalog_source_not_found", 404, fmt.Errorf("%w: %q", template.ErrSourceNotFound, params.ID))
}

func (h *handler) RefreshCatalogSource(ctx context.Context, params apiv1.RefreshCatalogSourceParams) (*apiv1.CatalogRefresh, error) {
	if params.ID == template.SourceCurated {
		return h.RefreshCatalog(ctx)
	}
	h.sourcesMu.Lock()
	defer h.sourcesMu.Unlock()
	for i, s := range h.catalogSources {
		if s.ID == params.ID {
			now := time.Now().UTC().Truncate(time.Second)
			h.catalogSources[i].LastRefreshedAt = apiv1.NewOptDateTime(now)
			return &apiv1.CatalogRefresh{CheckedAt: now, Outcome: apiv1.CatalogCheckOutcomeUnchanged}, nil
		}
	}
	return nil, mockSourceError("catalog_source_not_found", 404, fmt.Errorf("%w: %q", template.ErrSourceNotFound, params.ID))
}

// mockSourceCatalogs resolves the source a mock stack records: the mock's
// curated catalog, the seeded user-added source, or a source added through
// this mock, which supplies no templates of its own.
type mockSourceCatalogs struct{ h *handler }

func (m mockSourceCatalogs) From(_ context.Context, source string) (template.Catalog, error) {
	if source == template.SourceCurated {
		return mockCatalog(), nil
	}
	m.h.sourcesMu.Lock()
	defer m.h.sourcesMu.Unlock()
	for _, s := range m.h.catalogSources {
		if s.ID == source {
			if s.ID == mockExtrasSourceID {
				return mockExtrasCatalog(), nil
			}
			return template.MapCatalog{Source: s.ID, Kind: store.CatalogSourceUserAdded, Signed: s.Signed}, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", template.ErrSourceNotFound, source)
}

func (h *handler) GetStackTemplateUpdate(ctx context.Context, params apiv1.GetStackTemplateUpdateParams) (*apiv1.StackTemplateUpdate, error) {
	if !container.ValidStackName(params.Name) {
		return nil, errInvalidStackName(params.Name)
	}
	h.stacksMu.Lock()
	s, ok := h.stacks[params.Name]
	h.stacksMu.Unlock()
	if !ok {
		return nil, errStackNotFound(params.Name)
	}
	u, err := template.CheckUpdate(ctx, mockSourceCatalogs{h}, template.InstalledStack{
		Name:           s.Name,
		Source:         s.Template.Source,
		TemplateID:     s.Template.ID,
		Revision:       s.Template.Revision,
		Compose:        s.Compose.Or(""),
		ManuallyEdited: s.ManuallyEdited,
	})
	if err != nil {
		return nil, mapMockTemplateError(params.Name, err)
	}
	out := &apiv1.StackTemplateUpdate{Status: apiv1.StackTemplateUpdateStatus(u.Status), ManuallyEdited: u.ManuallyEdited}
	if u.Source != "" {
		out.Source = apiv1.NewOptString(u.Source)
	}
	if u.TemplateID != "" {
		out.TemplateId = apiv1.NewOptString(u.TemplateID)
	}
	if u.InstalledRevision > 0 {
		out.InstalledRevision = apiv1.NewOptInt(u.InstalledRevision)
	}
	if u.AvailableRevision > 0 {
		kind, signed := mockBadge(u.Kind, u.Signed)
		out.AvailableRevision = apiv1.NewOptInt(u.AvailableRevision)
		out.SourceKind = apiv1.NewOptCatalogSourceKind(kind)
		out.Signed = apiv1.NewOptBool(signed)
	}
	if u.Status == template.UpdateAvailable {
		out.Diff = apiv1.NewOptString(u.Diff)
	}
	return out, nil
}

// mockBadge is a source's badge as the API carries it: a kind a catalog does
// not report is shown as the least trusted badge, user-added and unsigned,
// as the production handler does.
func mockBadge(kind store.CatalogSourceKind, signed bool) (apiv1.CatalogSourceKind, bool) {
	switch kind {
	case store.CatalogSourceCurated:
		return apiv1.CatalogSourceKindCurated, signed
	case store.CatalogSourceUserAdded:
		return apiv1.CatalogSourceKindUserAdded, signed
	}
	return apiv1.CatalogSourceKindUserAdded, false
}
