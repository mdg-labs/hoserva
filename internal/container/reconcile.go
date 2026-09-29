package container

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

// ReconcileOutcome is what Reconcile did about one container name.
type ReconcileOutcome string

const (
	// ReconcileRestored: the original was renamed back to its own name
	// (and started again if it had been running).
	ReconcileRestored ReconcileOutcome = "restored"
	// ReconcileCleaned: the original was already in place; the unused
	// replacement was removed.
	ReconcileCleaned ReconcileOutcome = "cleaned"
	// ReconcileAmbiguous: nothing was removed, because the names and
	// state left behind do not say which container to keep.
	ReconcileAmbiguous ReconcileOutcome = "ambiguous"
)

// Reconciliation reports one container name that an interrupted Recreate
// left containers behind for.
type Reconciliation struct {
	Name    string
	Outcome ReconcileOutcome
	Detail  string
}

// Reconcile looks for what a Recreate that was killed between its steps
// leaves behind: a container under <name>_hoserva-old, the original
// waiting to be replaced, and/or one under <name>_hoserva-new, the
// replacement. Only what the Engine still holds decides, so a pass that
// fails part-way is finished by the next one.
//
//   - <name> is absent: the aside original gets its name back and is
//     started again if the Recreate had stopped it while it was running.
//   - <name> is the original and a replacement waits beside it: the
//     original is started again if the Recreate had stopped it.
//   - In both, the replacement is removed only after the original is
//     confirmed in place, only if the Engine never started it, and never
//     with its volumes, which are the original's too.
//   - Anything else, including a swap that got as far as naming the
//     replacement <name>, is reported as ambiguous and left as it is.
//
// The caller decides when it may run: starting the original needs the
// array up, like every other start. A container that is not part of a
// recreate is never looked at.
func (c *EngineClient) Reconcile(ctx context.Context) ([]Reconciliation, error) {
	all, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Container, len(all))
	bases := map[string]bool{}
	for _, ct := range all {
		byName[ct.Name] = ct
		for _, suffix := range []string{oldNameSuffix, newNameSuffix} {
			if base, ok := strings.CutSuffix(ct.Name, suffix); ok && base != "" {
				bases[base] = true
			}
		}
	}
	names := make([]string, 0, len(bases))
	for base := range bases {
		names = append(names, base)
	}
	sort.Strings(names)

	var out []Reconciliation
	var errs []error
	for _, name := range names {
		orig, hasOrig := byName[name]
		old, hasOld := byName[name+oldNameSuffix]
		repl, hasRepl := byName[name+newNameSuffix]
		var r Reconciliation
		var err error
		switch {
		case hasOrig && hasOld:
			r = ambiguous(name, fmt.Sprintf("%s, %s and possibly %s all exist, so it is not clear which one to keep", name, name+oldNameSuffix, name+newNameSuffix))
		case !hasOrig && !hasOld:
			r = ambiguous(name, fmt.Sprintf("only %s exists, with no original beside it", name+newNameSuffix))
		case !hasOrig:
			r, err = c.restoreAsideOriginal(ctx, name, old, repl, hasRepl)
		default:
			r, err = c.removeUnusedReplacement(ctx, name, orig, repl)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("container %q: %w", name, err))
			continue
		}
		out = append(out, r)
	}
	return out, errors.Join(errs...)
}

func ambiguous(name, detail string) Reconciliation {
	return Reconciliation{Name: name, Outcome: ReconcileAmbiguous, Detail: detail + "; nothing was removed"}
}

// restoreAsideOriginal handles <name> being absent: the original was
// renamed aside and the replacement never took its name.
func (c *EngineClient) restoreAsideOriginal(ctx context.Context, name string, old, repl Container, hasRepl bool) (Reconciliation, error) {
	oldInfo, err := c.inspectEngine(ctx, old.ID)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("inspecting %s: %w", old.Name, err)
	}
	var replInfo *dockercontainer.InspectResponse
	if hasRepl {
		info, err := c.inspectEngine(ctx, repl.ID)
		if err != nil {
			return Reconciliation{}, fmt.Errorf("inspecting %s: %w", repl.Name, err)
		}
		replInfo = &info
	}

	if err := reconcileStep(ctx, func(ctx context.Context) error {
		_, err := c.cli.ContainerRename(ctx, old.ID, dockerclient.ContainerRenameOptions{NewName: name})
		return err
	}); err != nil {
		return Reconciliation{}, fmt.Errorf("giving the original container its name back (it is called %s): %w", old.Name, mapEngineErr(err))
	}
	detail := fmt.Sprintf("renamed %s back to %s", old.Name, name)

	startDetail, err := c.startOriginalIfOwed(ctx, old.ID, oldInfo, replInfo)
	if err != nil {
		return Reconciliation{}, err
	}
	detail += startDetail

	if err := c.confirmInPlace(ctx, name, old.ID); err != nil {
		return Reconciliation{}, err
	}
	if replInfo == nil {
		return Reconciliation{Name: name, Outcome: ReconcileRestored, Detail: detail}, nil
	}
	if !neverStarted(replInfo.State) {
		detail += fmt.Sprintf("; kept %s, because the Engine has started it", repl.Name)
		return Reconciliation{Name: name, Outcome: ReconcileRestored, Detail: detail}, nil
	}
	if err := c.removeReplacement(ctx, repl); err != nil {
		return Reconciliation{}, err
	}
	detail += fmt.Sprintf("; removed the unused replacement %s", repl.Name)
	return Reconciliation{Name: name, Outcome: ReconcileRestored, Detail: detail}, nil
}

