package template

import "sync"

// CheckHub fans every finished catalog check out to the current subscribers
// of the events stream (doc 01 §5's catalog events). internal/api owns the
// wire format; this only broadcasts the domain result, like container.Hub.
type CheckHub struct {
	mu   sync.Mutex
	subs map[chan CheckResult]struct{}
}

// NewCheckHub returns an empty CheckHub.
func NewCheckHub() *CheckHub {
	return &CheckHub{subs: make(map[chan CheckResult]struct{})}
}

// Subscribe returns a channel that receives every result Publish sends from
// this point on, and an unsubscribe function the caller must call exactly
// once.
func (h *CheckHub) Subscribe() (<-chan CheckResult, func()) {
	ch := make(chan CheckResult, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Publish sends r to every current subscriber, dropping it for a subscriber
// whose channel is full rather than blocking. A nil CheckHub discards it.
func (h *CheckHub) Publish(r CheckResult) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- r:
		default:
		}
	}
}
