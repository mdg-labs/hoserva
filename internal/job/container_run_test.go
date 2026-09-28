package job

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestContainerRecreate_IsAServiceClassJobThatIsNotResumable(t *testing.T) {
	class, ok := ClassOf(TypeContainerRecreate)
	if !ok || class != ClassService {
		t.Fatalf("ClassOf(container_recreate) = %q, %v; want the service class", class, ok)
	}
	if Resumable(TypeContainerRecreate) {
		t.Fatal("container_recreate must not be resumable: a resumed swap would act on a plan made before the interruption")
	}
}

func TestContainerRecreateParams_Validation(t *testing.T) {
	for name, tc := range map[string]struct {
		params  string
		wantErr bool
	}{
		"valid":          {`{"id":"jellyfin"}`, false},
		"no params":      {``, true},
		"empty id":       {`{"id":""}`, true},
		"unknown field":  {`{"id":"a","deleteAppdata":true}`, true},
		"trailing value": {`{"id":"a"}{"id":"b"}`, true},
	} {
		err := ValidateParams(TypeContainerRecreate, []byte(tc.params))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: ValidateParams error = %v, wantErr %v", name, err, tc.wantErr)
		}
	}
}

func TestRunContainerRecreate_CallsRecreateWithTheContainerFromTheParams(t *testing.T) {
	s := newTestScheduler(t)
	var got string
	s.registry.Register(TypeContainerRecreate, false, RunContainerRecreate(func(_ context.Context, id string) error {
		got = id
		return nil
	}))

	j, err := s.Submit(context.Background(), TypeContainerRecreate, []string{"container:jellyfin"}, []byte(`{"id":"jellyfin"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusSucceeded || got != "jellyfin" {
		t.Fatalf("status %s, recreated %q; want succeeded and jellyfin", done.Status, got)
	}
	if done.Cancellable {
		t.Fatal("a recreate registered as not cancellable reports cancellable")
	}
}

func TestRunContainerRecreate_FailureFailsTheJobWithTheReason(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeContainerRecreate, false, RunContainerRecreate(func(context.Context, string) error {
		return errors.New("the original container was restored: port is already allocated")
	}))

	j, err := s.Submit(context.Background(), TypeContainerRecreate, []string{"container:jellyfin"}, []byte(`{"id":"jellyfin"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "the original container was restored") {
		t.Fatalf("job = %s %q, want failed with the recreate error", done.Status, done.ErrorMessage)
	}
}
