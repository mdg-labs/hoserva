package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"
)

const (
	// iconCSP lets an SVG icon draw itself and nothing else: no script, no
	// subresource, no framing, so opening the file directly runs no code.
	iconCSP           = "default-src 'none'; style-src 'unsafe-inline'; sandbox"
	iconContentOption = "nosniff"
	// screenshotCSP allows nothing at all: a screenshot is a raster image.
	screenshotCSP = "default-src 'none'; sandbox"
)

// CatalogRefresher runs one catalog check and reports the latest one;
// *template.Refresher is the production implementation.
type CatalogRefresher interface {
	Refresh(ctx context.Context) (template.CheckResult, error)
	Last() (template.CheckResult, bool)
}

// CatalogSettingsStore reads and writes the catalog refresh settings;
// *store.CatalogSettingsStore is the production implementation.
type CatalogSettingsStore interface {
	CatalogSettings(ctx context.Context) (store.CatalogSettings, error)
	UpdateCatalogSettings(ctx context.Context, u store.CatalogSettingsUpdate) (store.CatalogSettings, error)
}

// CatalogOpener is the check-on-open trigger; *template.AutoRefresher is the
// production implementation.
type CatalogOpener interface {
	CheckOnOpen(ctx context.Context)
}

func errCatalogNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "The template catalog is not configured on this daemon"}
}

