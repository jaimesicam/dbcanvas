// Package sim is the support desk: tickets arrive, each is embedded and searched,
// and the desk acts on what vector search finds — answers, routes, merges and
// raises incidents — while a keyword engine is asked the same questions on the
// side so the two can be scored against each other.
package sim

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"supportsim/internal/corpus"
	"supportsim/internal/embed"
	"supportsim/internal/search"
	"supportsim/internal/store"
)

// Thresholds, in cosine similarity. They were read off the score distributions
// corpus_test.go prints — same-problem pairs sit around 0.54 (p50) and different-
// problem pairs around 0.17, with p90s at 0.82 and 0.32 — and the Lab tab lets you
// see why a single number can never be perfect.
const (
	autoAnswerCos = 0.70 // nearest resolved ticket at least this close …
	autoAnswerAgr = 0.60 // … and this share of the neighbours' votes agree
	duplicateCos  = 0.70 // same customer, still open, this close: a re-send
	novelCos      = 0.60 // nothing resolved is this close: a problem the desk hasn't seen
	incidentCos   = 0.40 // a novel ticket this close to other novel tickets …
	incidentMin   = 4    // … this many of them (it included) within incidentSpan: an outage
	incidentSpan  = 3 * time.Minute
	historySeed   = 420 // resolved tickets written on first start
	ticketCap     = 4000
)

// Engine runs the desk.
type Engine struct {
	B      Backend
	Model  *embed.Model
	Search Searcher
	Mirror *search.Mirror
	Bus    *Bus
	Stats  *Metrics
	Label  string

	gen *corpus.Generator
	rng *rand.Rand

	mu        sync.Mutex
	phase     string
	detail    string
	paused    bool
	perMinute float64
	indexes   []store.IndexStatus
	searchErr string
	info      store.ServerInfo
	recent    []*Decision // newest first
	incidents []*Incident
	lastBurst time.Time
	pending   []corpus.Ticket // queued tickets (outage bursts)
	mapCache  *Map
	mapAt     time.Time
	novel     map[int64]time.Time // recent tickets nothing resolved resembled
	hybrid    search.HybridOpts   // how the desk fuses keyword and vector (Showdown can change it)
	eval      []evalItem          // fixed scoring set for the tune sweep and the index benchmark
	ws        workshop            // the Index Workshop's variant collection and playground
}

// New builds an engine.
// New builds an engine over a backend; mirror must be the one the backend searches
// in-app when it has no native vector search.
func New(b Backend, mirror *search.Mirror, m *embed.Model, label string) *Engine {
	return &Engine{
		B: b, Model: m, Mirror: mirror, Label: label,
		Search:    b.Searcher(),
		Bus:       NewBus(),
		Stats:     &Metrics{},
		gen:       corpus.NewGenerator(time.Now().UnixNano()),
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
		phase:     "starting",
		perMinute: 12,
		lastBurst: time.Now(),
		novel:     map[int64]time.Time{},
		hybrid:    search.DefaultHybrid(),
	}
}

func (e *Engine) setPhase(p, d string) {
	e.mu.Lock()
	e.phase, e.detail = p, d
	e.mu.Unlock()
	log.Printf("supportsim: %s — %s", p, d)
	e.Bus.Publish("status", e.Status())
}

// Status is the header's view of the engine.
type Status struct {
	Engine     string              `json:"engine"` // mongodb | postgres
	Phase      string              `json:"phase"`
	Detail     string              `json:"detail"`
	Paused     bool                `json:"paused"`
	PerMinute  float64             `json:"perMinute"`
	Mongot     bool                `json:"mongot"`
	SearchErr  string              `json:"searchErr,omitempty"`
	Indexes    []store.IndexStatus `json:"indexes"`
	Server     store.ServerInfo    `json:"server"`
	Target     string              `json:"target"`
	Model      string              `json:"model"`
	Dims       int                 `json:"dims"`
	KBDocs     int                 `json:"kbDocs"`
	TicketDocs int                 `json:"ticketDocs"`
	Hybrid     search.HybridOpts   `json:"hybrid"`
	HybridName string              `json:"hybridName"`
}

