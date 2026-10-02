package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/template"
)

func errStackNotFound(name string) error {
	return &mockError{code: "stack_not_found", statusCode: 404, message: fmt.Sprintf("no stack %q", name)}
}

func errInvalidStackName(name string) error {
	return &mockError{code: "invalid_stack_name", statusCode: 400, message: fmt.Sprintf("%s: %q", container.ErrInvalidStackName, name)}
}

func errReservedEnv(names []string) error {
	return &mockError{code: "invalid_stack_env", statusCode: 400, message: fmt.Sprintf("%s: %s; Docker takes these from the daemon's environment", container.ErrReservedEnvName, strings.Join(names, ", "))}
}

func (h *handler) ListStacks(ctx context.Context) (*apiv1.ListStacksOK, error) {
	h.stacksMu.Lock()
	defer h.stacksMu.Unlock()
	out := &apiv1.ListStacksOK{Stacks: make([]apiv1.Stack, 0, len(h.stacks))}
	for _, s := range h.stacks {
		s.Compose = apiv1.OptString{}
		out.Stacks = append(out.Stacks, s)
	}
	sort.Slice(out.Stacks, func(i, j int) bool { return out.Stacks[i].Name < out.Stacks[j].Name })
	return out, nil
}

func (h *handler) GetStack(ctx context.Context, params apiv1.GetStackParams) (*apiv1.Stack, error) {
	if !container.ValidStackName(params.Name) {
		return nil, errInvalidStackName(params.Name)
	}
	h.stacksMu.Lock()
	defer h.stacksMu.Unlock()
	s, ok := h.stacks[params.Name]
	if !ok {
		return nil, errStackNotFound(params.Name)
	}
	return &s, nil
}

func (h *handler) CreateStack(ctx context.Context, req *apiv1.CreateStackRequest) (*apiv1.Stack, error) {
	if !container.ValidStackName(req.Name) {
		return nil, errInvalidStackName(req.Name)
	}
	if strings.TrimSpace(req.Compose) == "" {
		return nil, &mockError{code: "invalid_stack", statusCode: 400, message: fmt.Sprintf("%s: the compose file is empty", container.ErrInvalidStack)}
	}
	if names := container.ReservedEnvDefined(req.Env.Or("")); len(names) > 0 {
		return nil, errReservedEnv(names)
	}
	h.stacksMu.Lock()
	defer h.stacksMu.Unlock()
	if _, ok := h.stacks[req.Name]; ok {
		return nil, &mockError{code: "stack_exists", statusCode: 409, message: fmt.Sprintf("a stack named %q already exists", req.Name)}
	}
	s := apiv1.Stack{Name: req.Name, InstalledAt: time.Now().UTC().Truncate(time.Second)}
	if t, ok := req.Template.Get(); ok {
		s.Template = t
	}
	if h.stacks == nil {
		h.stacks = map[string]apiv1.Stack{}
	}
	h.setStackPorts(req.Name, composePorts(req.Compose, req.Env.Or("")))
	h.setStackEnv(req.Name, req.Env.Or(""))
	stored := s
	stored.Compose = apiv1.NewOptString(req.Compose)
	h.stacks[req.Name] = stored
	return &s, nil
}

// UpdateStack mirrors production: the name, then an empty file, are refused
// before the stack is looked up, and the text is checked before anything is
// stored (validateMockCompose stands in for `docker compose config`). A dry
// run changes nothing; otherwise the text is stored and the stack is marked
// manually edited. Its ports are worked out again from the new text.
func (h *handler) UpdateStack(ctx context.Context, req *apiv1.UpdateStackRequest, params apiv1.UpdateStackParams) (*apiv1.UpdateStackResult, error) {
	if !container.ValidStackName(params.Name) {
		return nil, errInvalidStackName(params.Name)
	}
	if strings.TrimSpace(req.Compose) == "" {
		return nil, errInvalidStack("the compose file is empty")
	}
	h.stacksMu.Lock()
	defer h.stacksMu.Unlock()
	s, ok := h.stacks[params.Name]
	if !ok {
		return nil, errStackNotFound(params.Name)
	}
	if err := validateMockCompose(req.Compose); err != nil {
		return nil, errInvalidStack(err.Error())
	}
	if params.DryRun.Or(false) {
		return &apiv1.UpdateStackResult{Applied: false, Stack: s}, nil
	}
	s.Compose = apiv1.NewOptString(req.Compose)
	s.ManuallyEdited = true
	h.stacks[params.Name] = s
	h.setStackPorts(params.Name, composePorts(req.Compose, h.stackEnvs[params.Name]))
	return &apiv1.UpdateStackResult{Applied: true, Stack: s}, nil
}

// GetStackConfig and UpdateStackConfig run the production installer over the
// mock's stacks, so a value is refused exactly as the daemon refuses it.
func (h *handler) GetStackConfig(ctx context.Context, params apiv1.GetStackConfigParams) (*apiv1.StackConfig, error) {
	cfg, err := h.templateInstaller().Config(ctx, params.Name)
	if err != nil {
		return nil, mapMockTemplateError(params.Name, err)
	}
	out := mockStackConfigToAPI(cfg)
	return &out, nil
}

