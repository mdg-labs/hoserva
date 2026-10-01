package api

import (
	"context"
	"errors"
	"strconv"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/template"
)

func errTemplatesNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "Template install is not configured on this daemon"}
}

// mapTemplateError reports the template-side errors as such and leaves
// everything else, the stack and Docker errors an install can run into, to
// mapStackError.
func mapTemplateError(name string, err error) error {
	switch {
	case errors.Is(err, template.ErrTemplateNotFound):
		return &apiError{code: "template_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrInvalidTemplate):
		return &apiError{code: "template_invalid", statusCode: 422, message: err.Error()}
	case errors.Is(err, template.ErrInvalidInput):
		return &apiError{code: "invalid_template_input", statusCode: 400, message: err.Error()}
	case errors.Is(err, template.ErrGPUUnavailable):
		return &apiError{code: "gpu_unavailable", statusCode: 409, message: err.Error()}
	case errors.Is(err, template.ErrNoFreePort):
		return &apiError{code: "no_free_port", statusCode: 409, message: err.Error()}
	}
	return mapStackError(name, err, "installing")
}

func planRequest(id string, req *apiv1.TemplateInstallRequest) template.PlanRequest {
	return template.PlanRequest{ID: id, Name: req.Name.Or(""), Values: req.Values.Or(nil)}
}

func (h *Handler) PreviewTemplateInstall(ctx context.Context, req *apiv1.TemplateInstallRequest, params apiv1.PreviewTemplateInstallParams) (*apiv1.TemplateInstallPlan, error) {
	if h.TemplateInstall == nil {
		return nil, errTemplatesNotConfigured()
	}
	plan, err := h.TemplateInstall.Preview(ctx, planRequest(params.ID, req))
	if err != nil {
		return nil, mapTemplateError(req.Name.Or(params.ID), err)
	}
	out := planToAPI(plan)
	return &out, nil
}

func (h *Handler) InstallTemplate(ctx context.Context, req *apiv1.TemplateInstallRequest, params apiv1.InstallTemplateParams) (*apiv1.TemplateInstallResult, error) {
	if h.TemplateInstall == nil {
		return nil, errTemplatesNotConfigured()
	}
	plan, st, err := h.TemplateInstall.Install(ctx, planRequest(params.ID, req))
	if err != nil {
		return nil, mapTemplateError(req.Name.Or(params.ID), err)
	}
	return &apiv1.TemplateInstallResult{Stack: stackToAPI(st), Plan: planToAPI(plan)}, nil
}

func planToAPI(p *template.Plan) apiv1.TemplateInstallPlan {
	out := apiv1.TemplateInstallPlan{
		Template:   apiv1.StackTemplate{Source: p.Source, ID: p.ID, Revision: strconv.Itoa(p.Revision)},
		Title:      p.Title,
		Name:       p.Name,
		Inputs:     make([]apiv1.TemplateInput, len(p.Inputs)),
		Privileges: make([]apiv1.TemplatePrivilege, len(p.Privileges)),
		Compose:    p.Compose,
	}
	for i, in := range p.Inputs {
		ti := apiv1.TemplateInput{
			Name:        in.Name,
			Kind:        apiv1.TemplateInputKind(in.Kind),
			Generated:   in.Generated,
			Suggestions: in.Suggestions,
		}
		if in.Role != "" {
			ti.Role = apiv1.NewOptTemplateInputRole(apiv1.TemplateInputRole(in.Role))
		}
		if in.Label != "" {
			ti.Label = apiv1.NewOptString(in.Label)
		}
		if in.Description != "" {
			ti.Description = apiv1.NewOptString(in.Description)
		}
		if in.Kind != template.KindSecret {
			ti.Value = apiv1.NewOptString(in.Value)
		}
		if in.Requested != "" {
			ti.RequestedValue = apiv1.NewOptString(in.Requested)
		}
		out.Inputs[i] = ti
	}
	for i, pr := range p.Privileges {
		tp := apiv1.TemplatePrivilege{Kind: apiv1.TemplatePrivilegeKind(pr.Kind), Service: pr.Service, Description: pr.Description}
		if pr.Detail != "" {
			tp.Detail = apiv1.NewOptString(pr.Detail)
		}
		out.Privileges[i] = tp
	}
	return out
}
