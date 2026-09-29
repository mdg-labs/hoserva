package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	dockermount "github.com/moby/moby/api/types/mount"
	dockernetwork "github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"
)

const (
	// pullTimeout bounds an image pull, which downloads whole layers.
	pullTimeout = 15 * time.Minute
	// newNameSuffix and oldNameSuffix name the replacement while it is
	// being built and the original while it is being replaced, so the
	// container's own name is never free for anyone else in between.
	newNameSuffix = "_hoserva-new"
	oldNameSuffix = "_hoserva-old"
)

// Recreate replaces the container with one built from the same image
// reference, configuration, volumes and networks, after pulling the image
// again. The replacement is created under a temporary name first, so a
// failed pull or create leaves the original untouched. Only then is the
// original stopped, renamed aside and replaced; a failure at any of those
// steps puts the original back — same name, running again if it was
// running — and removes the replacement, but only once the original is
// back; if it cannot be restored the replacement is kept. A container the
// Engine deletes when it stops (--rm) is refused up front. The original is
// deleted last,
// without its volumes, which the replacement now uses.
func (c *EngineClient) Recreate(ctx context.Context, id string) error {
	ct, err := c.resolve(ctx, id)
	if err != nil {
		return err
	}
	old, err := c.inspectEngine(ctx, ct.ID)
	if err != nil {
		return err
	}
	if old.Config == nil || old.HostConfig == nil {
		return fmt.Errorf("container: the Engine returned an incomplete description of %q", ct.Name)
	}
	if old.HostConfig.AutoRemove {
		return fmt.Errorf("container: %q was started with --rm, so the Engine deletes it the moment it stops and it cannot be recreated", ct.Name)
	}
	name := strings.TrimPrefix(old.Name, "/")
	wasRunning := old.State != nil && old.State.Running

	if err := c.pull(ctx, old.Config.Image); err != nil {
		return fmt.Errorf("pulling %s (the existing container is untouched): %w", old.Config.Image, err)
	}

	cfg, host, nets := recreateSpec(old)
	createCtx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	created, err := c.cli.ContainerCreate(createCtx, dockerclient.ContainerCreateOptions{
		Config:           cfg,
		HostConfig:       host,
		NetworkingConfig: nets,
		Name:             name + newNameSuffix,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("creating the replacement container (the existing container is untouched): %w", mapEngineErr(err))
	}

	return c.swap(ctx, old, created.ID, name, wasRunning)
}

// swap moves name from the original container to the created one.
func (c *EngineClient) swap(ctx context.Context, old dockercontainer.InspectResponse, newID, name string, wasRunning bool) error {
	var stoppedOld, renamedOld, renamedNew, startedNew bool

	rollback := func(step string, cause error) error {
		errs := []error{fmt.Errorf("%s: %w", step, mapEngineErr(cause))}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*lifecycleTimeout)
		defer cancel()
		if startedNew {
			if _, err := c.cli.ContainerStop(rctx, newID, dockerclient.ContainerStopOptions{}); err != nil {
				errs = append(errs, fmt.Errorf("stopping the replacement: %w", err))
			}
		}
		if renamedNew {
			if _, err := c.cli.ContainerRename(rctx, newID, dockerclient.ContainerRenameOptions{NewName: name + newNameSuffix}); err != nil {
				errs = append(errs, fmt.Errorf("renaming the replacement aside: %w", err))
			}
		}
		originalRestored := true
		if renamedOld {
			if _, err := c.cli.ContainerRename(rctx, old.ID, dockerclient.ContainerRenameOptions{NewName: name}); err != nil {
				originalRestored = false
				errs = append(errs, fmt.Errorf("giving the original container its name back (it is now called %s): %w", name+oldNameSuffix, err))
			}
		}
		if stoppedOld && wasRunning {
			if _, err := c.cli.ContainerStart(rctx, old.ID, dockerclient.ContainerStartOptions{}); err != nil {
				originalRestored = false
				errs = append(errs, fmt.Errorf("starting the original container again: %w", err))
			}
		}
		if !originalRestored {
			errs = append(errs, fmt.Errorf("the replacement (%s) is left in place, because the original could not be restored", name+newNameSuffix))
		} else if _, err := c.cli.ContainerRemove(rctx, newID, dockerclient.ContainerRemoveOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("removing the replacement (%s): %w", name+newNameSuffix, err))
		}
		if len(errs) > 1 {
			return fmt.Errorf("the original container could not be fully restored: %w", errors.Join(errs...))
		}
		return fmt.Errorf("the original container was restored: %w", errs[0])
	}

	// Each step gets its own deadline: a slow stop can use most of
	// lifecycleTimeout, and a start that inherited only the remainder
	// would fail and roll back a swap that was going fine.
	step := func(do func(context.Context) error) error {
		stepCtx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
		defer cancel()
		return do(stepCtx)
	}

	if wasRunning {
		stoppedOld = true
		if err := step(func(ctx context.Context) error {
			_, err := c.cli.ContainerStop(ctx, old.ID, dockerclient.ContainerStopOptions{})
			return err
		}); err != nil {
			return rollback("stopping the original container", err)
		}
	}
	if err := step(func(ctx context.Context) error {
		_, err := c.cli.ContainerRename(ctx, old.ID, dockerclient.ContainerRenameOptions{NewName: name + oldNameSuffix})
		return err
	}); err != nil {
		return rollback("renaming the original container aside", err)
	}
	renamedOld = true
	if err := step(func(ctx context.Context) error {
		_, err := c.cli.ContainerRename(ctx, newID, dockerclient.ContainerRenameOptions{NewName: name})
		return err
	}); err != nil {
		return rollback("naming the replacement container", err)
	}
	renamedNew = true
	if wasRunning {
		startedNew = true
		if err := step(func(ctx context.Context) error {
			_, err := c.cli.ContainerStart(ctx, newID, dockerclient.ContainerStartOptions{})
			return err
		}); err != nil {
			return rollback("starting the replacement container", err)
		}
	}
	if err := step(func(ctx context.Context) error {
		_, err := c.cli.ContainerRemove(ctx, old.ID, dockerclient.ContainerRemoveOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("the container was replaced, but the original (%s) could not be removed: %w", name+oldNameSuffix, mapEngineErr(err))
	}
	return nil
}

// pull downloads ref again. The Engine reports a failed pull inside a
// 200 response's progress stream, so the stream is read to its end.
func (c *EngineClient) pull(ctx context.Context, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	rc, err := c.cli.ImagePull(ctx, ref, dockerclient.ImagePullOptions{})
	if err != nil {
		return mapEngineErr(err)
	}
	defer func() { _ = rc.Close() }()
	dec := json.NewDecoder(rc)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading pull progress: %w", err)
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
	}
}

