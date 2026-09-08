package sim

import "sync"

// busMessage is the envelope every WebSocket push uses — a "type" tag plus
// whichever payload field is relevant, so the frontend's single onmessage
// handler can dispatch by type instead of needing one WS message shape per
// kind of event.
type busMessage struct {
	Type string        `json:"type"`
	Seed *SeedProgress `json:"seed,omitempty"`
}

// EventBus is the in-process pub/sub layer: background pollers publish here
// (stage S2+), and every WebSocket handler subscribes independently. This is
// a convenience channel only — a client that misses a message always
// recovers via GET /api/state, never by relying on every individual push
// having arrived. Slow consumers are DROPPED, not blocked: a stalled browser
// must never back-pressure the publisher.
type EventBus struct {
	mu   sync.RWMutex
	subs map[int]chan []byte
	next int
}

func NewEventBus() *EventBus {
	return &EventBus{subs: map[int]chan []byte{}}
}

// Subscribe registers a new subscriber and returns its id (for Unsubscribe)
// and a buffered channel of raw JSON event payloads.
func (b *EventBus) Subscribe() (int, <-chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan []byte, 256)
	b.subs[id] = ch
	return id, ch
}

func (b *EventBus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// Publish fans msg out to every current subscriber. Non-blocking: a
// subscriber whose channel is full simply misses this message.
func (b *EventBus) Publish(msg []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}
