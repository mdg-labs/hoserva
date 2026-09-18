package parity

import (
	"context"
	"errors"
	"sync"
)

// errFakeStreamClosed is FakeStream's Next error once Close has run and
// its queue is drained.
var errFakeStreamClosed = errors.New("parity: journal: fake stream closed")

// FakeWatcher is a scriptable Watcher for tests: each mountpoint gets its
// own manually fed FakeStream instead of a real fanotify mark, so
// Journal and everything above it are tested without CAP_SYS_ADMIN
// (CLAUDE.md).
type FakeWatcher struct {
	mu      sync.Mutex
	streams map[string]*FakeStream
}

// NewFakeWatcher returns an empty FakeWatcher.
func NewFakeWatcher() *FakeWatcher {
	return &FakeWatcher{streams: make(map[string]*FakeStream)}
}

var _ Watcher = (*FakeWatcher)(nil)

// Watch creates a fresh FakeStream for mountpoint — replacing any
// previous one, the way a real remount replaces the filesystem instance
// a prior mark was scoped to — and returns it. A test drives it with
// (*FakeWatcher).Stream(mountpoint).
func (w *FakeWatcher) Watch(ctx context.Context, mountpoint string) (Stream, error) {
	s := newFakeStream()

	w.mu.Lock()
	w.streams[mountpoint] = s
	w.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return s, nil
}

// Stream returns the live FakeStream for mountpoint, or nil if Watch has
// not (yet, or again since a later Watch call) been made for it.
func (w *FakeWatcher) Stream(mountpoint string) *FakeStream {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.streams[mountpoint]
}

// FakeStream is a manually driven Stream: a test calls Push to enqueue
// one batch of events exactly as one real fanotify read(2) would deliver
// them, and Next blocks until a batch is available — never polling,
// mirroring the real Stream's push-only contract.
type FakeStream struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  [][]StreamEvent
	closed bool
}

func newFakeStream() *FakeStream {
	s := &FakeStream{}
	s.cond = sync.NewCond(&s.mu)
	return s
}

var _ Stream = (*FakeStream)(nil)

// Push enqueues one batch of events for a subsequent Next to return.
func (s *FakeStream) Push(batch []StreamEvent) {
	s.mu.Lock()
	s.queue = append(s.queue, batch)
	s.mu.Unlock()
	s.cond.Broadcast()
}

// Next blocks until Push has queued a batch or Close has run.
func (s *FakeStream) Next() ([]StreamEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.queue) == 0 && !s.closed {
		s.cond.Wait()
	}
	if len(s.queue) == 0 {
		return nil, errFakeStreamClosed
	}
	batch := s.queue[0]
	s.queue = s.queue[1:]
	return batch, nil
}

// Close stops the stream; any Next call blocked waiting for a batch
// returns errFakeStreamClosed once woken. Idempotent.
func (s *FakeStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cond.Broadcast()
	return nil
}