// removeUnusedReplacement handles <name> being the original with a
// replacement waiting beside it, which is where a kill before the
// original is renamed aside leaves things.
func (c *EngineClient) removeUnusedReplacement(ctx context.Context, name string, orig, repl Container) (Reconciliation, error) {
	replInfo, err := c.inspectEngine(ctx, repl.ID)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("inspecting %s: %w", repl.Name, err)
	}
	if !neverStarted(replInfo.State) {
		return ambiguous(name, fmt.Sprintf("%s exists beside %s and the Engine has started it", repl.Name, name)), nil
	}
	origInfo, err := c.inspectEngine(ctx, orig.ID)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("inspecting %s: %w", name, err)
	}
	startDetail, err := c.startOriginalIfOwed(ctx, orig.ID, origInfo, &replInfo)
	if err != nil {
		return Reconciliation{}, err
	}
	if err := c.confirmInPlace(ctx, name, orig.ID); err != nil {
		return Reconciliation{}, err
	}
	if err := c.removeReplacement(ctx, repl); err != nil {
		return Reconciliation{}, err
	}
	return Reconciliation{
		Name:    name,
		Outcome: ReconcileCleaned,
		Detail:  fmt.Sprintf("%s was still in place; removed the unused replacement %s%s", name, repl.Name, startDetail),
	}, nil
}

// startOriginalIfOwed starts the original again when the interrupted
// Recreate stopped it. The Engine does not record that it was running, so
// it is read off the timestamps: the replacement was created before the
// original was stopped, so an original that stopped after the replacement
// existed was stopped by the swap. It returns text for the report.
func (c *EngineClient) startOriginalIfOwed(ctx context.Context, id string, orig dockercontainer.InspectResponse, repl *dockercontainer.InspectResponse) (string, error) {
	owed, known := startOwed(orig, repl)
	if !known {
		return "; left stopped, because nothing shows whether it was running before the recreate", nil
	}
	if !owed {
		return "", nil
	}
	if err := reconcileStep(ctx, func(ctx context.Context) error {
		_, err := c.cli.ContainerStart(ctx, id, dockerclient.ContainerStartOptions{})
		return err
	}); err != nil {
		return "", fmt.Errorf("starting the original container again: %w", mapEngineErr(err))
	}
	return "; started it again", nil
}

// confirmInPlace checks that the container now under name is the original.
func (c *EngineClient) confirmInPlace(ctx context.Context, name, id string) error {
	got, err := c.inspectEngine(ctx, name)
	if err != nil {
		return fmt.Errorf("confirming the original container is back as %s: %w", name, err)
	}
	if got.ID != id {
		return fmt.Errorf("confirming the original container is back as %s: it is container %s, not %s", name, got.ID, id)
	}
	return nil
}

// removeReplacement removes the replacement without its volumes, which
// the original uses too.
func (c *EngineClient) removeReplacement(ctx context.Context, repl Container) error {
	if err := reconcileStep(ctx, func(ctx context.Context) error {
		_, err := c.cli.ContainerRemove(ctx, repl.ID, dockerclient.ContainerRemoveOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("removing the replacement %s: %w", repl.Name, mapEngineErr(err))
	}
	return nil
}

// reconcileStep gives each Engine call its own deadline, so a slow call
// cannot leave the next one too little time.
func reconcileStep(ctx context.Context, do func(context.Context) error) error {
	stepCtx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	return do(stepCtx)
}

// neverStarted reports whether the Engine has never run the container.
func neverStarted(st *dockercontainer.State) bool {
	if st == nil || st.Running || st.Status != dockercontainer.StateCreated {
		return false
	}
	started, err := time.Parse(time.RFC3339Nano, st.StartedAt)
	return err == nil && started.IsZero()
}

// startOwed reports whether the original was running before the
// interrupted Recreate stopped it, and whether that can be told at all.
func startOwed(orig dockercontainer.InspectResponse, repl *dockercontainer.InspectResponse) (owed, known bool) {
	if orig.State == nil {
		return false, false
	}
	if orig.State.Running {
		return false, true
	}
	if repl == nil {
		return false, false
	}
	finished, err := time.Parse(time.RFC3339Nano, orig.State.FinishedAt)
	if err != nil {
		return false, false
	}
	created, err := time.Parse(time.RFC3339Nano, repl.Created)
	if err != nil {
		return false, false
	}
	return !finished.Before(created), true
}
