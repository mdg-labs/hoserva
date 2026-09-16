package job

import "sync"

// Hub fans a job's progress/state changes out to every current subscriber
// (doc 01 §5's `/api/v1/events`, job_progress events). It knows nothing
// about SSE framing or the generated api/gen/go/events types — internal/api
// owns translating a *Job into an events.Event and writing the wire
// format; this package only broadcasts its own domain type, so it stays
// usable from a test with no HTTP server at all.
type Hub struct {
	mu   sync.Mutex
	subs map[chan *Job]struct{}
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[chan *Job]struct{})}
}

// Subscribe returns a channel that receives every Job Publish sends from
// this point on, and an unsubscribe function the caller must call exactly
// once (typically deferred) to stop receiving and let the channel be
// garbage collected. The channel is buffered so one slow subscriber
// dropping a message never blocks Publish or another subscriber.
func (h *Hub) Subscribe() (<-chan *Job, func()) {
	ch := make(chan *Job, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
	return ch, unsubscribe
}

// Publish sends j to every current subscriber, dropping the update for
// any subscriber whose channel is full rather than blocking — a missed
// progress tick is harmless (the next GetJob or job_progress event carries
// the current state regardless); blocking the scheduler on a stalled SSE
// client is not.
func (h *Hub) Publish(j *Job) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- j:
		default:
		}
	}
}
