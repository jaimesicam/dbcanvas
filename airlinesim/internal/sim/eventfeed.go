package sim

import (
	"context"
	"encoding/json"
	"log"
	"time"
)

// runEventFeed polls reservation_events by auto-increment id cursor and republishes
// each new row on the in-process EventBus for the live activity feed. MySQL has no
// change-stream equivalent — this poller IS the mechanism, not a fallback (it's the
// same technique Hotel Sim only falls back to on a standalone MongoDB node).
func (e *Engine) runEventFeed(ctx context.Context) {
	cursor, err := e.Store.MaxEventID(ctx)
	if err != nil {
		log.Printf("airlinesim: event feed: initial cursor: %v", err)
	}
	tickLoop(ctx, 500*time.Millisecond, func() {
		evs, err := e.Store.EventsSince(ctx, cursor, 200)
		if err != nil {
			return
		}
		for _, ev := range evs {
			cursor = ev.ID
			if payload, mErr := json.Marshal(ev); mErr == nil {
				e.Bus.Publish(payload)
			}
		}
	})
}

// runClockPersister checkpoints the simulated clock every 10s so a restart resumes
// from here rather than a stale or zero value.
func (e *Engine) runClockPersister(ctx context.Context) {
	tickLoop(ctx, 10*time.Second, func() {
		e.persistClock(ctx)
	})
}