func mapCatalogError(err error) error {
	switch {
	case errors.Is(err, template.ErrCatalogUnavailable):
		return &apiError{code: "catalog_unavailable", statusCode: 503, message: err.Error()}
	case errors.Is(err, template.ErrIconNotFound):
		return &apiError{code: "template_icon_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrScreenshotNotFound):
		return &apiError{code: "template_screenshot_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrTemplateNotFound):
		return &apiError{code: "template_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrInvalidTemplate):
		return &apiError{code: "template_invalid", statusCode: 422, message: err.Error()}
	}
	return err
}

func (h *Handler) ListCatalog(ctx context.Context) (*apiv1.CatalogList, error) {
	if h.Catalog == nil {
		return nil, errCatalogNotConfigured()
	}
	if h.Stacks == nil {
		return nil, errStacksNotConfigured()
	}
	if h.CatalogOpen != nil {
		h.CatalogOpen.CheckOnOpen(ctx)
	}
	index, err := h.Catalog.Index(ctx)
	if err != nil {
		return nil, mapCatalogError(err)
	}
	stacks, err := h.Stacks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing stacks: %w", err)
	}
	installed := make(map[string]bool, len(stacks))
	for _, s := range stacks {
		if s.TemplateID != "" {
			installed[s.TemplateID] = true
		}
	}
	out := &apiv1.CatalogList{Serial: index.Serial, Templates: make([]apiv1.CatalogEntry, len(index.Templates))}
	if !index.GeneratedAt.IsZero() {
		out.GeneratedAt = apiv1.NewOptDateTime(index.GeneratedAt)
	}
	for i, t := range index.Templates {
		kind, signed := sourceBadge(t.Kind, t.Signed)
		out.Templates[i] = apiv1.CatalogEntry{
			ID:         t.ID,
			Revision:   t.Revision,
			Title:      t.Title,
			Categories: t.Categories,
			Docs:       t.Docs,
			Maintainer: optString(t.Maintainer),
			Source:     t.Source,
			SourceKind: kind,
			Signed:     signed,
			Installed:  installed[t.ID],
		}
	}
	if h.CatalogRefresh != nil {
		if last, ok := h.CatalogRefresh.Last(); ok {
			out.LastCheckedAt = apiv1.NewOptDateTime(last.CheckedAt)
			out.LastOutcome = apiv1.NewOptCatalogCheckOutcome(apiv1.CatalogCheckOutcome(last.Outcome))
		}
	}
	return out, nil
}

func (h *Handler) RefreshCatalog(ctx context.Context) (*apiv1.CatalogRefresh, error) {
	if h.CatalogRefresh == nil {
		return nil, errCatalogNotConfigured()
	}
	res, err := h.CatalogRefresh.Refresh(ctx)
	if err != nil {
		return nil, fmt.Errorf("waiting for the catalog check: %w", err)
	}
	return checkResultToAPI(res), nil
}

func checkResultToAPI(res template.CheckResult) *apiv1.CatalogRefresh {
	out := &apiv1.CatalogRefresh{CheckedAt: res.CheckedAt, Outcome: apiv1.CatalogCheckOutcome(res.Outcome)}
	switch res.Outcome {
	case template.OutcomeUpdated:
		out.NewTemplates = apiv1.NewOptInt(res.New)
		out.UpdatedTemplates = apiv1.NewOptInt(res.Updated)
	case template.OutcomeFailed:
		out.Reason = apiv1.NewOptCatalogRefreshReason(apiv1.CatalogRefreshReason(res.Reason))
		out.Message = apiv1.NewOptString(res.Message)
	}
	return out
}

func catalogSettingsToAPI(s store.CatalogSettings) *apiv1.CatalogSettings {
	return &apiv1.CatalogSettings{RefreshInterval: apiv1.CatalogRefreshInterval(s.RefreshInterval), CheckOnOpen: s.CheckOnOpen}
}

func (h *Handler) GetCatalogSettings(ctx context.Context) (*apiv1.CatalogSettings, error) {
	if h.CatalogSettings == nil {
		return nil, errCatalogNotConfigured()
	}
	s, err := h.CatalogSettings.CatalogSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the catalog settings: %w", err)
	}
	return catalogSettingsToAPI(s), nil
}

func (h *Handler) UpdateCatalogSettings(ctx context.Context, req *apiv1.CatalogSettingsUpdate) (*apiv1.CatalogSettings, error) {
	if h.CatalogSettings == nil {
		return nil, errCatalogNotConfigured()
	}
	var u store.CatalogSettingsUpdate
	if v, ok := req.RefreshInterval.Get(); ok {
		interval := string(v)
		u.RefreshInterval = &interval
	}
	if v, ok := req.CheckOnOpen.Get(); ok {
		u.CheckOnOpen = &v
	}
	s, err := h.CatalogSettings.UpdateCatalogSettings(ctx, u)
	if err != nil {
		if errors.Is(err, store.ErrCatalogInterval) {
			return nil, &apiError{code: "invalid_catalog_interval", statusCode: 400, message: err.Error()}
		}
		return nil, fmt.Errorf("saving the catalog settings: %w", err)
	}
	return catalogSettingsToAPI(s), nil
}

func (h *Handler) GetCatalogTemplate(ctx context.Context, params apiv1.GetCatalogTemplateParams) (*apiv1.CatalogTemplate, error) {
	if h.Catalog == nil {
		return nil, errCatalogNotConfigured()
	}
	d, err := template.Show(ctx, h.Catalog, params.ID)
	if err != nil {
		return nil, mapCatalogError(err)
	}
	kind, signed := sourceBadge(d.Kind, d.Signed)
	out := &apiv1.CatalogTemplate{
		ID:              d.ID,
		Revision:        d.Revision,
		Title:           d.Title,
		Categories:      d.Categories,
		Docs:            d.Docs,
		Maintainer:      optString(d.Maintainer),
		Description:     optString(d.Description),
		ScreenshotCount: d.Screenshots,
		Source:          d.Source,
		SourceKind:      kind,
		Signed:          signed,
		Compose:         d.Compose,
		Privileges:      make([]apiv1.TemplatePrivilege, len(d.Privileges)),
	}
	if d.Links != (template.Links{}) {
		out.Links = apiv1.NewOptCatalogTemplateLinks(apiv1.CatalogTemplateLinks{
			Project: optString(d.Links.Project),
			Support: optString(d.Links.Support),
			Donate:  optString(d.Links.Donate),
		})
	}
	for i, pr := range d.Privileges {
		out.Privileges[i] = privilegeToAPI(pr)
	}
	return out, nil
}

func (h *Handler) GetCatalogTemplateIcon(ctx context.Context, params apiv1.GetCatalogTemplateIconParams) (apiv1.GetCatalogTemplateIconRes, error) {
	if h.Catalog == nil {
		return nil, errCatalogNotConfigured()
	}
	icon, err := h.Catalog.Icon(ctx, params.ID)
	if err != nil {
		return nil, mapCatalogError(err)
	}
	body := bytes.NewReader(icon.Data)
	switch icon.ContentType {
	case "image/svg+xml":
		return &apiv1.GetCatalogTemplateIconOKImageSvgXMLHeaders{ContentSecurityPolicy: iconCSP, XContentTypeOptions: iconContentOption, Response: apiv1.GetCatalogTemplateIconOKImageSvgXML{Data: body}}, nil
	case "image/png":
		return &apiv1.GetCatalogTemplateIconOKImagePNGHeaders{ContentSecurityPolicy: iconCSP, XContentTypeOptions: iconContentOption, Response: apiv1.GetCatalogTemplateIconOKImagePNG{Data: body}}, nil
	case "image/webp":
		return &apiv1.GetCatalogTemplateIconOKImageWEBPHeaders{ContentSecurityPolicy: iconCSP, XContentTypeOptions: iconContentOption, Response: apiv1.GetCatalogTemplateIconOKImageWEBP{Data: body}}, nil
	case "image/jpeg":
		return &apiv1.GetCatalogTemplateIconOKImageJpegHeaders{ContentSecurityPolicy: iconCSP, XContentTypeOptions: iconContentOption, Response: apiv1.GetCatalogTemplateIconOKImageJpeg{Data: body}}, nil
	}
	return nil, fmt.Errorf("catalog icon of template %q has the content type %q, which the API does not serve", params.ID, icon.ContentType)
}

func (h *Handler) GetCatalogTemplateScreenshot(ctx context.Context, params apiv1.GetCatalogTemplateScreenshotParams) (apiv1.GetCatalogTemplateScreenshotRes, error) {
	if h.Catalog == nil {
		return nil, errCatalogNotConfigured()
	}
	shot, err := h.Catalog.Screenshot(ctx, params.ID, params.Index)
	if err != nil {
		return nil, mapCatalogError(err)
	}
	body := bytes.NewReader(shot.Data)
	switch shot.ContentType {
	case "image/png":
		return &apiv1.GetCatalogTemplateScreenshotOKImagePNGHeaders{ContentSecurityPolicy: screenshotCSP, XContentTypeOptions: iconContentOption, Response: apiv1.GetCatalogTemplateScreenshotOKImagePNG{Data: body}}, nil
	case "image/webp":
		return &apiv1.GetCatalogTemplateScreenshotOKImageWEBPHeaders{ContentSecurityPolicy: screenshotCSP, XContentTypeOptions: iconContentOption, Response: apiv1.GetCatalogTemplateScreenshotOKImageWEBP{Data: body}}, nil
	case "image/jpeg":
		return &apiv1.GetCatalogTemplateScreenshotOKImageJpegHeaders{ContentSecurityPolicy: screenshotCSP, XContentTypeOptions: iconContentOption, Response: apiv1.GetCatalogTemplateScreenshotOKImageJpeg{Data: body}}, nil
	}
	return nil, fmt.Errorf("catalog screenshot %d of template %q has the content type %q, which the API does not serve", params.Index, params.ID, shot.ContentType)
}
