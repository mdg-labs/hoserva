package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
)

// GetShareRelocationPrecheck is what a caller shows before it starts a
// share relocation (doc 09 §2): the containers whose mounts use the share and
// the open files cache.Precheck finds. Nothing is moved, stopped or queued.
// It makes the refusals StartShareRelocation makes before it queues a job: no
// cache disk, no array, and the scheduler's admission checks (maintenance
// mode, an unfinished Unraid migration, a database restore), asked through
// Scheduler.CheckAdmission so no job is submitted to find out. The answer
// covers both sides of the share, whichever way it will move.
func (h *Handler) GetShareRelocationPrecheck(ctx context.Context, params apiv1.GetShareRelocationPrecheckParams) (*apiv1.ShareRelocationPrecheck, error) {
	if h.Shares == nil || h.RelocationShare == nil {
		return nil, errSharesNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	if _, err := h.Shares.Get(ctx, string(params.Name)); err != nil {
		return nil, mapShareError(err)
	}
	if err := h.requireCacheDisk(ctx); err != nil {
		return nil, err
	}
	if err := h.Scheduler.CheckAdmission(ctx, job.TypeShareRelocation); err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	sh, err := h.RelocationShare(ctx, string(params.Name))
	if err != nil {
		return nil, fmt.Errorf("resolving share %q for the relocation precheck: %w", params.Name, err)
	}

	out := &apiv1.ShareRelocationPrecheck{Containers: []apiv1.ShareRelocationContainer{}, OpenPaths: []string{}}
	containers, available, err := h.listContainersForPrecheck(ctx)
	if err != nil {
		return nil, err
	}
	out.DockerAvailable = available
	roots := append([]string{filepath.Join(pool.CatchAllPath, sh.Name), sh.CachePath}, sh.Branches...)
	for _, u := range container.UsingPaths(containers, roots) {
		out.Containers = append(out.Containers, apiv1.ShareRelocationContainer{
			ID:     u.Container.ID,
			Name:   u.Container.Name,
			State:  apiv1.AppState(u.Container.State),
			Active: u.Active,
			Mounts: u.Sources,
		})
	}

	res, err := cache.Precheck(ctx, sh, cache.Deps{Open: h.RelocationOpen})
	if err != nil {
		return nil, fmt.Errorf("listing the open files of share %q: %w", params.Name, err)
	}
	if res.OpenPaths != nil {
		out.OpenPaths = res.OpenPaths
	}
	return out, nil
}

// listContainersForPrecheck lists the Engine's containers. Docker being
// unconfigured or unreachable is the normal state of a host without
// containers (doc 04 §3) and reports available=false; any other failure fails
// the request, so an error never reads as "no container uses the share".
func (h *Handler) listContainersForPrecheck(ctx context.Context) ([]container.Container, bool, error) {
	if h.Container == nil {
		return nil, false, nil
	}
	containers, err := h.Container.List(ctx)
	if err != nil {
		if errors.Is(err, container.ErrUnavailable) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("listing containers for the relocation precheck: %w", err)
	}
	return containers, true, nil
}
