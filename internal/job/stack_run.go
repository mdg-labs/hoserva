package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// StackStartParams is startStack's persisted payload: the stack's name.
type StackStartParams struct {
	Name string `json:"name"`
}

func decodeStackStartParams(params []byte) (StackStartParams, error) {
	var p StackStartParams
	if err := decodeJSON(bytes.TrimSpace(params), &p); err != nil {
		return StackStartParams{}, err
	}
	if p.Name == "" {
		return StackStartParams{}, errors.New("job: stack_start has no stack name")
	}
	return p, nil
}

// RunStackStart is the RunFunc hoservad registers for TypeStackStart. up
// runs `docker compose up --detach` for the stack (container.StackService
// .Up); tests inject a fake.
func RunStackStart(up func(ctx context.Context, name string) error) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeStackStartParams(rc.Params())
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(rc.Output(), "starting stack %s\n", p.Name)
		if err := up(ctx, p.Name); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(rc.Output(), "stack %s started\n", p.Name)
		return nil
	}
}
