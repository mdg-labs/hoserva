package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"
)

// CatalogSourceService is the catalog sources (doc 04 §4): *template.Sources
// is the production implementation.
type CatalogSourceService interface {
	List(ctx context.Context) ([]template.SourceStatus, error)
	Add(ctx context.Context, req template.AddRequest) (template.SourceStatus, error)
	Refresh(ctx context.Context, id string) (template.CheckResult, error)
	Remove(ctx context.Context, id string) error
	From(ctx context.Context, source string) (template.Catalog, error)
}

// sourceBadge is a source's badge as the API carries it. A catalog that
// reports no kind says nothing about its source's trust, which is shown as
// the least trusted badge: user-added and unsigned.
func sourceBadge(kind store.CatalogSourceKind, signed bool) (apiv1.CatalogSourceKind, bool) {
	switch kind {
	case store.CatalogSourceCurated:
		return apiv1.CatalogSourceKindCurated, signed
	case store.CatalogSourceUserAdded:
		return apiv1.CatalogSourceKindUserAdded, signed
	}
	return apiv1.CatalogSourceKindUserAdded, false
}

func mapSourceError(err error) error {
	switch {
	case errors.Is(err, template.ErrInvalidSource):
		return &apiError{code: "invalid_catalog_source", statusCode: 400, message: err.Error()}
	case errors.Is(err, template.ErrSourceExists):
		return &apiError{code: "catalog_source_exists", statusCode: 409, message: err.Error()}
	case errors.Is(err, template.ErrSourceNotFound):
		return &apiError{code: "catalog_source_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrSourceCurated):
		return &apiError{code: "catalog_source_curated", statusCode: 409, message: err.Error()}
	case errors.Is(err, template.ErrSourceUnreachable):
		return &apiError{code: "catalog_source_unreachable", statusCode: 502, message: err.Error()}
	case errors.Is(err, template.ErrSourceRejected):
		return &apiError{code: "catalog_source_rejected", statusCode: 422, message: err.Error()}
	}
	return err
}

func sourceToAPI(s template.SourceStatus) apiv1.CatalogSource {
	kind, _ := sourceBadge(s.Kind, s.Signed)
	out := apiv1.CatalogSource{ID: s.ID, URL: s.URL, Kind: kind, Signed: s.Signed}
	if s.Serial > 0 {
		out.Serial = apiv1.NewOptInt64(s.Serial)
	}
	if !s.LastRefreshedAt.IsZero() {
		out.LastRefreshedAt = apiv1.NewOptDateTime(s.LastRefreshedAt)
	}
	return out
}

func (h *Handler) ListCatalogSources(ctx context.Context) (*apiv1.CatalogSourceList, error) {
	if h.CatalogSources == nil {
		return nil, errCatalogNotConfigured()
	}
	list, err := h.CatalogSources.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the catalog sources: %w", err)
	}
	out := &apiv1.CatalogSourceList{Sources: make([]apiv1.CatalogSource, len(list))}
	for i, s := range list {
		out.Sources[i] = sourceToAPI(s)
	}
	return out, nil
}

func (h *Handler) AddCatalogSource(ctx context.Context, req *apiv1.AddCatalogSourceRequest) (*apiv1.CatalogSource, error) {
	if h.CatalogSources == nil {
		return nil, errCatalogNotConfigured()
	}
	// A key that is present but blank is refused: it must never turn a
	// source the user meant to verify (an empty variable in a script) into
	// an unsigned one.
	if key, set := req.PublicKey.Get(); set && strings.TrimSpace(key) == "" {
		return nil, mapSourceError(fmt.Errorf("%w: the public key is empty; leave it out to add an unsigned source", template.ErrInvalidSource))
	}
	s, err := h.CatalogSources.Add(ctx, template.AddRequest{URL: req.URL, PublicKey: req.PublicKey.Or("")})
	if err != nil {
		return nil, mapSourceError(err)
	}
	out := sourceToAPI(s)
	return &out, nil
}

func (h *Handler) RemoveCatalogSource(ctx context.Context, params apiv1.RemoveCatalogSourceParams) error {
	if h.CatalogSources == nil {
		return errCatalogNotConfigured()
	}
	if err := h.CatalogSources.Remove(ctx, params.ID); err != nil {
		return mapSourceError(err)
	}
	return nil
}

func (h *Handler) RefreshCatalogSource(ctx context.Context, params apiv1.RefreshCatalogSourceParams) (*apiv1.CatalogRefresh, error) {
	if h.CatalogSources == nil {
		return nil, errCatalogNotConfigured()
	}
	res, err := h.CatalogSources.Refresh(ctx, params.ID)
	if err != nil {
		return nil, mapSourceError(err)
	}
	return checkResultToAPI(res), nil
}

func (h *Handler) GetStackTemplateUpdate(ctx context.Context, params apiv1.GetStackTemplateUpdateParams) (*apiv1.StackTemplateUpdate, error) {
	if h.Stacks == nil {
		return nil, errStacksNotConfigured()
	}
	if h.CatalogSources == nil {
		return nil, errCatalogNotConfigured()
	}
	st, err := h.Stacks.Get(ctx, params.Name)
	if err != nil {
		return nil, mapStackError(params.Name, err, "reading")
	}
	u, err := template.CheckUpdate(ctx, h.CatalogSources, template.InstalledStack{
		Name:           st.Name,
		Source:         st.TemplateSource,
		TemplateID:     st.TemplateID,
		Revision:       st.TemplateRevision,
		Compose:        st.Compose,
		ManuallyEdited: st.ManuallyEdited,
	})
	if err != nil {
		return nil, mapCatalogError(err)
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
		kind, signed := sourceBadge(u.Kind, u.Signed)
		out.AvailableRevision = apiv1.NewOptInt(u.AvailableRevision)
		out.SourceKind = apiv1.NewOptCatalogSourceKind(kind)
		out.Signed = apiv1.NewOptBool(signed)
	}
	if u.Status == template.UpdateAvailable {
		out.Diff = apiv1.NewOptString(u.Diff)
	}
	return out, nil
}