// Status reports the engine's state.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Status{Engine: e.B.Engine(), Phase: e.phase, Detail: e.detail, Paused: e.paused, PerMinute: e.perMinute,
		Mongot: e.Search.Ready(), SearchErr: e.searchErr, Indexes: e.indexes, Server: e.info, Target: e.Label,
		Model: e.Model.Name(), Dims: e.Model.Dim(), KBDocs: e.Mirror.Len(store.KB), TicketDocs: e.Mirror.Len(store.Tickets),
		Hybrid: e.hybrid, HybridName: e.hybrid.LabelFor(e.B.Engine())}
}

// HybridOpts is the desk's current fusion.
func (e *Engine) HybridOpts() search.HybridOpts {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hybrid
}

// SetHybrid changes how the desk fuses keyword and vector search, and restarts the
// hybrid score so the scoreboard measures the new setting, not a blend of old and new.
func (e *Engine) SetHybrid(o search.HybridOpts) {
	o = o.Normalize()
	e.mu.Lock()
	e.hybrid = o
	e.mu.Unlock()
	e.Stats.Update(func(c *Counters) { c.AnswerHybrid, c.HybridScored = 0, 0 })
	e.Bus.Publish("status", e.Status())
}

// SetControls changes speed / pause.
func (e *Engine) SetControls(paused *bool, perMinute *float64) {
	e.mu.Lock()
	if paused != nil {
		e.paused = *paused
	}
	if perMinute != nil && *perMinute >= 1 && *perMinute <= 240 {
		e.perMinute = *perMinute
	}
	e.mu.Unlock()
	e.Bus.Publish("status", e.Status())
}

// Run brings the desk up and runs it until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	e.info = e.B.Info(ctx)
	e.setPhase("preparing", "creating the schema")
	for {
		if err := e.B.Prepare(ctx); err == nil {
			break
		} else if ctx.Err() != nil {
			return
		} else {
			e.setPhase("preparing", "waiting for the database: "+err.Error())
			time.Sleep(3 * time.Second)
		}
	}
	e.info = e.B.Info(ctx)
	if err := e.seed(ctx); err != nil {
		e.setPhase("error", "seeding failed: "+err.Error())
		return
	}
	e.ensureSearch(ctx, true)
	if e.Search.Ready() {
		e.detectVariants(ctx)
	}
	e.setPhase("running", "taking tickets")

	go e.searchWatch(ctx)
	go e.lifecycle(ctx)
	go e.metricsTick(ctx)
	e.intake(ctx)
}

// ensureSearch creates the vector (and full-text) indexes and, when wait is set,
// waits for them to be usable — on MongoDB the moment mongot has built them, worth
// showing as its own phase; on PostgreSQL the CREATE INDEX itself.
func (e *Engine) ensureSearch(ctx context.Context, wait bool) {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		st := e.B.EnsureSearch(ctx)
		e.mu.Lock()
		e.indexes, e.searchErr = st.Indexes, st.Err
		e.mu.Unlock()
		e.Search.SetReady(st.Ready)
		if st.Ready || !wait || st.Building == "" || time.Now().After(deadline) || ctx.Err() != nil {
			return
		}
		e.setPhase("indexing", st.Building)
		time.Sleep(2 * time.Second)
	}
}

func summarize(st []store.IndexStatus) string {
	s := ""
	for i, x := range st {
		if i > 0 {
			s += ", "
		}
		s += x.Name + " " + x.Status
	}
	return s
}

// searchWatch re-checks the indexes every few seconds, so a mongot that comes up
// (or goes down) after the app started is noticed and shown.
func (e *Engine) searchWatch(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			before := e.Search.Ready()
			e.ensureSearch(ctx, false)
			info := e.B.Info(ctx)
			e.mu.Lock()
			e.info = info
			e.mu.Unlock()
			if before != e.Search.Ready() {
				log.Printf("supportsim: native vector search %v → %v", before, e.Search.Ready())
			}
			e.Bus.Publish("status", e.Status())
		}
	}
}

// ---------------------------------------------------------------- seeding

