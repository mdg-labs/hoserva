package container

import (
	"context"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	dockerclient "github.com/moby/moby/client"
)

// PullImage downloads ref again; the Engine reports a failed pull inside a
// 200 response's progress stream, which pull reads to its end.
func (c *EngineClient) PullImage(ctx context.Context, ref string) error {
	if err := c.pull(ctx, ref); err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	return nil
}

// TagImage gives the local image with this ID the reference ref, moving the
// tag off any other image that holds it. A kept image is held this way: a
// tag the Engine's own cleanup leaves alone.
func (c *EngineClient) TagImage(ctx context.Context, imageID, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	_, err := c.cli.ImageTag(ctx, dockerclient.ImageTagOptions{Source: imageID, Target: ref})
	return imageErr(fmt.Sprintf("tagging image %s as %s", imageID, ref), err)
}

// UntagImage removes the reference ref. The Engine deletes the image itself
// only when ref was its last reference; one that a container still uses is
// refused with ErrImageInUse and keeps its reference.
func (c *EngineClient) UntagImage(ctx context.Context, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	_, err := c.cli.ImageRemove(ctx, ref, dockerclient.ImageRemoveOptions{PruneChildren: true})
	return imageErr("removing the reference "+ref, err)
}

// imageErr maps an Engine error from an image call. Its not-found is
// ErrImageNotFound, never ErrNotFound, which names a missing container.
func imageErr(what string, err error) error {
	switch {
	case err == nil:
		return nil
	case cerrdefs.IsNotFound(err):
		return fmt.Errorf("%s: %w: %v", what, ErrImageNotFound, err)
	case cerrdefs.IsConflict(err):
		return fmt.Errorf("%s: %w: %v", what, ErrImageInUse, err)
	default:
		return fmt.Errorf("%s: %w", what, wrapEngineErr(err))
	}
}
