package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Container update job modes. Only the check is implemented here; the
// update mode is reserved for applying one, which is not built yet.
const (
	ContainerUpdateModeCheck  = "check"
	ContainerUpdateModeUpdate = "update"
)

// ErrContainerUpdateNotImplemented is what an update-mode run fails with:
// the job never reports success for an update it did not perform.
var ErrContainerUpdateNotImplemented = errors.New("job: applying a container update is not implemented yet")

// ContainerUpdateParams is container_update's persisted payload. Mode
// "check" is the daily registry check; JitterSeconds is how long the run
// waits before it asks any registry, so installations do not all ask at the
// same minute (Q81).
type ContainerUpdateParams struct {
	Mode          string `json:"mode"`
	JitterSeconds int    `json:"jitterSeconds,omitempty"`
}

func decodeContainerUpdateParams(params []byte) (ContainerUpdateParams, error) {
	var p ContainerUpdateParams
	if err := decodeJSON(bytes.TrimSpace(params), &p); err != nil {
		return ContainerUpdateParams{}, err
	}
	if p.Mode != ContainerUpdateModeCheck && p.Mode != ContainerUpdateModeUpdate {
		return ContainerUpdateParams{}, fmt.Errorf("job: container_update mode must be %q or %q", ContainerUpdateModeCheck, ContainerUpdateModeUpdate)
	}
	if p.JitterSeconds < 0 {
		return ContainerUpdateParams{}, errors.New("job: container_update jitterSeconds must not be negative")
	}
	return p, nil
}

// ContainerUpdateDeps is what RunContainerUpdate runs against; tests inject
// fakes.
type ContainerUpdateDeps struct {
	// Check runs the registry check (container.UpdateChecker.Run).
	Check func(ctx context.Context, out io.Writer) error
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
func RunContainerUpdate(deps ContainerUpdateDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeContainerUpdateParams(rc.Params())
		if err != nil {
			return err
		}
		if p.Mode != ContainerUpdateModeCheck {
			return ErrContainerUpdateNotImplemented
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
