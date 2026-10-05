package api

import (
	"context"
	"errors"
	"path"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"

	"github.com/google/uuid"
)

// migrateStackError maps what Phase D's operations can return: the migrator's
// own refusals first, then the stack layer's, which the stack operations map
// the same way.
func migrateStackError(stack string, err error, verb string) error {
	mapped := migrateError(err)
	var ae *apiError
	if errors.As(mapped, &ae) {
		return mapped
	}
	return mapStackError(stack, err, verb)
}

func optPositiveInt(n int) apiv1.OptInt {
	if n == 0 {
		return apiv1.OptInt{}
	}
	return apiv1.NewOptInt(n)
}

func migrationStackToAPI(st migrate.MigratedStack, awaiting bool) apiv1.MigrationContainerStack {
	return apiv1.MigrationContainerStack{
		Name: st.Name, Source: st.Source, Kind: apiv1.MigrationContainerStackKind(st.Kind), State: apiv1.MigrationContainerStackState(st.State),
		AutostartPosition: optPositiveInt(st.Position), WaitSeconds: optPositiveInt(st.WaitSeconds),
		Awaiting: awaiting, Checked: st.Checked, CheckFailed: st.CheckFailed,
	}
}

// ListMigrationContainers returns what Phase D can create from the scan and
// what it has created (doc 05 §4 steps 19 and 20).
func (h *Handler) ListMigrationContainers(ctx context.Context) (*apiv1.MigrationContainers, error) {
	if h.Migration == nil {
		return nil, errMigrationNotConfigured()
	}
	o, err := h.Migration.Offer(ctx)
	if err != nil {
		return nil, migrateStackError("", err, "listing")
	}
	out := &apiv1.MigrationContainers{
		ParityInitialized: o.ParityInitialized,
		Templates:         make([]apiv1.MigrationContainerTemplate, 0, len(o.Templates)),
		ComposeProjects:   make([]apiv1.MigrationContainerProject, 0, len(o.Projects)),
		ByHand:            make([]apiv1.MigrationByHandContainer, 0, len(o.ByHand)),
		Stacks:            make([]apiv1.MigrationContainerStack, 0, len(o.Stacks)),
		Awaiting:          optString(o.Awaiting),
		Next:              optString(o.Next),
	}
	for _, t := range o.Templates {
		out.Templates = append(out.Templates, apiv1.MigrationContainerTemplate{
			Name: t.Entry.Name, File: path.Base(t.Entry.File), Class: apiv1.MigrationTemplateClass(t.Entry.Class),
			Status: apiv1.MigrationTemplateStatus(t.Entry.Outcome.Status), WarningCount: t.Entry.Outcome.ActionWarnings(),
			Error: optString(t.Entry.Outcome.FailureText()), Stack: optString(t.Stack),
			AutostartPosition: optPositiveInt(t.Entry.AutostartPosition), AutostartWaitSeconds: optPositiveInt(t.Entry.AutostartWaitSeconds),
			Creatable: t.Creatable, Preselected: t.Preselected, Created: t.Created,
		})
	}
	for _, p := range o.Projects {
		status, errText := composeProjectStatus(p.Project.Outcome)
		containers := p.Project.Containers
		if containers == nil {
			containers = []string{}
		}
		out.ComposeProjects = append(out.ComposeProjects, apiv1.MigrationContainerProject{
			Name: p.Project.Name, Containers: containers, Status: status, Error: optString(errText),
			Stack: optString(p.Stack), Creatable: p.Creatable, Created: p.Created,
		})
	}
	for _, b := range o.ByHand {
		out.ByHand = append(out.ByHand, apiv1.MigrationByHandContainer{Name: b.Name, Image: optString(b.Image)})
	}
	for _, st := range o.Stacks {
		out.Stacks = append(out.Stacks, migrationStackToAPI(st.MigratedStack, st.Awaiting))
	}
	return out, nil
}

