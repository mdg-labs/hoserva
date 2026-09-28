package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// ContainerRecreateParams is recreateApp's persisted payload: the
// container's Engine ID or name.
type ContainerRecreateParams struct {
	ID string `json:"id"`
}

func decodeContainerRecreateParams(params []byte) (ContainerRecreateParams, error) {
	var p ContainerRecreateParams
	if err := decodeJSON(bytes.TrimSpace(params), &p); err != nil {
		return ContainerRecreateParams{}, err
	}
	if p.ID == "" {
		return ContainerRecreateParams{}, errors.New("job: container_recreate has no container id")
	}
	return p, nil
}

// RunContainerRecreate is the RunFunc hoservad registers for
// TypeContainerRecreate. recreate replaces the container (container
// .Lifecycle.Recreate); tests inject a fake. A recreate that fails says in
// its error what state the original container was left in, and the
// scheduler records that error on the job.
func RunContainerRecreate(recreate func(ctx context.Context, id string) error) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeContainerRecreateParams(rc.Params())
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(rc.Output(), "recreating container %s\n", p.ID)
		if err := recreate(ctx, p.ID); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(rc.Output(), "container %s recreated\n", p.ID)
		return nil
	}
}
