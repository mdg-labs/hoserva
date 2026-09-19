package notify

import "sync"

// Hub fans every Alert Service.Publish persists out to current SSE
// subscribers (doc 01 §5's notification events). internal/api owns
// translating an Alert into events.Event and writing the wire format;
// this package only broadcasts its own domain type.
type Hub struct {
	mu   sync.Mutex
	subs map[chan *Alert]struct{}
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[chan *Alert]struct{})}
}

// Subscribe returns a channel that receives every Alert Publish sends
// from this point on, and an unsubscribe function the caller must call
// exactly once.
func (h *Hub) Subscribe() (<-chan *Alert, func()) {
	ch := make(chan *Alert, 16)
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

// Publish sends a to every current subscriber, dropping the update for
// any subscriber whose channel is full rather than blocking.
func (h *Hub) Publish(a *Alert) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- a:
		default:
		}
	}
}
