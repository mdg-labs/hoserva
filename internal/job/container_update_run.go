package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Container update job modes: the daily registry check, updating one or more
// containers, and reverting one container's last update.
const (
	ContainerUpdateModeCheck  = "check"
	ContainerUpdateModeUpdate = "update"
	ContainerUpdateModeRevert = "revert"
)

// ContainerUpdateParams is container_update's persisted payload. Mode
// "check" is the daily registry check; JitterSeconds is how long the run
// waits before it asks any registry, so installations do not all ask at the
// same minute (Q81). Mode "update" updates Containers one after another;
// mode "revert" reverts the one container in Containers, and Sharers are the
// other containers its appdata restore may stop (they are in the job's
// scope, so nothing recreates them meanwhile).
type ContainerUpdateParams struct {
	Mode          string   `json:"mode"`
	JitterSeconds int      `json:"jitterSeconds,omitempty"`
	Containers    []string `json:"containers,omitempty"`
	Sharers       []string `json:"sharers,omitempty"`
}

func decodeContainerUpdateParams(params []byte) (ContainerUpdateParams, error) {
	var p ContainerUpdateParams
	if err := decodeJSON(bytes.TrimSpace(params), &p); err != nil {
		return ContainerUpdateParams{}, err
	}
	switch p.Mode {
	case ContainerUpdateModeCheck, ContainerUpdateModeUpdate, ContainerUpdateModeRevert:
	default:
		return ContainerUpdateParams{}, fmt.Errorf("job: container_update mode must be %q, %q or %q", ContainerUpdateModeCheck, ContainerUpdateModeUpdate, ContainerUpdateModeRevert)
	}
	if p.JitterSeconds < 0 {
		return ContainerUpdateParams{}, errors.New("job: container_update jitterSeconds must not be negative")
	}
	if p.JitterSeconds > 0 && p.Mode != ContainerUpdateModeCheck {
		return ContainerUpdateParams{}, fmt.Errorf("job: container_update jitterSeconds only applies to mode %q", ContainerUpdateModeCheck)
	}
	switch p.Mode {
	case ContainerUpdateModeCheck:
		if len(p.Containers) > 0 || len(p.Sharers) > 0 {
			return ContainerUpdateParams{}, fmt.Errorf("job: container_update mode %q takes no containers", p.Mode)
		}
	case ContainerUpdateModeUpdate:
		if len(p.Containers) == 0 {
			return ContainerUpdateParams{}, errors.New("job: container_update mode \"update\" needs at least one container")
		}
		if len(p.Sharers) > 0 {
			return ContainerUpdateParams{}, errors.New("job: container_update sharers only apply to mode \"revert\"")
		}
	case ContainerUpdateModeRevert:
		if len(p.Containers) != 1 {
			return ContainerUpdateParams{}, errors.New("job: container_update mode \"revert\" needs exactly one container")
		}
	}
	seen := map[string]bool{}
	for _, name := range append(append([]string(nil), p.Containers...), p.Sharers...) {
		if name == "" {
			return ContainerUpdateParams{}, errors.New("job: container_update has an empty container name")
		}
		if seen[name] {
			return ContainerUpdateParams{}, fmt.Errorf("job: container_update names %q twice", name)
		}
		seen[name] = true
	}
	return p, nil
}

// ContainerUpdateDeps is what RunContainerUpdate runs against; tests inject
// fakes.
type ContainerUpdateDeps struct {
	// Check runs the registry check (container.UpdateChecker.Run).
	Check func(ctx context.Context, out io.Writer) error
	// Update updates one container (container.Updater.Update).
	Update func(ctx context.Context, name string, out io.Writer) error
	// Revert reverts one container's last update (container.Updater.Revert).
	Revert func(ctx context.Context, name string, sharers []string, out io.Writer) error
	// Wait sleeps for d or until ctx ends; nil waits on a real timer.
	Wait func(ctx context.Context, d time.Duration) error
}

func waitTimer(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RunContainerUpdate is the RunFunc hoservad registers for
// TypeContainerUpdate.
//
// A cancel is honoured between containers and never inside one: an update
// or revert replaces a container and restores appdata, and a cancel that
// arrived between those steps would leave the two half done, so each runs to
// the end under a context a cancel cannot reach. The containers after the
// cancel are not touched. One container's failure does not stop the others;
// the job fails, naming each, once they have all been tried.
func RunContainerUpdate(deps ContainerUpdateDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeContainerUpdateParams(rc.Params())
		if err != nil {
			return err
		}
		switch p.Mode {
		case ContainerUpdateModeUpdate:
			return runContainerUpdates(ctx, rc, deps, p)
		case ContainerUpdateModeRevert:
			if deps.Revert == nil {
				return errors.New("job: container reverts are not configured on this daemon")
			}
			name := p.Containers[0]
			_, _ = fmt.Fprintf(rc.Output(), "reverting %s\n", name)
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := deps.Revert(context.WithoutCancel(ctx), name, p.Sharers, rc.Output()); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			return nil
		}
		if p.JitterSeconds > 0 {
			wait := deps.Wait
			if wait == nil {
				wait = waitTimer
			}
			_, _ = fmt.Fprintf(rc.Output(), "waiting %ds before asking the registries\n", p.JitterSeconds)
			if err := wait(ctx, time.Duration(p.JitterSeconds)*time.Second); err != nil {
				return err
			}
		}
		return deps.Check(ctx, rc.Output())
	}
}

func runContainerUpdates(ctx context.Context, rc *RunContext, deps ContainerUpdateDeps, p ContainerUpdateParams) error {
	if deps.Update == nil {
		return errors.New("job: container updates are not configured on this daemon")
	}
	var failures []error
	for i, name := range p.Containers {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, fmt.Errorf("cancelled before updating %s and the %d containers after it: %w", name, len(p.Containers)-i-1, err))...)
		}
		_, _ = fmt.Fprintf(rc.Output(), "updating %s (%d of %d)\n", name, i+1, len(p.Containers))
		if err := deps.Update(context.WithoutCancel(ctx), name, rc.Output()); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d containers were not updated: %w", len(failures), len(p.Containers), errors.Join(failures...))
	}
	return nil
}