// recreateSpec builds the create request for a replacement of old: the
// same configuration, host configuration and network attachments, with
// the runtime-only values the Engine assigned to old dropped. Labels are
// carried over exactly — nothing is added, so a container Hoserva did not
// install stays exactly what it was. Volumes the original mounted that its
// host configuration does not name (anonymous volumes) are attached to the
// replacement by name, so it sees the same data instead of fresh, empty
// volumes.
func recreateSpec(old dockercontainer.InspectResponse) (*dockercontainer.Config, *dockercontainer.HostConfig, *dockernetwork.NetworkingConfig) {
	cfg := *old.Config
	shortID := old.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	if cfg.Hostname == shortID {
		cfg.Hostname = ""
	}

	host := *old.HostConfig
	host.Mounts = append([]dockermount.Mount(nil), host.Mounts...)
	covered := map[string]bool{}
	for _, b := range host.Binds {
		if parts := strings.Split(b, ":"); len(parts) >= 2 {
			covered[parts[1]] = true
		}
	}
	for _, m := range host.Mounts {
		covered[m.Target] = true
	}
	for _, m := range old.Mounts {
		if m.Type != dockermount.TypeVolume || m.Name == "" || covered[m.Destination] {
			continue
		}
		host.Mounts = append(host.Mounts, dockermount.Mount{
			Type:     dockermount.TypeVolume,
			Source:   m.Name,
			Target:   m.Destination,
			ReadOnly: !m.RW,
		})
	}

	nets := &dockernetwork.NetworkingConfig{EndpointsConfig: map[string]*dockernetwork.EndpointSettings{}}
	if old.NetworkSettings != nil {
		for name, ep := range old.NetworkSettings.Networks {
			if name == "host" || name == "none" || ep == nil {
				continue
			}
			var aliases []string
			for _, a := range ep.Aliases {
				if a != shortID {
					aliases = append(aliases, a)
				}
			}
			nets.EndpointsConfig[name] = &dockernetwork.EndpointSettings{
				IPAMConfig: ep.IPAMConfig,
				Links:      ep.Links,
				Aliases:    aliases,
				DriverOpts: ep.DriverOpts,
			}
		}
	}
	return &cfg, &host, nets
}
