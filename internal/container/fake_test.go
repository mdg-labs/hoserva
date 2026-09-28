package container

import (
	"context"
	"errors"
	"testing"
)

func TestFakeProvider_DefaultVersion(t *testing.T) {
	f := NewFakeProvider()
	v, err := f.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Version == "" || v.APIVersion == "" {
		t.Fatalf("Version() = %+v, want a non-empty default", v)
	}
}

func TestFakeProvider_SetVersion_OldEngine(t *testing.T) {
	f := NewFakeProvider()
	f.SetVersion(EngineVersion{Version: "18.09.0", APIVersion: "1.39", MinAPIVersion: "1.12"})

	v, err := f.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Version != "18.09.0" {
		t.Fatalf("Version() = %+v, want the scripted old release", v)
	}
}

func TestFakeProvider_SetUnavailable(t *testing.T) {
	f := NewFakeProvider()
	f.SetUnavailable(nil)

	if _, err := f.Version(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Version() error = %v, want ErrUnavailable", err)
	}
	if _, err := f.List(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("List() error = %v, want ErrUnavailable", err)
	}
	if _, err := f.Images(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Images() error = %v, want ErrUnavailable", err)
	}
}

func TestFakeProvider_ListAndInspect(t *testing.T) {
	f := NewFakeProvider()
	f.AddContainer(Container{ID: "abc123", Name: "jellyfin", Image: "lscr.io/linuxserver/jellyfin", Tag: "10.9.7", State: "running"})

	list, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "jellyfin" {
		t.Fatalf("List() = %+v", list)
	}

	byID, err := f.Inspect(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Inspect by id: %v", err)
	}
	if byID.Name != "jellyfin" {
		t.Fatalf("Inspect(abc123) = %+v", byID)
	}

	byName, err := f.Inspect(context.Background(), "jellyfin")
	if err != nil {
		t.Fatalf("Inspect by name: %v", err)
	}
	if byName.ID != "abc123" {
		t.Fatalf("Inspect(jellyfin) = %+v", byName)
	}

	if _, err := f.Inspect(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect(nope) error = %v, want ErrNotFound", err)
	}
}

func TestFakeProvider_Images(t *testing.T) {
	f := NewFakeProvider()
	f.AddImage(Image{ID: "sha256:abc", RepoTags: []string{"alpine:3.20"}, Size: 100})

	images, err := f.Images(context.Background())
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(images) != 1 || images[0].ID != "sha256:abc" {
		t.Fatalf("Images() = %+v", images)
	}
}
