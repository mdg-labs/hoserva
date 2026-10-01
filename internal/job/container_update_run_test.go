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
		"check":                   {`{"mode":"check"}`, false},
		"check, jitter":           {`{"mode":"check","jitterSeconds":90}`, false},
		"update":                  {`{"mode":"update","containers":["jellyfin","nginx"]}`, false},
		"revert":                  {`{"mode":"revert","containers":["jellyfin"],"sharers":["transcoder"]}`, false},
		"no params":               {``, true},
		"empty mode":              {`{"mode":""}`, true},
		"unknown mode":            {`{"mode":"rollback"}`, true},
		"negative":                {`{"mode":"check","jitterSeconds":-1}`, true},
		"unknown field":           {`{"mode":"check","all":true}`, true},
		"trailing value":          {`{"mode":"check"}{"mode":"check"}`, true},
		"update names none":       {`{"mode":"update"}`, true},
		"update empty name":       {`{"mode":"update","containers":[""]}`, true},
		"update names one twice":  {`{"mode":"update","containers":["a","a"]}`, true},
		"update with sharers":     {`{"mode":"update","containers":["a"],"sharers":["b"]}`, true},
		"update with jitter":      {`{"mode":"update","containers":["a"],"jitterSeconds":5}`, true},
		"revert names none":       {`{"mode":"revert"}`, true},
		"revert names two":        {`{"mode":"revert","containers":["a","b"]}`, true},
		"revert sharer is itself": {`{"mode":"revert","containers":["a"],"sharers":["a"]}`, true},
		"check names containers":  {`{"mode":"check","containers":["a"]}`, true},
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

func TestRunContainerUpdate_UpdatesEachContainerInOrderAndFailsNamingEveryOneThatFailed(t *testing.T) {
	s := newTestScheduler(t)
	var order []string
	s.registry.Register(TypeContainerUpdate, true, RunContainerUpdate(ContainerUpdateDeps{
		Update: func(_ context.Context, name string, _ io.Writer) error {
			order = append(order, name)
			if name == "postgres" || name == "redis" {
				return errors.New("the pre-update snapshot failed")
			}
			return nil
		},
	}))
	j, err := s.Submit(context.Background(), TypeContainerUpdate, []string{"container:jellyfin", "container:postgres", "container:nginx", "container:redis"}, []byte(`{"mode":"update","containers":["jellyfin","postgres","nginx","redis"]}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if strings.Join(order, ",") != "jellyfin,postgres,nginx,redis" {
		t.Fatalf("updated %v, want every container tried in order even after one failed", order)
	}
	for _, want := range []string{"2 of 4 containers were not updated", "postgres: the pre-update snapshot failed", "redis: the pre-update snapshot failed"} {
		if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, want) {
			t.Fatalf("status %s, error %q; want a failed job saying %q", done.Status, done.ErrorMessage, want)
		}
	}
	if strings.Contains(done.ErrorMessage, "jellyfin") || strings.Contains(done.ErrorMessage, "nginx") {
		t.Fatalf("error %q blames a container that was updated", done.ErrorMessage)
	}
}

func TestRunContainerUpdate_AllUpdatedSucceeds(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeContainerUpdate, true, RunContainerUpdate(ContainerUpdateDeps{
		Update: func(context.Context, string, io.Writer) error { return nil },
	}))
	j, err := s.Submit(context.Background(), TypeContainerUpdate, nil, []byte(`{"mode":"update","containers":["jellyfin"]}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if done := await(t, s, j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status %s, error %q; want succeeded", done.Status, done.ErrorMessage)
	}
}

func TestRunContainerUpdate_RevertGetsItsSharersAndFailsWithTheReason(t *testing.T) {
	s := newTestScheduler(t)
	var gotName string
	var gotSharers []string
	s.registry.Register(TypeContainerUpdate, true, RunContainerUpdate(ContainerUpdateDeps{
		Revert: func(_ context.Context, name string, sharers []string, _ io.Writer) error {
			gotName, gotSharers = name, sharers
			return errors.New("the update can no longer be reverted")
		},
	}))
	j, err := s.Submit(context.Background(), TypeContainerUpdate, nil, []byte(`{"mode":"revert","containers":["jellyfin"],"sharers":["transcoder"]}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if gotName != "jellyfin" || strings.Join(gotSharers, ",") != "transcoder" {
		t.Fatalf("Revert(%q, %v), want jellyfin with its sharers", gotName, gotSharers)
	}
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "jellyfin: the update can no longer be reverted") {
		t.Fatalf("status %s, error %q; want failed with the container and the reason", done.Status, done.ErrorMessage)
	}
}

// A cancel between two containers stops the rest and fails the job; the
// container being updated at that moment is not interrupted.
func TestRunContainerUpdate_ACancelStopsTheRemainingContainersButNeverInterruptsOne(t *testing.T) {
	s := newTestScheduler(t)
	started, release := make(chan struct{}), make(chan struct{})
	var updated []string
	var duringUpdate error
	s.registry.Register(TypeContainerUpdate, true, RunContainerUpdate(ContainerUpdateDeps{
		Update: func(ctx context.Context, name string, _ io.Writer) error {
			close(started)
			<-release
			duringUpdate = ctx.Err()
			updated = append(updated, name)
			return nil
		},
	}))
	j, err := s.Submit(context.Background(), TypeContainerUpdate, nil, []byte(`{"mode":"update","containers":["jellyfin","nginx"]}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started
	if _, err := s.Cancel(context.Background(), j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(release)
	done := await(t, s, j.ID)
	if duringUpdate != nil || strings.Join(updated, ",") != "jellyfin" {
		t.Fatalf("update context error %v, updated %v; want jellyfin finished under a context the cancel did not reach, and nginx left alone", duringUpdate, updated)
	}
	if done.Status == StatusSucceeded {
		t.Fatalf("status %s; a job cancelled before its last container is not a success", done.Status)
	}
}

func TestRunContainerUpdate_UpdateAndRevertWithoutWiringFail(t *testing.T) {
	for mode, body := range map[string]string{
		"update": `{"mode":"update","containers":["a"]}`,
		"revert": `{"mode":"revert","containers":["a"]}`,
	} {
		s := newTestScheduler(t)
		s.registry.Register(TypeContainerUpdate, true, RunContainerUpdate(ContainerUpdateDeps{}))
		j, err := s.Submit(context.Background(), TypeContainerUpdate, nil, []byte(body))
		if err != nil {
			t.Fatalf("%s: Submit: %v", mode, err)
		}
		if done := await(t, s, j.ID); done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "not configured") {
			t.Fatalf("%s: status %s, error %q; want failed as not configured", mode, done.Status, done.ErrorMessage)
		}
	}
}