func (e *Engine) seed(ctx context.Context) error {
	// The knowledge base: always rewritten, so an edited corpus reaches an existing
	// database on the next start.
	e.setPhase("seeding", fmt.Sprintf("embedding %d knowledge-base articles", len(corpus.Articles)))
	var texts []string
	for _, a := range corpus.Articles {
		texts = append(texts, a.EmbedText())
	}
	vecs := e.Model.EmbedBatch(texts)
	for i, a := range corpus.Articles {
		if err := e.B.SaveArticle(ctx, a, vecs[i]); err != nil {
			return fmt.Errorf("write article %s: %w", a.Slug, err)
		}
		e.Mirror.Put(store.KB, &search.Doc{ID: a.Slug, Title: a.Title, Team: a.Team, KB: a.Slug, Vec: vecs[i]})
	}

	n, err := e.B.TicketCount(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		e.setPhase("seeding", fmt.Sprintf("writing %d resolved tickets from the last two weeks", historySeed))
		var tickets []corpus.Ticket
		var tt []string
		for i := 0; i < historySeed; i++ {
			t := e.gen.Random()
			tickets = append(tickets, t)
			tt = append(tt, t.EmbedText())
		}
		tv := e.Model.EmbedBatch(tt)
		recs := make([]TicketRecord, 0, len(tickets))
		now := time.Now()
		for i, t := range tickets {
			id, err := e.B.NextTicketNumber(ctx)
			if err != nil {
				return err
			}
			created := now.Add(-time.Duration(e.rng.Int63n(int64(14 * 24 * time.Hour)))).Truncate(time.Millisecond)
			recs = append(recs, TicketRecord{ID: id, T: t, Vec: tv[i], Created: created, Status: "resolved", Team: t.TruthTeam,
				ResolvedKB: t.TruthKB, ResolvedBy: "agent"})
		}
		if err := e.B.InsertTickets(ctx, recs); err != nil {
			return fmt.Errorf("write history: %w", err)
		}
	}
	e.setPhase("seeding", "loading ticket embeddings")
	return e.B.LoadTickets(ctx, func(d *search.Doc) { e.Mirror.Put(store.Tickets, d) })
}

// ---------------------------------------------------------------- intake

