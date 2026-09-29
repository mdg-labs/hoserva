package container

import "sync"

// Hub fans container state changes out to every current subscriber (doc
// 01 §5's container_state events). internal/api owns translating a
// StateChange into events.Event and the wire format; this package only
// broadcasts its own domain type, like job.Hub and notify.Hub.
type Hub struct {
	mu   sync.Mutex
	subs map[chan StateChange]struct{}
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[chan StateChange]struct{})}
}

// Subscribe returns a channel that receives every StateChange Publish
// sends from this point on, and an unsubscribe function the caller must
// call exactly once.
func (h *Hub) Subscribe() (<-chan StateChange, func()) {
	ch := make(chan StateChange, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Publish sends sc to every current subscriber, dropping it for any
// subscriber whose channel is full rather than blocking. A nil Hub
// discards it.
func (h *Hub) Publish(sc StateChange) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- sc:
		default:
		}
	}
}
