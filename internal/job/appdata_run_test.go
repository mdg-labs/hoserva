package job

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppdataJobs_AreServiceClassAndNotResumable(t *testing.T) {
	for _, typ := range []Type{TypeAppdataBackup, TypeAppdataRestore, TypeAppdataRestorePreview} {
		class, ok := ClassOf(typ)
		if !ok || class != ClassService {
			t.Errorf("ClassOf(%s) = %q, %v; want the service class", typ, class, ok)
		}
		if Resumable(typ) {
			t.Errorf("%s must not be resumable: a resumed run would act on containers stopped by the one that was interrupted", typ)
		}
	}
}

func TestAppdataParams_Validation(t *testing.T) {
	for name, tc := range map[string]struct {
		typ     Type
		params  string
		wantErr bool
	}{
		"backup, no params":            {TypeAppdataBackup, ``, false},
		"backup, every container":      {TypeAppdataBackup, `{}`, false},
		"backup, named containers":     {TypeAppdataBackup, `{"containers":["a","b"]}`, false},
		"backup, an empty name":        {TypeAppdataBackup, `{"containers":[""]}`, true},
		"backup, unknown field":        {TypeAppdataBackup, `{"stop":false}`, true},
		"restore":                      {TypeAppdataRestore, `{"container":"a","archive":"x.tar.zst","destinationId":"pool"}`, false},
		"restore, no params":           {TypeAppdataRestore, ``, true},
		"restore, no archive":          {TypeAppdataRestore, `{"container":"a","destinationId":"pool"}`, true},
		"restore, no destination":      {TypeAppdataRestore, `{"container":"a","archive":"x"}`, true},
		"restore, no container":        {TypeAppdataRestore, `{"archive":"x","destinationId":"pool"}`, true},
		"restore, unknown field":       {TypeAppdataRestore, `{"container":"a","archive":"x","destinationId":"p","confirm":true}`, true},
		"restore, two objects":         {TypeAppdataRestore, `{"container":"a","archive":"x","destinationId":"p"}{}`, true},
		"preview":                      {TypeAppdataRestorePreview, `{"container":"a","archive":"x.tar.zst","destinationId":"pool"}`, false},
		"preview, no params":           {TypeAppdataRestorePreview, ``, true},
		"preview, no archive":          {TypeAppdataRestorePreview, `{"container":"a","destinationId":"pool"}`, true},
		"preview, unknown field":       {TypeAppdataRestorePreview, `{"container":"a","archive":"x","destinationId":"p","confirm":true}`, true},
		"backup, two objects":          {TypeAppdataBackup, `{}{}`, true},
		"recreate still needs its own": {TypeContainerRecreate, `{"containers":["a"]}`, true},
	} {
		err := ValidateParams(tc.typ, []byte(tc.params))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: ValidateParams error = %v, wantErr %v", name, err, tc.wantErr)
		}
	}
}

