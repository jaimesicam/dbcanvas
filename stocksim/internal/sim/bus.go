package sim

import (
	"encoding/json"
	"sync"

	"stocksim/internal/store"
)

// EventBus is the in-process pub/sub layer: the event-feed poller publishes
// here, and every WebSocket handler subscribes independently. This is a
// convenience channel only — a client that misses a message always recovers
// via GET /api/state, never by relying on every individual push having
// arrived. Slow consumers are DROPPED, not blocked: a stalled browser must
// never back-pressure the poller loop.
type EventBus struct {
	mu   sync.RWMutex
	subs map[int]chan []byte
	next int
}

func NewEventBus() *EventBus {
	return &EventBus{subs: map[int]chan []byte{}}
}

// Subscribe registers a new subscriber and returns its id (for Unsubscribe)
// and a buffered channel of raw JSON payloads.
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

// Publish fans msg out to every current subscriber. Non-blocking: a subscriber
// whose channel is full simply misses this message.
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

// busMessage is the envelope every WebSocket push uses — a "type" tag plus
// whichever payload field is relevant, so the frontend's single onmessage
// handler can dispatch by type instead of needing one message shape per kind
// of event. Borrowed from MarketChaos, which needed the same thing once its
// dashboard grew past one panel.
type busMessage struct {
	Type   string        `json:"type"`
	Event  *store.Event  `json:"event,omitempty"`
	Quotes []QuoteUpdate `json:"quotes,omitempty"`
	Seed   *SeedProgress `json:"seed,omitempty"`
}

// QuoteUpdate is the compact price push the ticker grid animates from. Kept
// far smaller than a full Security so a busy market does not flood the socket.
type QuoteUpdate struct {
	SecurityID string  `json:"securityId"`
	Symbol     string  `json:"symbol"`
	Price      float64 `json:"price"`
	ChangePct  float64 `json:"changePct"`
}

// SeedProgress reports how far the background seed has got, so the dashboard
// can show real progress instead of an empty page. /healthz deliberately does
// not wait for seeding — see main.go.
type SeedProgress struct {
	Running bool   `json:"running"`
	Done    bool   `json:"done"`
	Step    string `json:"step"`
	Percent int    `json:"percent"`
	Error   string `json:"error,omitempty"`
}

func (b *EventBus) publishJSON(m busMessage) {
	if payload, err := json.Marshal(m); err == nil {
		b.Publish(payload)
	}
}
