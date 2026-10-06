package sim

import (
	"encoding/json"
	"sync"
)

// Event is one thing that happened on the desk, pushed to every open dashboard.
type Event struct {
	Type string `json:"type"` // ticket | update | incident | metrics | status
	Data any    `json:"data"`
}

// Bus fans events out to Server-Sent-Events subscribers. A slow browser tab gets
// events dropped rather than holding up the simulation — the dashboard re-reads
// the full state on reconnect, so a missed card is cosmetic.
type Bus struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{subs: map[chan []byte]struct{}{}} }

// Subscribe returns a channel of encoded events and a function to stop.
func (b *Bus) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Publish sends an event to every subscriber that has room for it.
func (b *Bus) Publish(typ string, data any) {
	msg, err := json.Marshal(Event{Type: typ, Data: data})
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}
