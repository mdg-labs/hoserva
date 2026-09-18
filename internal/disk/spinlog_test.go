package disk

import (
	"testing"
	"time"
)

func TestSpinEventLog_FirstObservationIsNotATransition(t *testing.T) {
	l := NewSpinEventLog()
	if changed := l.Observe("/dev/sdb", Active, time.Now()); changed {
		t.Fatal("Observe: first observation reported a transition")
	}
	if got := l.Events("/dev/sdb"); len(got) != 0 {
		t.Fatalf("Events: got %+v, want none", got)
	}
}

func TestSpinEventLog_RecordsTransition(t *testing.T) {
	l := NewSpinEventLog()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)

	l.Observe("/dev/sdb", Active, t0)
	if changed := l.Observe("/dev/sdb", Standby, t1); !changed {
		t.Fatal("Observe: Active -> Standby was not reported as a transition")
	}

	events := l.Events("/dev/sdb")
	if len(events) != 1 {
		t.Fatalf("Events: got %d, want 1: %+v", len(events), events)
	}
	want := SpinEvent{Device: "/dev/sdb", From: Active, To: Standby, At: t1}
	if events[0] != want {
		t.Fatalf("Events[0]: got %+v, want %+v", events[0], want)
	}
}

func TestSpinEventLog_NoChangeNotRecorded(t *testing.T) {
	l := NewSpinEventLog()
	l.Observe("/dev/sdb", Active, time.Now())
	if changed := l.Observe("/dev/sdb", Active, time.Now()); changed {
		t.Fatal("Observe: repeating the same state was reported as a transition")
	}
	if got := l.Events("/dev/sdb"); len(got) != 0 {
		t.Fatalf("Events: got %+v, want none", got)
	}
}

func TestSpinEventLog_TracksDevicesIndependently(t *testing.T) {
	l := NewSpinEventLog()
	l.Observe("/dev/sdb", Active, time.Now())
	l.Observe("/dev/sdc", Active, time.Now())
	l.Observe("/dev/sdb", Standby, time.Now())

	if got := l.Events("/dev/sdb"); len(got) != 1 {
		t.Fatalf("Events(/dev/sdb): got %d, want 1", len(got))
	}
	if got := l.Events("/dev/sdc"); len(got) != 0 {
		t.Fatalf("Events(/dev/sdc): got %d, want 0", len(got))
	}
}

func TestSpinEventLog_LastKnown(t *testing.T) {
	l := NewSpinEventLog()
	if _, ok := l.LastKnown("/dev/sdb"); ok {
		t.Fatal("LastKnown: reported a state for an unobserved disk")
	}
	l.Observe("/dev/sdb", Standby, time.Now())
	got, ok := l.LastKnown("/dev/sdb")
	if !ok || got != Standby {
		t.Fatalf("LastKnown: got (%v, %v), want (Standby, true)", got, ok)
	}
}
