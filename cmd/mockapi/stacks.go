package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
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
	h.stacks[req.Name] = s
	return &s, nil
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
