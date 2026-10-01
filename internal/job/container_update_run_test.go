package job

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestContainerUpdateParams_Validation(t *testing.T) {
	for name, tc := range map[string]struct {
		params  string
		wantErr bool
	}{
		"check":          {`{"mode":"check"}`, false},
		"check, jitter":  {`{"mode":"check","jitterSeconds":90}`, false},
		"update":         {`{"mode":"update"}`, false},
		"no params":      {``, true},
		"empty mode":     {`{"mode":""}`, true},
		"unknown mode":   {`{"mode":"revert"}`, true},
		"negative":       {`{"mode":"check","jitterSeconds":-1}`, true},
		"unknown field":  {`{"mode":"check","all":true}`, true},
		"trailing value": {`{"mode":"check"}{"mode":"check"}`, true},
	} {
		err := ValidateParams(TypeContainerUpdate, []byte(tc.params))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: ValidateParams error = %v, wantErr %v", name, err, tc.wantErr)
		}
	}
}

func TestContainerUpdate_IsAServiceClassJob(t *testing.T) {
	if class, ok := ClassOf(TypeContainerUpdate); !ok || class != ClassService {
		t.Fatalf("ClassOf(container_update) = %q, %v; want the service class", class, ok)
	}
}

func TestRunContainerUpdate_CheckWaitsTheJitterThenChecks(t *testing.T) {
	s := newTestScheduler(t)
	var order []string
	s.registry.Register(TypeContainerUpdate, true, RunContainerUpdate(ContainerUpdateDeps{
		Check: func(_ context.Context, out io.Writer) error {
			order = append(order, "check")
			_, _ = io.WriteString(out, "checked 3 image(s)\n")
			return nil
		},
		Wait: func(_ context.Context, d time.Duration) error {
			order = append(order, "wait "+d.String())
			return nil
		},
	}))
	j, err := s.Submit(context.Background(), TypeContainerUpdate, []string{"container_update_check"}, []byte(`{"mode":"check","jitterSeconds":90}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusSucceeded || strings.Join(order, ",") != "wait 1m30s,check" {
		t.Fatalf("status %s, order %v; want succeeded, waiting the jitter before the first registry request", done.Status, order)
	}
}

func TestRunContainerUpdate_ACancelDuringTheJitterStopsBeforeAnyRequest(t *testing.T) {
	s := newTestScheduler(t)
	checked := false
	s.registry.Register(TypeContainerUpdate, true, RunContainerUpdate(ContainerUpdateDeps{
		Check: func(context.Context, io.Writer) error { checked = true; return nil },
		Wait:  func(ctx context.Context, _ time.Duration) error { return context.Canceled },
	}))
	j, err := s.Submit(context.Background(), TypeContainerUpdate, nil, []byte(`{"mode":"check","jitterSeconds":60}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status == StatusSucceeded || checked {
		t.Fatalf("status %s, checked %v; want no registry request and no success", done.Status, checked)
	}
}

func TestRunContainerUpdate_CheckFailureFailsTheJob(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeContainerUpdate, true, RunContainerUpdate(ContainerUpdateDeps{
		Check: func(context.Context, io.Writer) error {
			return errors.New("listing containers: docker engine is not reachable")
		},
	}))
	j, err := s.Submit(context.Background(), TypeContainerUpdate, nil, []byte(`{"mode":"check"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "not reachable") {
		t.Fatalf("status %s, error %q; want failed with the reason", done.Status, done.ErrorMessage)
	}
}

func TestRunContainerUpdate_UpdateModeRefusesAndNeverSucceeds(t *testing.T) {
	s := newTestScheduler(t)
	checked := false
	s.registry.Register(TypeContainerUpdate, true, RunContainerUpdate(ContainerUpdateDeps{
		Check: func(context.Context, io.Writer) error { checked = true; return nil },
	}))
	j, err := s.Submit(context.Background(), TypeContainerUpdate, nil, []byte(`{"mode":"update"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "not implemented") || checked {
		t.Fatalf("status %s, error %q, checked %v; want failed as not implemented, having updated nothing", done.Status, done.ErrorMessage, checked)
	}
}