func TestRunAppdataBackup_PassesTheNamedContainersAndWritesToTheJobLog(t *testing.T) {
	s := newTestScheduler(t)
	var got []string
	s.registry.Register(TypeAppdataBackup, true, RunAppdataBackup(AppdataBackupDeps{
		Backup: func(_ context.Context, containers []string, out io.Writer) error {
			got = containers
			_, _ = io.WriteString(out, "archiving alpha\n")
			return nil
		},
	}))

	j, err := s.Submit(context.Background(), TypeAppdataBackup, []string{"container:alpha"}, []byte(`{"containers":["alpha"]}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusSucceeded || !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("status %s, containers %v; want succeeded with [alpha]", done.Status, got)
	}
}

func TestRunAppdataBackup_FailureFailsTheJobAndIsReportedOnce(t *testing.T) {
	s := newTestScheduler(t)
	var mu sync.Mutex
	var failures []error
	s.registry.Register(TypeAppdataBackup, true, RunAppdataBackup(AppdataBackupDeps{
		Backup: func(context.Context, []string, io.Writer) error { return errors.New("destination pool: disk full") },
		Failed: func(_ context.Context, err error) {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, err)
		},
	}))

	j, err := s.Submit(context.Background(), TypeAppdataBackup, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "disk full") {
		t.Fatalf("job = %s %q, want failed with the reason", done.Status, done.ErrorMessage)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "disk full") {
		t.Fatalf("Failed called with %v, want once with the backup's error", failures)
	}
}

func TestRunAppdataBackup_ACancelledRunIsNotAFailureAlert(t *testing.T) {
	s := newTestScheduler(t)
	started := make(chan struct{})
	var alerted bool
	s.registry.Register(TypeAppdataBackup, true, RunAppdataBackup(AppdataBackupDeps{
		Backup: func(ctx context.Context, _ []string, _ io.Writer) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		Failed: func(context.Context, error) { alerted = true },
	}))

	j, err := s.Submit(context.Background(), TypeAppdataBackup, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started
	if _, err := s.Cancel(context.Background(), j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", done.Status)
	}
	if alerted {
		t.Fatal("a job the operator cancelled raised a backup-failed alert")
	}
}

func TestRunAppdataRestore_PassesTheRequestFromTheParams(t *testing.T) {
	s := newTestScheduler(t)
	var got AppdataRestoreParams
	s.registry.Register(TypeAppdataRestore, false, RunAppdataRestore(func(_ context.Context, p AppdataRestoreParams, _ io.Writer) error {
		got = p
		return nil
	}))

	j, err := s.Submit(context.Background(), TypeAppdataRestore, []string{"container:alpha"}, []byte(`{"container":"alpha","archive":"a.tar.zst","destinationId":"pool"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	want := AppdataRestoreParams{Container: "alpha", Archive: "a.tar.zst", DestinationID: "pool"}
	if done.Status != StatusSucceeded || got != want {
		t.Fatalf("status %s, restore %+v; want succeeded with %+v", done.Status, got, want)
	}
}

func TestRunAppdataRestorePreview_PassesTheRequestAndItsOwnJobID(t *testing.T) {
	s := newTestScheduler(t)
	var gotID string
	var got AppdataRestoreParams
	s.registry.Register(TypeAppdataRestorePreview, true, RunAppdataRestorePreview(func(_ context.Context, id string, p AppdataRestoreParams, _ io.Writer) error {
		gotID, got = id, p
		return nil
	}))

	j, err := s.Submit(context.Background(), TypeAppdataRestorePreview, []string{"container:alpha"}, []byte(`{"container":"alpha","archive":"a.tar.zst","destinationId":"pool"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	want := AppdataRestoreParams{Container: "alpha", Archive: "a.tar.zst", DestinationID: "pool"}
	if done.Status != StatusSucceeded || got != want || gotID != j.ID {
		t.Fatalf("status %s, preview of %+v under %q; want succeeded with %+v under %q", done.Status, got, gotID, want, j.ID)
	}
}

// The scheduler queues a preview behind a backup or restore of the same
// container and runs it beside those of others, which is what keeps a
// scheduled backup from being refused by a preview.
func TestAppdataRestorePreview_QueuesBehindTheSameContainersBackupAndRunsBesideOthers(t *testing.T) {
	s := newTestScheduler(t)
	release := make(chan struct{})
	started := make(chan string, 4)
	s.registry.Register(TypeAppdataBackup, true, func(_ context.Context, rc *RunContext) error {
		started <- "backup"
		<-release
		return nil
	})
	s.registry.Register(TypeAppdataRestorePreview, true, func(_ context.Context, rc *RunContext) error {
		started <- "preview"
		return nil
	})
	ctx := context.Background()
	params := []byte(`{"container":"alpha","archive":"a.tar.zst","destinationId":"pool"}`)

	backupJob, err := s.Submit(ctx, TypeAppdataBackup, []string{"container:alpha"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-started; got != "backup" {
		t.Fatalf("started %s first", got)
	}
	same, err := s.Submit(ctx, TypeAppdataRestorePreview, []string{"container:alpha"}, params)
	if err != nil {
		t.Fatal(err)
	}
	if same.Status != StatusQueued {
		t.Fatalf("a preview of the container being backed up is %s, want queued", same.Status)
	}
	other, err := s.Submit(ctx, TypeAppdataRestorePreview, []string{"container:beta"}, params)
	if err != nil {
		t.Fatal(err)
	}
	if got := await(t, s, other.ID); got.Status != StatusSucceeded {
		t.Fatalf("a preview of another container beside the backup = %s", got.Status)
	}
	close(release)
	if got := await(t, s, backupJob.ID); got.Status != StatusSucceeded {
		t.Fatalf("backup = %s", got.Status)
	}
	if got := await(t, s, same.ID); got.Status != StatusSucceeded {
		t.Fatalf("the queued preview = %s", got.Status)
	}
}

func TestDefaultOtherJobs_AppdataBackupIsWeeklyAndOn(t *testing.T) {
	for _, j := range DefaultOtherJobs() {
		if j.ID != "appdata_backup" {
			continue
		}
		if !j.Enabled || j.Frequency != FrequencyWeekly {
			t.Fatalf("appdata_backup default = %+v, want enabled and weekly (doc 10 §2)", j)
		}
		return
	}
	t.Fatal("no appdata_backup default")
}

func TestOtherJobIsDue(t *testing.T) {
	utc := time.UTC
	weekly := OtherJobSettings{ID: "appdata_backup", Enabled: true, Frequency: FrequencyWeekly, Time: "04:00"}
	sunday := time.Date(2026, 9, 27, 4, 0, 0, 0, utc)

	for name, tc := range map[string]struct {
		now   time.Time
		job   OtherJobSettings
		since time.Time
		want  bool
	}{
		"saved on Tuesday, next Sunday not reached": {sunday.AddDate(0, 0, 2), weekly, sunday.AddDate(0, 0, 2).Add(-time.Hour), false},
		"the Sunday window has opened":              {sunday.Add(time.Minute), weekly, sunday.AddDate(0, 0, -5), true},
		"already ran in that window":                {sunday.Add(time.Hour), weekly, sunday.Add(time.Minute), false},
		"a missed window is still due":              {sunday.AddDate(0, 0, 3), weekly, sunday.AddDate(0, 0, -7), true},
		"disabled":                                  {sunday.Add(time.Minute), OtherJobSettings{Enabled: false, Frequency: FrequencyWeekly, Time: "04:00"}, sunday.AddDate(0, 0, -7), false},
		"exactly at the window's start":             {sunday, weekly, sunday.AddDate(0, 0, -1), true},
	} {
		if got := OtherJobIsDue(tc.now, utc, tc.job, tc.since); got != tc.want {
			t.Errorf("%s: OtherJobIsDue = %v, want %v", name, got, tc.want)
		}
	}
}

func TestPrevOtherJobRun(t *testing.T) {
	utc := time.UTC
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, utc) }
	daily := func(clock string) OtherJobSettings { return OtherJobSettings{Frequency: FrequencyDaily, Time: clock} }
	weekly := OtherJobSettings{Frequency: FrequencyWeekly, Time: "04:00"}
	monthly := OtherJobSettings{Frequency: FrequencyMonthly, Time: "05:00"}
	// 2026-09-29 is a Tuesday and 2026-09-27 the Sunday before it.
	for name, tc := range map[string]struct {
		now  time.Time
		job  OtherJobSettings
		want time.Time
	}{
		"daily, today's time has passed":        {at(2026, 9, 29, 10, 30), daily("04:00"), at(2026, 9, 29, 4, 0)},
		"daily, today's time not yet":           {at(2026, 9, 29, 10, 30), daily("11:00"), at(2026, 9, 28, 11, 0)},
		"weekly, midweek":                       {at(2026, 9, 29, 10, 30), weekly, at(2026, 9, 27, 4, 0)},
		"weekly, on the Sunday before its time": {at(2026, 9, 27, 3, 0), weekly, at(2026, 9, 20, 4, 0)},
		"weekly, at the minute it opens":        {at(2026, 9, 27, 4, 0), weekly, at(2026, 9, 27, 4, 0)},
		"monthly, after the 1st":                {at(2026, 9, 29, 10, 30), monthly, at(2026, 9, 1, 5, 0)},
		"monthly, on the 1st before its time":   {at(2026, 10, 1, 4, 0), monthly, at(2026, 9, 1, 5, 0)},
	} {
		if got := PrevOtherJobRun(tc.now, utc, tc.job); !got.Equal(tc.want) {
			t.Errorf("%s: PrevOtherJobRun = %v, want %v", name, got, tc.want)
		}
	}
}
