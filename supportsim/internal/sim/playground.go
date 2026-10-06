package sim

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// playground.go — a search index's life, watched.
//
// One index, "playground", on vector_variants. You create it, change its definition,
// break it and drop it, and while that happens two probe queries run against it every
// 700 ms: a plain $vectorSearch, and one that pre-filters on team. The timeline records
// every change in what mongot reports and what the probes get back, which shows the
// things the documentation states and nobody believes until they see them:
//
//   - a new index is PENDING, then BUILDING, then READY; until then queries fail;
//   - an update builds the new definition beside the old one and keeps answering from
//     the old until the new is ready — the filter probe starts working only then;
//   - a definition that does not match the data (wrong numDimensions) does not fail
//     loudly: the index builds, and the documents silently are not in it;
//   - a drop takes effect at once.

// PlaygroundAction is what a button does.
type PlaygroundAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Shell string `json:"shell"`
	Hint  string `json:"hint"`
	def   bson.D
	drop  bool
}

type PGEvent struct {
	T         float64 `json:"t"` // seconds since the action
	Action    string  `json:"action,omitempty"`
	Status    string  `json:"status"`
	Queryable bool    `json:"queryable"`
	Version   string  `json:"version"`
	Detail    string  `json:"detail,omitempty"`
	Plain     string  `json:"plain"`  // the plain probe's outcome
	Filter    string  `json:"filter"` // the filtered probe's outcome
}

type playground struct {
	started time.Time
	action  string
	events  []PGEvent
	last    string
	cancel  context.CancelFunc
}

// PlaygroundView is the panel's state.
type PlaygroundView struct {
	Actions []PlaygroundAction `json:"actions"`
	Action  string             `json:"action"`
	Events  []PGEvent          `json:"events"`
	Running bool               `json:"running"`
}

// Playground returns the timeline.
func (e *Engine) Playground() PlaygroundView {
	e.ws.mu.Lock()
	defer e.ws.mu.Unlock()
	v := PlaygroundView{Actions: e.B.Workshop().PlaygroundActions()}
	if p := e.ws.pg; p != nil {
		v.Action = p.action
		v.Events = append([]PGEvent(nil), p.events...)
		v.Running = p.cancel != nil
	}
	return v
}

// PlaygroundDo runs an action and starts watching.// PlaygroundDo runs an action and starts watching.
func (e *Engine) PlaygroundDo(ctx context.Context, id string) error {
	if e.WorkshopStatus().Phase != "ready" {
		return fmt.Errorf("build the variant indexes first — the playground index lives on their collection")
	}
	label := id
	for _, a := range e.B.Workshop().PlaygroundActions() {
		if a.ID == id {
			label = a.Label
		}
	}
	if err := e.B.Workshop().PlaygroundDo(ctx, id); err != nil {
		return err
	}
	e.ws.mu.Lock()
	if e.ws.pg != nil && e.ws.pg.cancel != nil {
		e.ws.pg.cancel()
	}
	wctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	p := &playground{started: time.Now(), action: label, cancel: cancel}
	e.ws.pg = p
	e.ws.mu.Unlock()
	go e.watchPlayground(wctx, p)
	return nil
}

func (e *Engine) watchPlayground(ctx context.Context, p *playground) {
	defer func() {
		e.ws.mu.Lock()
		p.cancel = nil
		e.ws.mu.Unlock()
	}()
	vec := e.Model.Embed("customers can't connect from their new office")
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	for {
		ev := e.B.Workshop().PlaygroundProbe(ctx, vec)
		ev.T = time.Since(p.started).Seconds()
		// Timings vary on every probe; a change worth a timeline row is a change in
		// anything else.
		key := ev.Status + fmt.Sprint(ev.Queryable) + ev.Version + ev.Detail + untimed(ev.Plain) + untimed(ev.Filter)
		e.ws.mu.Lock()
		if key != p.last {
			p.last = key
			if len(p.events) == 0 {
				ev.Action = p.action
			}
			p.events = append(p.events, ev)
			if len(p.events) > 60 {
				p.events = p.events[len(p.events)-60:]
			}
		}
		e.ws.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func causeOf(msg string) string {
	msg = firstLineOf(msg)
	if i := strings.LastIndex(msg, "caused by ::"); i >= 0 {
		return strings.TrimSpace(msg[i+len("caused by ::"):])
	}
	return msg
}

// defVersion reads latestDefinitionVersion, which is {version, createdAt}.
func shorten(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// untimed drops a probe result's trailing " · 1.2 ms".
func untimed(s string) string {
	if i := strings.LastIndex(s, " · "); i > 0 && strings.HasSuffix(s, " ms") {
		return s[:i]
	}
	return s
}
