package job

import (
	"context"
	"testing"
	"time"
)

func TestStore_CreateGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewStore(db)

	progress := 42
	want := &Job{
		ID:          "job-1",
		Type:        TypeSync,
		Class:       ClassParity,
		Status:      StatusRunning,
		Progress:    &progress,
		Resumable:   false,
		Cancellable: true,
		ResourceIDs: []string{"disk-1", "disk-2"},
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
	}
	if err := s.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "job-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != want.ID || got.Type != want.Type || got.Class != want.Class || got.Status != want.Status {
		t.Fatalf("Get roundtrip mismatch: got %+v, want %+v", got, want)
	}
	if got.Progress == nil || *got.Progress != progress {
		t.Fatalf("Progress = %v, want %d", got.Progress, progress)
	}
	if len(got.ResourceIDs) != 2 || got.ResourceIDs[0] != "disk-1" || got.ResourceIDs[1] != "disk-2" {
		t.Fatalf("ResourceIDs = %v, want [disk-1 disk-2]", got.ResourceIDs)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
}

func TestStore_GetNotFound(t *testing.T) {
	db := newTestDB(t)
	s := NewStore(db)
	if _, err := s.Get(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("Get(missing id) = nil error, want ErrNotFound")
	}
}

func TestStore_ListFiltersByClassAndStatus(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewStore(db)

	seed := []*Job{
		{ID: "a", Type: TypeSync, Class: ClassParity, Status: StatusRunning, CreatedAt: time.Now().UTC()},
		{ID: "b", Type: TypeMover, Class: ClassArrayWrite, Status: StatusQueued, CreatedAt: time.Now().UTC()},
		{ID: "c", Type: TypeScrub, Class: ClassParity, Status: StatusSucceeded, CreatedAt: time.Now().UTC()},
	}
	for _, j := range seed {
		if err := s.Create(ctx, j); err != nil {
			t.Fatalf("Create(%s): %v", j.ID, err)
		}
	}

	parity := ClassParity
	byClass, err := s.List(ctx, ListFilter{Class: &parity})
	if err != nil {
		t.Fatalf("List by class: %v", err)
	}
	if len(byClass) != 2 {
		t.Fatalf("List(class=parity) returned %d jobs, want 2", len(byClass))
	}

	running := StatusRunning
	byStatus, err := s.List(ctx, ListFilter{Status: &running})
	if err != nil {
		t.Fatalf("List by status: %v", err)
	}
	if len(byStatus) != 1 || byStatus[0].ID != "a" {
		t.Fatalf("List(status=running) = %+v, want just job a", byStatus)
	}
}

func TestStore_ListActiveAndInterruptActive(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewStore(db)

	seed := []*Job{
		{ID: "running", Type: TypeSync, Class: ClassParity, Status: StatusRunning, CreatedAt: time.Now().UTC()},
		{ID: "queued", Type: TypeMover, Class: ClassArrayWrite, Status: StatusQueued, CreatedAt: time.Now().UTC()},
		{ID: "done", Type: TypeScrub, Class: ClassParity, Status: StatusSucceeded, CreatedAt: time.Now().UTC()},
	}
	for _, j := range seed {
		if err := s.Create(ctx, j); err != nil {
			t.Fatalf("Create(%s): %v", j.ID, err)
		}
	}

	active, err := s.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("ListActive returned %d jobs, want 2 (running, queued)", len(active))
	}

	// Doc 01 §4: "marked interrupted after a restart, never resumed
	// automatically" — this is the exact call the daemon's startup path
	// makes, against a real SQLite database, not a fake.
	if err := s.InterruptActive(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("InterruptActive: %v", err)
	}

	stillActive, err := s.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive after interrupt: %v", err)
	}
	if len(stillActive) != 0 {
		t.Fatalf("ListActive after InterruptActive returned %d jobs, want 0", len(stillActive))
	}

	for _, id := range []string{"running", "queued"} {
		got, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != StatusInterrupted {
			t.Errorf("job %s status = %s, want interrupted", id, got.Status)
		}
		if got.FinishedAt == nil {
			t.Errorf("job %s FinishedAt is nil, want set", id)
		}
	}

	done, err := s.Get(ctx, "done")
	if err != nil {
		t.Fatalf("Get(done): %v", err)
	}
	if done.Status != StatusSucceeded {
		t.Errorf("already-succeeded job's status changed to %s, want unchanged succeeded", done.Status)
	}
}

func TestStore_UpdateProgressAndCheckpoint(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewStore(db)

	j := &Job{ID: "j1", Type: TypeMover, Class: ClassArrayWrite, Status: StatusRunning, CreatedAt: time.Now().UTC()}
	if err := s.Create(ctx, j); err != nil {
		t.Fatalf("Create: %v", err)
	}

	pct := 55
	if err := s.UpdateProgress(ctx, "j1", &pct); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	if err := s.SaveCheckpoint(ctx, "j1", []byte(`{"offset":123}`)); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	got, err := s.Get(ctx, "j1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Progress == nil || *got.Progress != 55 {
		t.Fatalf("Progress = %v, want 55", got.Progress)
	}
	if string(got.Checkpoint) != `{"offset":123}` {
		t.Fatalf("Checkpoint = %q, want the saved JSON", got.Checkpoint)
	}
}
