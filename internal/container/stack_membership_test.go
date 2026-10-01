package container

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"
)

type listFailingStackStore struct{ *memStackStore }

func (listFailingStackStore) List(context.Context) ([]store.Stack, error) {
	return nil, errors.New("database is locked")
}

func TestManagingStacks_NamesTheStackOnlyForTheContainersItStarted(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "immich")
	r.addContainer("immich", "immich-server-1")
	r.addContainerFrom("immich", filepath.Join(r.parent, "home", "immich"), "hand-run")
	r.addContainer("", "portainer")
	r.addContainer("gone", "orphan-1")

	all, err := r.fake.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.ManagingStacks(context.Background(), all)
	if err != nil {
		t.Fatalf("ManagingStacks() error = %v", err)
	}
	want := map[string]string{"id-immich-server-1": "immich"}
	if len(got) != len(want) || got["id-immich-server-1"] != "immich" {
		t.Fatalf("ManagingStacks() = %v, want %v: a project of the same name run from elsewhere, a container with no project and one of a project with no stack are all unmanaged", got, want)
	}
}

func TestManagingStacks_AFailedStackReadIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "immich")
	r.addContainer("immich", "immich-server-1")
	r.svc.Store = listFailingStackStore{r.store}
	all, _ := r.fake.List(context.Background())

	got, err := r.svc.ManagingStacks(context.Background(), all)
	if err == nil {
		t.Fatalf("ManagingStacks() = %v, nil; want an error, which a caller must not read as every container being unmanaged", got)
	}
}

func TestManagingStacks_ContainersOfNoProjectNeedNoStackRead(t *testing.T) {
	r := newStackRig(t)
	r.svc.Store = listFailingStackStore{r.store}
	r.addContainer("", "portainer")
	all, _ := r.fake.List(context.Background())

	got, err := r.svc.ManagingStacks(context.Background(), all)
	if err != nil || len(got) != 0 {
		t.Fatalf("ManagingStacks() = %v, %v; want nothing managed and no error", got, err)
	}
}
