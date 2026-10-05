package webui

import (
	"sync"
)

// hub fans a snapshot out to every connected client.
//
// A monitoring page must never be able to block the agent, so publishing to a
// slow or dead client is dropped rather than waited for. Losing one update is
// harmless because every message is a complete snapshot: the next one is
// self-correcting, and a client that misses several still converges.
type hub struct {
	mu          sync.Mutex
	subscribers map[int]chan Snapshot
	next        int
}

func newHub() *hub {
	return &hub{subscribers: make(map[int]chan Snapshot)}
}

// subscribe returns a channel of snapshots and a function to unsubscribe.
func (h *hub) subscribe() (<-chan Snapshot, func()) {
	// A small buffer absorbs a burst of updates while the client is being
	// served, without letting a slow client grow an unbounded backlog.
	ch := make(chan Snapshot, 4)

	h.mu.Lock()
	id := h.next
	h.next++
	h.subscribers[id] = ch
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if c, ok := h.subscribers[id]; ok {
				delete(h.subscribers, id)
				close(c)
			}
		})
	}
}

// publish delivers a snapshot to every subscriber, dropping it for any that is
// not keeping up.
func (h *hub) publish(snap Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for id, ch := range h.subscribers {
		select {
		case ch <- snap:
		default:
			// The client is behind. Dropping is the right call: a monitoring
			// page that lags is fine, an agent that blocks on a browser is not.
			// The subscription stays open and will receive the next update.
			_ = id
		}
	}
}

// count returns the number of connected clients, for the metrics endpoint.
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}