func (e *Engine) intake(ctx context.Context) {
	for ctx.Err() == nil {
		e.mu.Lock()
		paused, rate := e.paused, e.perMinute
		var next *corpus.Ticket
		if len(e.pending) > 0 {
			t := e.pending[0]
			e.pending = e.pending[1:]
			next = &t
		}
		burstDue := time.Since(e.lastBurst) > time.Duration(5+e.rng.Intn(4))*time.Minute
		e.mu.Unlock()

		if paused && next == nil {
			sleep(ctx, 500*time.Millisecond)
			continue
		}
		if burstDue && !paused {
			e.TriggerOutage("")
		}
		t := e.nextTicket(next)
		if _, err := e.Process(ctx, t); err != nil && ctx.Err() == nil {
			log.Printf("supportsim: process ticket: %v", err)
		}
		wait := time.Duration(float64(time.Minute) / rate)
		if next != nil {
			wait = time.Duration(4+e.rng.Intn(6)) * time.Second // an outage arrives fast
		}
		// Arrivals are Poisson-ish rather than a metronome.
		wait = time.Duration(float64(wait) * (0.4 + 1.2*e.rng.Float64()))
		sleep(ctx, wait)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// nextTicket draws the next ticket: a queued outage ticket, or now and then the
// same customer writing in again about the same thing in other words, or a fresh
// everyday ticket.
func (e *Engine) nextTicket(queued *corpus.Ticket) corpus.Ticket {
	if queued != nil {
		return *queued
	}
	e.mu.Lock()
	var resend *Decision
	if len(e.recent) > 3 && e.rng.Float64() < 0.08 {
		resend = e.recent[1+e.rng.Intn(min(len(e.recent)-1, 12))]
	}
	e.mu.Unlock()
	if resend != nil && resend.Ticket.Status == "open" {
		if a, ok := corpus.ArchetypeByID(resend.Ticket.Truth.Archetype); ok && !a.Incident {
			t := e.gen.From(a)
			t.Customer = resend.Ticket.Customer
			t.Plan = resend.Ticket.Plan
			t.Subject = "Re: " + t.Subject
			return t
		}
	}
	return e.gen.Random()
}

// TriggerOutage queues an outage burst: 6–9 customers reporting the same platform
// problem in their own words within a minute or two. id picks the outage; "" is
// random.
func (e *Engine) TriggerOutage(id string) string {
	inc := corpus.Incidents()
	a := inc[e.rng.Intn(len(inc))]
	if found, ok := corpus.ArchetypeByID(id); ok && found.Incident {
		a = found
	}
	n := 6 + e.rng.Intn(4)
	e.mu.Lock()
	for i := 0; i < n; i++ {
		e.pending = append(e.pending, e.gen.From(a))
	}
	e.lastBurst = time.Now()
	e.mu.Unlock()
	return a.ID
}

// ---------------------------------------------------------------- lifecycle

// lifecycle is the human side of the desk: agents resolve open tickets after a
// while (with the right answer — they read the ticket), customers whose automatic
// answer was wrong write back and reopen, and old tickets age out.
func (e *Engine) lifecycle(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	prune := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		e.mu.Lock()
		var resolve, reopen []*Decision
		for _, d := range e.recent {
			age := now.Sub(d.Ticket.CreatedAt)
			switch d.Ticket.Status {
			case "open", "duplicate":
				if age > d.resolveAfter {
					resolve = append(resolve, d)
				}
			case "auto_answered":
				if !d.Answer.Correct && age > 25*time.Second {
					reopen = append(reopen, d)
				} else if d.Answer.Correct && age > 40*time.Second {
					resolve = append(resolve, d)
				}
			}
		}
		e.mu.Unlock()
		for _, d := range reopen {
			e.setStatus(ctx, d, "open", "", "", "customer replied: “that didn't help” — reopened for an agent")
			e.Stats.Update(func(m *Counters) { m.AutoReopened++ })
			d.resolveAfter = now.Sub(d.Ticket.CreatedAt) + time.Duration(30+e.rng.Intn(40))*time.Second
		}
		for _, d := range resolve {
			by := "agent"
			if d.Ticket.Status == "auto_answered" {
				by = "auto"
			}
			e.setStatus(ctx, d, "resolved", d.Ticket.Truth.Team, d.Ticket.Truth.KB, "resolved by "+by)
		}
		if now.Sub(prune) > time.Minute {
			prune = now
			e.prune(ctx)
		}
	}
}

func (e *Engine) setStatus(ctx context.Context, d *Decision, status, team, kb, note string) {
	if err := e.B.UpdateTicket(ctx, d.Ticket.ID, TicketSet{Status: status, Team: team, ResolvedKB: kb, Resolved: status == "resolved"}); err != nil {
		return
	}
	e.Mirror.Update(d.Ticket.ID, status, team, kb)
	e.mu.Lock()
	d.Ticket.Status = status
	if team != "" {
		d.Ticket.Team = team
	}
	e.mu.Unlock()
	e.Bus.Publish("update", map[string]any{"id": d.Ticket.ID, "status": status, "note": note})
}

// prune keeps the tickets bounded: the oldest resolved ones beyond ticketCap are
// deleted, and so leave the vector index.
func (e *Engine) prune(ctx context.Context) {
	for _, id := range e.B.Prune(ctx, ticketCap) {
		e.Mirror.Remove(id)
	}
}

func (e *Engine) metricsTick(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Bus.Publish("metrics", e.Stats.Snapshot())
		}
	}
}

// Recent returns copies of the newest decisions (copies, because the lifecycle
// loop changes a ticket's status while a handler may be encoding it).
func (e *Engine) Recent(n int) []Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n > len(e.recent) {
		n = len(e.recent)
	}
	out := make([]Decision, n)
	for i := 0; i < n; i++ {
		out[i] = *e.recent[i]
	}
	return out
}

// Incidents returns the incidents raised so far, newest first.
func (e *Engine) Incidents() []*Incident {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Incident, len(e.incidents))
	copy(out, e.incidents)
	return out
}

// Decision looks up a recent ticket's decision (a copy).
func (e *Engine) Decision(id int64) *Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, d := range e.recent {
		if d.Ticket.ID == id {
			c := *d
			return &c
		}
	}
	return nil
}