// CreateMigrationStacks creates a stopped Compose stack for each selected
// template or Compose Manager project (doc 05 §4 step 19), after the parity
// initialisation is confirmed. A stack that could not be made is reported in
// its own result.
func (h *Handler) CreateMigrationStacks(ctx context.Context, req *apiv1.MigrationStacksRequest) (*apiv1.MigrationStacksCreated, error) {
	if h.Migration == nil {
		return nil, errMigrationNotConfigured()
	}
	sel := make([]migrate.StackSelection, 0, len(req.Items))
	for _, it := range req.Items {
		sel = append(sel, migrate.StackSelection{Name: it.Name, Acknowledged: it.Acknowledged.Or(false)})
	}
	results, err := h.Migration.CreateStacks(ctx, sel)
	if err != nil {
		return nil, migrateStackError("", err, "creating")
	}
	out := &apiv1.MigrationStacksCreated{Results: make([]apiv1.MigrationStackResult, 0, len(results))}
	for _, r := range results {
		item := apiv1.MigrationStackResult{Name: r.Name, Stack: r.Stack, Status: apiv1.MigrationStackResultStatusCreated}
		switch {
		case r.Err != nil:
			item.Status = apiv1.MigrationStackResultStatusFailed
			var ae *apiError
			if mapped := migrateStackError(r.Stack, r.Err, "creating"); errors.As(mapped, &ae) {
				item.Error = apiv1.NewOptError(apiv1.Error{Code: ae.code, Message: ae.message})
			}
		case r.AlreadyCreated:
			item.Status = apiv1.MigrationStackResultStatusAlreadyCreated
		}
		out.Results = append(out.Results, item)
	}
	return out, nil
}

// StartMigrationContainer queues the start of one migrated stack as a
// stack_start job (doc 05 §4 step 20), refusing while another is unconfirmed.
func (h *Handler) StartMigrationContainer(ctx context.Context, params apiv1.StartMigrationContainerParams) (*apiv1.Job, error) {
	if h.Migration == nil || h.Scheduler == nil {
		return nil, errMigrationNotConfigured()
	}
	var queued *job.Job
	err := h.Migration.StartContainer(ctx, params.Name, func(ctx context.Context, stack string) (string, error) {
		body, err := encodeStackStartParams(stack)
		if err != nil {
			return "", err
		}
		j, err := h.Scheduler.Submit(ctx, job.TypeStackStart, []string{"stack:" + stack}, body)
		if err != nil {
			return "", mapSchedulerError(uuid.Nil, err)
		}
		queued = j
		return j.ID, nil
	})
	if err != nil {
		return nil, migrateStackError(params.Name, err, "starting")
	}
	return jobToAPI(queued)
}

// CheckMigrationContainer reads what a started stack's containers see of their
// data.
func (h *Handler) CheckMigrationContainer(ctx context.Context, params apiv1.CheckMigrationContainerParams) (*apiv1.MigrationContainerCheck, error) {
	if h.Migration == nil {
		return nil, errMigrationNotConfigured()
	}
	c, err := h.Migration.CheckContainer(ctx, params.Name)
	if err != nil {
		return nil, migrateStackError(params.Name, err, "checking")
	}
	out := &apiv1.MigrationContainerCheck{Stack: c.Stack, Running: c.Running, AllOk: c.AllOK, Paths: make([]apiv1.MigrationDataPath, 0, len(c.Paths))}
	for _, p := range c.Paths {
		out.Paths = append(out.Paths, apiv1.MigrationDataPath{
			Container: p.Container, Path: p.Path, Destination: p.Destination,
			Status: apiv1.MigrationDataPathStatus(p.Status), Error: optString(p.Error),
		})
	}
	return out, nil
}

// ConfirmMigrationContainer records that the user confirmed a started stack
// sees its data.
func (h *Handler) ConfirmMigrationContainer(ctx context.Context, req apiv1.OptMigrationContainerConfirmRequest, params apiv1.ConfirmMigrationContainerParams) (*apiv1.MigrationContainerStack, error) {
	if h.Migration == nil {
		return nil, errMigrationNotConfigured()
	}
	st, err := h.Migration.ConfirmContainer(ctx, params.Name, req.Or(apiv1.MigrationContainerConfirmRequest{}).AcceptFailedCheck.Or(false))
	if err != nil {
		return nil, migrateStackError(params.Name, err, "confirming")
	}
	out := migrationStackToAPI(st, false)
	return &out, nil
}