func (h *handler) UpdateStackConfig(ctx context.Context, req *apiv1.UpdateStackConfigRequest, params apiv1.UpdateStackConfigParams) (*apiv1.StackConfig, error) {
	cfg, err := h.templateInstaller().UpdateConfig(ctx, params.Name, template.ConfigUpdate{
		Values:   req.Values.Or(nil),
		Generate: req.Generate,
	})
	if err != nil {
		return nil, mapMockTemplateError(params.Name, err)
	}
	out := mockStackConfigToAPI(cfg)
	return &out, nil
}

func mockStackConfigToAPI(c *template.StackConfig) apiv1.StackConfig {
	out := apiv1.StackConfig{
		Stack: apiv1.Stack{
			Name:           c.Stack.Name,
			Template:       apiv1.StackTemplate{Source: c.Stack.TemplateSource, ID: c.Stack.TemplateID, Revision: c.Stack.TemplateRevision},
			InstalledAt:    c.Stack.InstalledAt,
			ManuallyEdited: c.Stack.ManuallyEdited,
		},
		Inputs: make([]apiv1.StackConfigInput, len(c.Inputs)),
	}
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

// StartStack mirrors production: the stack is looked up, then the array
// must be running, before the stack_start job is recorded.
func (h *handler) StartStack(ctx context.Context, params apiv1.StartStackParams) (*apiv1.Job, error) {
	if !container.ValidStackName(params.Name) {
		return nil, errInvalidStackName(params.Name)
	}
	h.stacksMu.Lock()
	_, ok := h.stacks[params.Name]
	h.stacksMu.Unlock()
	if !ok {
		return nil, errStackNotFound(params.Name)
	}
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	return h.queueServiceJob(apiv1.JobTypeStackStart)
}

func errInvalidStack(reason string) error {
	return &mockError{code: "invalid_stack", statusCode: 400, message: fmt.Sprintf("%s: %s", container.ErrInvalidStack, reason)}
}

// validateMockCompose is the mock's `docker compose config`: the text must
// be YAML whose top level is a mapping, and whose services, when given, are
// a mapping too.
func validateMockCompose(text string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("the compose file must be a mapping")
	}
	top := doc.Content[0].Content
	for i := 0; i+1 < len(top); i += 2 {
		if top[i].Value == "services" && top[i+1].Kind != yaml.MappingNode {
			return fmt.Errorf("services must be a mapping")
		}
	}
	return nil
}

// RemoveStack mirrors production: asking for appdata deletion is refused with
// array_stopped, before the stack is looked up, while the array is not
// running, and with appdata_unavailable, as RemoveApp is, where the mock has
// no cache disk; a plain remove needs neither. The name is free again
// afterwards, and so are its containers: production brings them down with
// the stack, so they leave the list.
func (h *handler) RemoveStack(ctx context.Context, params apiv1.RemoveStackParams) (*apiv1.RemoveStackResult, error) {
	if !container.ValidStackName(params.Name) {
		return nil, errInvalidStackName(params.Name)
	}
	deleteAppdata := params.DeleteAppdata.Or(false)
	if deleteAppdata {
		if err := h.requireArrayRunning(); err != nil {
			return nil, err
		}
	}
	h.stacksMu.Lock()
	defer h.stacksMu.Unlock()
	if _, ok := h.stacks[params.Name]; !ok {
		return nil, errStackNotFound(params.Name)
	}
	if deleteAppdata && container.CacheAppdataRoots(mockArrayDisks(h.scenario)) == nil {
		return nil, &mockError{code: "appdata_unavailable", statusCode: 409, message: "no appdata location is configured, so appdata cannot be deleted"}
	}
	delete(h.stacks, params.Name)
	delete(h.stackPorts, params.Name)
	delete(h.stackEnvs, params.Name)
	h.appsMu.Lock()
	h.apps = slices.DeleteFunc(h.apps, func(a apiv1.App) bool { return a.Stack.Or("") == params.Name })
	h.appsMu.Unlock()
	deleted := []string{}
	if deleteAppdata {
		deleted = append(deleted, "/var/lib/hoserva/stacks/"+params.Name)
	}
	return &apiv1.RemoveStackResult{DeletedPaths: deleted}, nil
}

// setStackPorts records the ports a stack publishes; stacksMu is held.
func (h *handler) setStackPorts(name string, ports map[int]bool) {
	if h.stackPorts == nil {
		h.stackPorts = map[string]map[int]bool{}
	}
	h.stackPorts[name] = ports
}

// setStackEnv records the .env a stack was created with; stacksMu is held.
func (h *handler) setStackEnv(name, env string) {
	if h.stackEnvs == nil {
		h.stackEnvs = map[string]string{}
	}
	h.stackEnvs[name] = env
}
