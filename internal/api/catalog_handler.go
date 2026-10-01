package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/template"
)

const (
	// iconCSP lets an SVG icon draw itself and nothing else: no script, no
	// subresource, no framing, so opening the file directly runs no code.
	iconCSP           = "default-src 'none'; style-src 'unsafe-inline'; sandbox"
	iconContentOption = "nosniff"
)

func errCatalogNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "The template catalog is not configured on this daemon"}
}

func mapCatalogError(err error) error {
	switch {
	case errors.Is(err, template.ErrCatalogUnavailable):
		return &apiError{code: "catalog_unavailable", statusCode: 503, message: err.Error()}
	case errors.Is(err, template.ErrIconNotFound):
		return &apiError{code: "template_icon_not_found", statusCode: 404, message: err.Error()}
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
		out.Templates[i] = apiv1.CatalogEntry{
			ID:         t.ID,
			Revision:   t.Revision,
			Title:      t.Title,
			Categories: t.Categories,
			Docs:       t.Docs,
			Source:     h.Catalog.Name(),
			Installed:  installed[t.ID],
		}
	}
	return out, nil
}

func (h *Handler) GetCatalogTemplate(ctx context.Context, params apiv1.GetCatalogTemplateParams) (*apiv1.CatalogTemplate, error) {
	if h.Catalog == nil {
		return nil, errCatalogNotConfigured()
	}
	d, err := template.Show(ctx, h.Catalog, params.ID)
	if err != nil {
		return nil, mapCatalogError(err)
	}
	out := &apiv1.CatalogTemplate{
		ID:         d.ID,
		Revision:   d.Revision,
		Title:      d.Title,
		Categories: d.Categories,
		Docs:       d.Docs,
		Source:     d.Source,
		Compose:    d.Compose,
		Privileges: make([]apiv1.TemplatePrivilege, len(d.Privileges)),
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
