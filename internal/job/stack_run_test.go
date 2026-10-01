package job

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStackStart_IsAServiceClassJobThatIsNotResumable(t *testing.T) {
	class, ok := ClassOf(TypeStackStart)
	if !ok || class != ClassService {
		t.Fatalf("ClassOf(stack_start) = %q, %v; want the service class", class, ok)
	}
	if Resumable(TypeStackStart) {
		t.Fatal("stack_start must not be resumable: a resumed start would act on a stack changed since")
	}
}

func TestStackStartParams_Validation(t *testing.T) {
	for name, tc := range map[string]struct {
		params  string
		wantErr bool
	}{
		"valid":          {`{"name":"jellyfin"}`, false},
		"no params":      {``, true},
		"empty name":     {`{"name":""}`, true},
		"unknown field":  {`{"name":"a","pull":true}`, true},
		"trailing value": {`{"name":"a"}{"name":"b"}`, true},
	} {
		err := ValidateParams(TypeStackStart, []byte(tc.params))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: ValidateParams error = %v, wantErr %v", name, err, tc.wantErr)
		}
	}
}

func TestRunStackStart_CallsUpWithTheStackFromTheParams(t *testing.T) {
	s := newTestScheduler(t)
	var got string
	s.registry.Register(TypeStackStart, false, RunStackStart(func(_ context.Context, name string) error {
		got = name
		return nil
	}))

	j, err := s.Submit(context.Background(), TypeStackStart, []string{"stack:jellyfin"}, []byte(`{"name":"jellyfin"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusSucceeded || got != "jellyfin" {
		t.Fatalf("status %s, started %q; want succeeded and jellyfin", done.Status, got)
	}
}

func TestRunStackStart_FailureFailsTheJobWithTheReason(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeStackStart, false, RunStackStart(func(context.Context, string) error {
		return errors.New("port is already allocated")
	}))

	j, err := s.Submit(context.Background(), TypeStackStart, []string{"stack:jellyfin"}, []byte(`{"name":"jellyfin"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "port is already allocated") {
		t.Fatalf("job = %s %q, want failed with the start error", done.Status, done.ErrorMessage)
	}
}
