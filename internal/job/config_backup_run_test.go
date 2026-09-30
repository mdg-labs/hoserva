package job

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestConfigBackupJob_IsServiceClassNotResumableAndTakesNoParams(t *testing.T) {
	class, ok := ClassOf(TypeConfigBackup)
	if !ok || class != ClassService {
		t.Fatalf("ClassOf = %q, %v; want the service class", class, ok)
	}
	if Resumable(TypeConfigBackup) {
		t.Fatal("a config backup is re-run, never resumed")
	}
	if err := ValidateParams(TypeConfigBackup, nil); err != nil {
		t.Fatalf("no params: %v", err)
	}
	if err := ValidateParams(TypeConfigBackup, []byte(`{"destination":"x"}`)); err == nil {
		t.Fatal("a config backup accepted params")
	}
}

func TestRunConfigBackup_ASuccessfulRunSucceedsAndRaisesNoAlert(t *testing.T) {
	s := newTestScheduler(t)
	alerted := false
	s.registry.Register(TypeConfigBackup, true, RunConfigBackup(ConfigBackupDeps{
		Backup: func(_ context.Context, out io.Writer) error {
			_, _ = io.WriteString(out, "wrote an archive\n")
			return nil
		},
		Failed: func(context.Context, error) { alerted = true },
	}))
	j, err := s.Submit(context.Background(), TypeConfigBackup, []string{"backup:config"}, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if done := await(t, s, j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status = %s %q, want succeeded", done.Status, done.ErrorMessage)
	}
	if alerted {
		t.Fatal("a successful config backup alerted")
	}
}

func TestRunConfigBackup_AFailureEndsTheJobFailedWithTheErrorAndAlertsOnce(t *testing.T) {
	s := newTestScheduler(t)
	var alerts []error
	s.registry.Register(TypeConfigBackup, true, RunConfigBackup(ConfigBackupDeps{
		Backup: func(context.Context, io.Writer) error {
			return errors.New("writing destination \"boot\": no space left")
		},
		Failed: func(_ context.Context, err error) { alerts = append(alerts, err) },
	}))
	j, err := s.Submit(context.Background(), TypeConfigBackup, []string{"backup:config"}, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "no space left") {
		t.Fatalf("job = %s %q, want failed with the error", done.Status, done.ErrorMessage)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0].Error(), "no space left") {
		t.Fatalf("alerts = %v, want one carrying the error", alerts)
	}
}

func TestRunConfigBackup_ACancelledBackupDoesNotAlert(t *testing.T) {
	s := newTestScheduler(t)
	started := make(chan struct{})
	var mu sync.Mutex
	alerted := false
	s.registry.Register(TypeConfigBackup, true, RunConfigBackup(ConfigBackupDeps{
		Backup: func(ctx context.Context, _ io.Writer) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		Failed: func(context.Context, error) {
			mu.Lock()
			alerted = true
			mu.Unlock()
		},
	}))
	j, err := s.Submit(context.Background(), TypeConfigBackup, []string{"backup:config"}, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started
	if _, err := s.Cancel(context.Background(), j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if done := await(t, s, j.ID); done.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", done.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if alerted {
		t.Fatal("a cancelled config backup alerted")
	}
}

func TestConfigBackupJobs_QueueBehindEachOtherButNotBehindOtherServiceJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)

	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	var once sync.Once
	s.registry.Register(TypeConfigBackup, false, RunConfigBackup(ConfigBackupDeps{
		Backup: func(context.Context, io.Writer) error {
			once.Do(func() { close(firstStarted) })
			<-firstRelease
			return nil
		},
	}))
	first, err := s.Submit(ctx, TypeConfigBackup, []string{"backup:config"}, nil)
	if err != nil {
		t.Fatalf("Submit first: %v", err)
	}
	<-firstStarted

	second, err := s.Submit(ctx, TypeConfigBackup, []string{"backup:config"}, nil)
	if err != nil {
		t.Fatalf("a second config backup was refused instead of queued: %v", err)
	}
	if second.Status != StatusQueued {
		t.Fatalf("second config backup = %s, want queued behind the first", second.Status)
	}

	otherStarted, otherRelease := registerBlocking(s, TypeAppdataBackup, false)
	other, err := s.Submit(ctx, TypeAppdataBackup, []string{"radarr"}, nil)
	if err != nil {
		t.Fatalf("Submit appdata backup: %v", err)
	}
	if other.Status != StatusRunning {
		t.Fatalf("an appdata backup behind a config backup = %s, want it running alongside", other.Status)
	}
	<-otherStarted
	close(otherRelease)
	waitSucceeded(t, s, other.ID)

	close(firstRelease)
	waitSucceeded(t, s, first.ID)
	waitSucceeded(t, s, second.ID)
}
