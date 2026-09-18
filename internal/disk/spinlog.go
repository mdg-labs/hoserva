package disk

import (
	"sync"
	"time"
)

// SpinEvent is one recorded spin-state transition for a disk (Q32):
// timestamp plus the state before and after, so the wake-events view
// (doc 03 §3.3a) can show when a disk woke and how long it stayed awake.
type SpinEvent struct {
	Device string
	From   SpinState
	To     SpinState
	At     time.Time
}

// SpinEventLog is the record of every observed spin-state transition. It
// never queries a disk itself — every observation is fed in from a poll
// or action that already knows the disk's state, so recording a
// transition never risks waking the disk it describes (doc 02 §4, doc 08
// §1).
type SpinEventLog struct {
	mu     sync.Mutex
	last   map[string]SpinState
	events map[string][]SpinEvent
}

// NewSpinEventLog returns an empty SpinEventLog.
func NewSpinEventLog() *SpinEventLog {
	return &SpinEventLog{
		last:   make(map[string]SpinState),
		events: make(map[string][]SpinEvent),
	}
}

// Observe records a transition if dev's spin state changed since the
// last observation, and reports whether it did. The first observation
// for a device is never recorded as a transition — there is no prior
// state to transition from.
func (l *SpinEventLog) Observe(dev string, state SpinState, at time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	prev, known := l.last[dev]
	l.last[dev] = state
	if !known || prev == state {
		return false
	}

	l.events[dev] = append(l.events[dev], SpinEvent{Device: dev, From: prev, To: state, At: at})
	return true
}

// Events returns dev's recorded transitions, oldest first.
func (l *SpinEventLog) Events(dev string) []SpinEvent {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]SpinEvent, len(l.events[dev]))
	copy(out, l.events[dev])
	return out
}

// LastKnown reports the most recently observed spin state for dev.
func (l *SpinEventLog) LastKnown(dev string) (SpinState, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.last[dev]
	return s, ok
}
