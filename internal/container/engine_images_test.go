package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	dockerclient "github.com/moby/moby/client"
)

func (e *scriptedEngine) ImageTag(ctx context.Context, o dockerclient.ImageTagOptions) (dockerclient.ImageTagResult, error) {
	return dockerclient.ImageTagResult{}, e.record(fmt.Sprintf("tag %s -> %s", o.Source, o.Target), "tag "+o.Target)
}

func (e *scriptedEngine) ImageRemove(ctx context.Context, ref string, o dockerclient.ImageRemoveOptions) (dockerclient.ImageRemoveResult, error) {
	return dockerclient.ImageRemoveResult{}, e.record("untag "+ref, "untag "+ref)
}

func TestRecreateLocal_NeverPulls(t *testing.T) {
	e := newScriptedEngine(true)
	if err := (&EngineClient{cli: e}).RecreateLocal(context.Background(), "jellyfin"); err != nil {
		t.Fatalf("RecreateLocal: %v", err)
	}
	assertCalls(t, e,
		"create jellyfin_hoserva-new",
		"stop old",
		"rename old -> jellyfin_hoserva-old",
		"rename new -> jellyfin",
		"start new",
		"remove old",
	)
}

// The original must survive a failed local recreate exactly as it survives a
// failed pull-and-recreate.
func TestRecreateLocal_FailedStartRestoresTheOriginal(t *testing.T) {
	e := newScriptedEngine(true)
	e.fail["start new"] = errors.New("port is already allocated")
	if err := (&EngineClient{cli: e}).RecreateLocal(context.Background(), "jellyfin"); err == nil {
		t.Fatal("RecreateLocal succeeded although the replacement did not start")
	}
	assertCalls(t, e,
		"create jellyfin_hoserva-new",
		"stop old",
		"rename old -> jellyfin_hoserva-old",
		"rename new -> jellyfin",
		"start new",
		"stop new",
		"rename new -> jellyfin_hoserva-new",
		"rename old -> jellyfin",
		"start old",
		"remove new",
	)
}

// PullImage touches no container, and reads the pull's progress stream to its
// end, where the Engine reports a failure.
func TestPullImage_OnlyPullsAndReportsAFailureInTheStream(t *testing.T) {
	e := newScriptedEngine(true)
	c := &EngineClient{cli: e}
	if err := c.PullImage(context.Background(), jellyfinRef); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	assertCalls(t, e, "pull "+jellyfinRef)

	e.pullMsg = `{"errorDetail":{"message":"denied"},"error":"pull access denied"}` + "\n"
	if err := c.PullImage(context.Background(), jellyfinRef); err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("PullImage = %v, want the failure the stream reported", err)
	}
}

// The Engine's listing reports the image ID in place of the reference once
// the reference points at another image, so the reference an update works
// with comes from the container's own inspection.
func TestConfiguredImage_ReadsTheCreationReferenceNotTheListing(t *testing.T) {
	e := newScriptedEngine(true)
	c := &EngineClient{cli: e}
	got, err := c.ConfiguredImage(context.Background(), "jellyfin")
	if err != nil || got != (ConfiguredImage{Ref: jellyfinRef}) {
		t.Fatalf("ConfiguredImage = %+v, %v, want %s", got, err, jellyfinRef)
	}

	e.info.Config.Image = "postgres:16"
	if got, err := c.ConfiguredImage(context.Background(), "jellyfin"); err != nil || got.Ref != "postgres:16" || got.Pinned {
		t.Fatalf("ConfiguredImage = %+v, %v, want the familiar form of a Docker Hub library image", got, err)
	}
	e.info.Config.Image = "postgres"
	if got, err := c.ConfiguredImage(context.Background(), "jellyfin"); err != nil || got.Ref != "postgres:latest" {
		t.Fatalf("ConfiguredImage of an untagged reference = %+v, %v, want postgres:latest", got, err)
	}
	e.info.Config.Image = "ghcr.io/owner/app@sha256:" + strings.Repeat("a", 64)
	if got, err := c.ConfiguredImage(context.Background(), "jellyfin"); err != nil || !got.Pinned {
		t.Fatalf("ConfiguredImage of a digest reference = %+v, %v, want it pinned", got, err)
	}
	e.info.Config.Image = ""
	if _, err := c.ConfiguredImage(context.Background(), "jellyfin"); err == nil {
		t.Fatal("ConfiguredImage with no reference returned no error")
	}
}

// The update check reads the reference of every container on each request,
// so one read must not list every container.
func TestConfiguredImage_InspectsOnlyThatContainer(t *testing.T) {
	e := newScriptedEngine(true)
	c := &EngineClient{cli: e}
	for _, id := range []string{oldContainerID, "jellyfin"} {
		if got, err := c.ConfiguredImage(context.Background(), id); err != nil || got.Ref != jellyfinRef {
			t.Fatalf("ConfiguredImage(%s) = %+v, %v, want %s", id, got, err, jellyfinRef)
		}
	}
	if e.lists != 0 {
		t.Fatalf("ConfiguredImage listed the containers %d times, want none", e.lists)
	}
}

// The Engine also matches a prefix of an ID; Inspect never does, and neither
// does ConfiguredImage.
func TestConfiguredImage_RefusesAnIDPrefix(t *testing.T) {
	e := newScriptedEngine(true)
	c := &EngineClient{cli: e}
	if _, err := c.ConfiguredImage(context.Background(), oldContainerID[:12]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConfiguredImage of an ID prefix = %v, want ErrNotFound", err)
	}
}

func TestTagAndUntagImage(t *testing.T) {
	e := newScriptedEngine(true)
	c := &EngineClient{cli: e}
	ctx := context.Background()
	if err := c.TagImage(ctx, "sha256:old", "hoserva-previous:3"); err != nil {
		t.Fatalf("TagImage: %v", err)
	}
	if err := c.UntagImage(ctx, "hoserva-previous:3"); err != nil {
		t.Fatalf("UntagImage: %v", err)
	}
	assertCalls(t, e, "tag sha256:old -> hoserva-previous:3", "untag hoserva-previous:3")
}

func TestUntagImage_MapsTheEnginesRefusals(t *testing.T) {
	e := newScriptedEngine(true)
	c := &EngineClient{cli: e}
	ctx := context.Background()

	e.fail["untag hoserva-previous:3"] = cerrdefs.ErrConflict.WithMessage("image is being used by stopped container abc")
	if err := c.UntagImage(ctx, "hoserva-previous:3"); !errors.Is(err, ErrImageInUse) {
		t.Fatalf("UntagImage of an image a container uses = %v, want ErrImageInUse", err)
	}
	e.fail["untag hoserva-previous:3"] = cerrdefs.ErrNotFound.WithMessage("no such image")
	if err := c.UntagImage(ctx, "hoserva-previous:3"); !errors.Is(err, ErrImageNotFound) || errors.Is(err, ErrNotFound) {
		t.Fatalf("UntagImage of an unknown reference = %v, want ErrImageNotFound and not the container's ErrNotFound", err)
	}
	e.fail["tag hoserva-previous:3"] = cerrdefs.ErrNotFound.WithMessage("no such image")
	if err := c.TagImage(ctx, "sha256:gone", "hoserva-previous:3"); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("TagImage of an unknown image = %v, want ErrImageNotFound", err)
	}
}
