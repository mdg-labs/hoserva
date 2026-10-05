package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
)

func errStacksNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "Compose stacks are not configured on this daemon"}
}

func mapStackError(name string, err error, verb string) error {
	switch {
	case errors.Is(err, container.ErrInvalidStackName):
		return &apiError{code: "invalid_stack_name", statusCode: 400, message: err.Error()}
	case errors.Is(err, container.ErrInvalidStack):
		return &apiError{code: "invalid_stack", statusCode: 400, message: err.Error()}
	case errors.Is(err, container.ErrReservedEnvName):
		return &apiError{code: "invalid_stack_env", statusCode: 400, message: err.Error()}
	case errors.Is(err, container.ErrStackNotFound):
		return &apiError{code: "stack_not_found", statusCode: 404, message: fmt.Sprintf("no stack %q", name)}
	case errors.Is(err, container.ErrStackExists):
		return &apiError{code: "stack_exists", statusCode: 409, message: fmt.Sprintf("a stack named %q already exists", name)}
	case errors.Is(err, container.ErrStackDirExists):
		return &apiError{code: "stack_dir_exists", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrStackDirUnsafe):
		return &apiError{code: "stack_dir_unsafe", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrStackProjectShared):
		return &apiError{code: "stack_project_shared", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrComposeUnavailable):
		return &apiError{code: "compose_unavailable", statusCode: 503, message: err.Error()}
	case errors.Is(err, container.ErrUnavailable):
		return &apiError{code: "docker_unavailable", statusCode: 503, message: fmt.Sprintf("Docker is not installed or not reachable: %v", err)}
	case errors.Is(err, container.ErrArrayStopped):
		return &apiError{code: "array_stopped", statusCode: 409, message: fmt.Sprintf("%v — stack %q was left as it was", err, name)}
	case errors.Is(err, container.ErrArrayStateUnknown):
		return &apiError{code: "array_state_unknown", statusCode: 503, message: fmt.Sprintf("%v — stack %q was left as it was", err, name)}
	case errors.Is(err, container.ErrAppdataShared):
		return &apiError{code: "appdata_shared", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrAppdataUnavailable):
		return &apiError{code: "appdata_unavailable", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrComposeFailed):
		return &apiError{code: "stack_action_failed", statusCode: 502, message: fmt.Sprintf("%s stack %q failed: %v", verb, name, err)}
	default:
		return &apiError{code: "stack_action_failed", statusCode: 500, message: fmt.Sprintf("%s stack %q failed: %v", verb, name, err)}
	}
}

func stackToAPI(s container.Stack) apiv1.Stack {
	return apiv1.Stack{
		Name: s.Name,
		Template: apiv1.StackTemplate{
			Source:   s.TemplateSource,
			ID:       s.TemplateID,
			Revision: s.TemplateRevision,
		},
		InstalledAt:    s.InstalledAt,
		ManuallyEdited: s.ManuallyEdited,
	}
}

// stackWithComposeToAPI is stackToAPI with the stored Compose text, which
// only getStack and updateStack return.
func stackWithComposeToAPI(s container.Stack) apiv1.Stack {
	out := stackToAPI(s)
	out.Compose = apiv1.NewOptString(s.Compose)
	return out
}

func (h *Handler) ListStacks(ctx context.Context) (*apiv1.ListStacksOK, error) {
	if h.Stacks == nil {
		return nil, errStacksNotConfigured()
	}
	stacks, err := h.Stacks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing stacks: %w", err)
	}
	out := &apiv1.ListStacksOK{Stacks: make([]apiv1.Stack, len(stacks))}
	for i, s := range stacks {
		out.Stacks[i] = stackToAPI(s)
	}
	return out, nil
}

func (h *Handler) GetStack(ctx context.Context, params apiv1.GetStackParams) (*apiv1.Stack, error) {
	if h.Stacks == nil {
		return nil, errStacksNotConfigured()
	}
	s, err := h.Stacks.Get(ctx, params.Name)
	if err != nil {
		return nil, mapStackError(params.Name, err, "reading")
	}
	out := stackWithComposeToAPI(s)
	return &out, nil
}

func (h *Handler) CreateStack(ctx context.Context, req *apiv1.CreateStackRequest) (*apiv1.Stack, error) {
	if h.Stacks == nil {
		return nil, errStacksNotConfigured()
	}
	n := container.NewStack{
		Name:    req.Name,
		Compose: req.Compose,
		Env:     req.Env.Or(""),
	}
	if t, ok := req.Template.Get(); ok {
		n.TemplateSource, n.TemplateID, n.TemplateRevision = t.Source, t.ID, t.Revision
	}
	s, err := h.Stacks.Create(ctx, n)
	if err != nil {
		return nil, mapStackError(req.Name, err, "creating")
	}
	out := stackToAPI(s)
	return &out, nil
}

func (h *Handler) RemoveStack(ctx context.Context, params apiv1.RemoveStackParams) (*apiv1.RemoveStackResult, error) {
	if h.Stacks == nil {
		return nil, errStacksNotConfigured()
	}
	res, err := h.Stacks.Remove(ctx, params.Name, params.DeleteAppdata.Or(false))
	if err != nil {
		if len(res.DeletedPaths) > 0 {
			return nil, &apiError{code: "stack_delete_incomplete", statusCode: 500, message: fmt.Sprintf("%v (deleted: %v)", err, res.DeletedPaths)}
		}
		return nil, mapStackError(params.Name, err, "removing")
	}
	return &apiv1.RemoveStackResult{DeletedPaths: res.DeletedPaths}, nil
}

func (h *Handler) UpdateStack(ctx context.Context, req *apiv1.UpdateStackRequest, params apiv1.UpdateStackParams) (*apiv1.UpdateStackResult, error) {
	if h.Stacks == nil {
		return nil, errStacksNotConfigured()
	}
	dryRun := params.DryRun.Or(false)
	s, err := h.Stacks.Update(ctx, params.Name, req.Compose, dryRun)
	if err != nil {
		return nil, mapStackError(params.Name, err, "editing")
	}
	return &apiv1.UpdateStackResult{Applied: !dryRun, Stack: stackWithComposeToAPI(s)}, nil
}

// StartStack queues the start as a job: `docker compose up` pulls images,
// which can take minutes, and must not hold a request open (doc 01 §4).
func (h *Handler) StartStack(ctx context.Context, params apiv1.StartStackParams) (*apiv1.Job, error) {
	if h.Stacks == nil {
		return nil, errStacksNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	if _, err := h.Stacks.Get(ctx, params.Name); err != nil {
		return nil, mapStackError(params.Name, err, "starting")
	}
	// Up checks again when the job runs; this refuses up front instead of
	// queueing a job that can only fail.
	if err := h.Stacks.RequireRunning(); err != nil {
		return nil, mapStackError(params.Name, err, "starting")
	}
	body, err := encodeStackStartParams(params.Name)
	if err != nil {
		return nil, err
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeStackStart, []string{"stack:" + params.Name}, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func encodeStackStartParams(name string) ([]byte, error) {
	body, err := json.Marshal(job.StackStartParams{Name: name})
	if err != nil {
		return nil, fmt.Errorf("encoding stack_start params: %w", err)
	}
	return body, nil
}
