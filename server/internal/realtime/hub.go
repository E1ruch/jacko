// Package realtime routes live events to local channel subscribers.
package realtime

import "sync"

type Hub struct {
	mu        sync.Mutex
	available bool
	channels  map[int64]map[*subscriber]struct{}
}

type Subscription struct {
	Events <-chan Frame
	Done   <-chan struct{}
}

type subscriber struct {
	events chan Frame
	done   chan struct{}
}

func NewHub() *Hub {
	return &Hub{channels: make(map[int64]map[*subscriber]struct{})}
}

// Disconnect closes existing subscriptions so clients can replay missed events.
func (h *Hub) Disconnect() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.available = false
	for _, subscribers := range h.channels {
		for subscriber := range subscribers {
			close(subscriber.done)
		}
	}
	clear(h.channels)
}

func (h *Hub) Connect() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.available = true
}

func (h *Hub) Subscribe(channelID int64) (Subscription, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.available {
		return Subscription{}, nil, false
	}

	listener := &subscriber{events: make(chan Frame, 64), done: make(chan struct{})}
	if h.channels[channelID] == nil {
		h.channels[channelID] = make(map[*subscriber]struct{})
	}
	h.channels[channelID][listener] = struct{}{}
	return Subscription{Events: listener.events, Done: listener.done}, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.remove(channelID, listener)
	}, true
}

// Deliver shares immutable encoded bytes across all local subscribers.
func (h *Hub) Deliver(channelID int64, event Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for subscriber := range h.channels[channelID] {
		select {
		case subscriber.events <- event:
		default:
			h.remove(channelID, subscriber)
		}
	}
}

func (h *Hub) remove(channelID int64, subscriber *subscriber) {
	subscribers := h.channels[channelID]
	if _, exists := subscribers[subscriber]; exists {
		delete(subscribers, subscriber)
		close(subscriber.done)
	}
	if len(subscribers) == 0 {
		delete(h.channels, channelID)
	}
}
