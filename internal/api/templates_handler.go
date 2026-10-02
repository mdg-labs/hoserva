package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/template"
)

func errTemplatesNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "Template install is not configured on this daemon"}
}

// mapTemplateError reports the template-side errors as such and leaves
// everything else, the stack and Docker errors an install or a change of an
// installed stack's inputs can run into, to mapStackError. verb names what
// was attempted, for the message of a failure it cannot classify.
func mapTemplateError(name string, err error, verb string) error {
	switch {
	case errors.Is(err, template.ErrTemplateNotFound):
		return &apiError{code: "template_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrInvalidTemplate):
		return &apiError{code: "template_invalid", statusCode: 422, message: err.Error()}
	case errors.Is(err, template.ErrInvalidInput):
		return &apiError{code: "invalid_template_input", statusCode: 400, message: err.Error()}
	case errors.Is(err, template.ErrGPUUnavailable):
		return &apiError{code: "gpu_unavailable", statusCode: 409, message: err.Error()}
	case errors.Is(err, template.ErrNoFreePort), errors.Is(err, template.ErrPortTaken):
		return &apiError{code: "no_free_port", statusCode: 409, message: err.Error()}
	case errors.Is(err, template.ErrStackHasNoTemplate):
		return &apiError{code: "stack_has_no_template", statusCode: 409, message: fmt.Sprintf("stack %q was not installed from a template and has no inputs to change; edit its Compose file instead", name)}
	}
	return mapStackError(name, err, verb)
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
		return nil, mapTemplateError(req.Name.Or(params.ID), err, "installing")
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
		return nil, mapTemplateError(req.Name.Or(params.ID), err, "installing")
	}
	return &apiv1.TemplateInstallResult{Stack: stackToAPI(st), Plan: planToAPI(plan)}, nil
}

func (h *Handler) GetStackConfig(ctx context.Context, params apiv1.GetStackConfigParams) (*apiv1.StackConfig, error) {
	if h.TemplateInstall == nil {
		return nil, errTemplatesNotConfigured()
	}
	cfg, err := h.TemplateInstall.Config(ctx, params.Name)
	if err != nil {
		return nil, mapTemplateError(params.Name, err, "reading")
	}
	out := stackConfigToAPI(cfg)
	return &out, nil
}

func (h *Handler) UpdateStackConfig(ctx context.Context, req *apiv1.UpdateStackConfigRequest, params apiv1.UpdateStackConfigParams) (*apiv1.StackConfig, error) {
	if h.TemplateInstall == nil {
		return nil, errTemplatesNotConfigured()
	}
	cfg, err := h.TemplateInstall.UpdateConfig(ctx, params.Name, template.ConfigUpdate{
		Values:   req.Values.Or(nil),
		Generate: req.Generate,
	})
	if err != nil {
		return nil, mapTemplateError(params.Name, err, "configuring")
	}
	out := stackConfigToAPI(cfg)
	return &out, nil
}

func stackConfigToAPI(c *template.StackConfig) apiv1.StackConfig {
	out := apiv1.StackConfig{Stack: stackToAPI(c.Stack), Inputs: make([]apiv1.StackConfigInput, len(c.Inputs))}
	for i, in := range c.Inputs {
		ci := apiv1.StackConfigInput{
			Name:        in.Name,
			Kind:        apiv1.StackConfigInputKind(in.Kind),
			ReadOnly:    in.ReadOnly,
			Suggestions: in.Suggestions,
		}
		if in.Role != "" {
			ci.Role = apiv1.NewOptStackConfigInputRole(apiv1.StackConfigInputRole(in.Role))
		}
		if in.Label != "" {
			ci.Label = apiv1.NewOptString(in.Label)
		}
		if in.Description != "" {
			ci.Description = apiv1.NewOptString(in.Description)
		}
		if in.Kind == template.KindSecret {
			ci.Set = apiv1.NewOptBool(in.Set)
		} else {
			ci.Value = apiv1.NewOptString(in.Value)
		}
		out.Inputs[i] = ci
	}
	return out
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
		out.Privileges[i] = privilegeToAPI(pr)
	}
	return out
}

func privilegeToAPI(pr template.Privilege) apiv1.TemplatePrivilege {
	tp := apiv1.TemplatePrivilege{Kind: apiv1.TemplatePrivilegeKind(pr.Kind), Service: pr.Service, Description: pr.Description}
	if pr.Detail != "" {
		tp.Detail = apiv1.NewOptString(pr.Detail)
	}
	return tp
}

// ConvertUnraidTemplate converts an Unraid XML template to a Compose file for
// review (doc 04 §5). It needs nothing from the daemon: the conversion is a
// pure function of the template text, and nothing is created or run.
func (h *Handler) ConvertUnraidTemplate(_ context.Context, req *apiv1.UnraidConvertRequest) (*apiv1.UnraidConversion, error) {
	conv, err := template.ConvertUnraid([]byte(req.XML), template.ConvertOptions{})
	if err != nil {
		if errors.Is(err, template.ErrInvalidUnraidTemplate) {
			return nil, &apiError{code: "invalid_unraid_template", statusCode: 400, message: err.Error()}
		}
		return nil, err
	}
	out := conversionToAPI(conv)
	return &out, nil
}

func conversionToAPI(c *template.Conversion) apiv1.UnraidConversion {
	out := apiv1.UnraidConversion{
		Source:     c.Source,
		Compose:    c.Compose,
		Clean:      c.Clean(),
		Warnings:   make([]apiv1.ConversionWarning, len(c.Warnings)),
		Privileges: make([]apiv1.TemplatePrivilege, len(c.Privileges)),
		Metadata: apiv1.UnraidTemplateMetadata{
			Title:      c.Metadata.Title,
			Overview:   optString(c.Metadata.Overview),
			Category:   optString(c.Metadata.Category),
			Support:    optString(c.Metadata.Support),
			Project:    optString(c.Metadata.Project),
			Webui:      optString(c.Metadata.WebUI),
			Icon:       optString(c.Metadata.Icon),
			Requires:   optString(c.Metadata.Requires),
			DonateLink: optString(c.Metadata.DonateLink),
			Variables:  make([]apiv1.UnraidVariable, len(c.Metadata.Variables)),
		},
	}
	for i, w := range c.Warnings {
		out.Warnings[i] = apiv1.ConversionWarning{
			Class:   apiv1.ConversionWarningClass(w.Class),
			Message: w.Message,
			Detail:  optString(w.Detail),
			Command: optString(w.Command),
		}
	}
	for i, p := range c.Privileges {
		out.Privileges[i] = privilegeToAPI(p)
	}
	for i, v := range c.Metadata.Variables {
		out.Metadata.Variables[i] = apiv1.UnraidVariable{Name: v.Name, Value: v.Value, Description: optString(v.Description), Secret: v.Secret}
	}
	return out
}

func optString(s string) apiv1.OptString {
	if s == "" {
		return apiv1.OptString{}
	}
	return apiv1.NewOptString(s)
}
