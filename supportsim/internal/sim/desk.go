package sim

import (
	"context"
	"fmt"
	"sort"
	"time"

	"supportsim/internal/corpus"
	"supportsim/internal/search"
	"supportsim/internal/store"
)

// TicketView is a ticket as the dashboard shows it.
type TicketView struct {
	ID        int64     `json:"id"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Customer  string    `json:"customer"`
	Plan      string    `json:"plan"`
	CreatedAt time.Time `json:"createdAt"`
	Status    string    `json:"status"` // open | auto_answered | duplicate | resolved
	Team      string    `json:"team"`   // routed team
	Truth     Truth     `json:"truth"`
}

// Truth is what the simulator knows and a real desk never would.
type Truth struct {
	Archetype string `json:"archetype"`
	Team      string `json:"team"`
	KB        string `json:"kb"`
	KBTitle   string `json:"kbTitle"`
}

// Pick is one engine's answer to "which article solves this?" or "which team?".
type Pick struct {
	Value   string  `json:"value"`           // article slug or team
	Title   string  `json:"title,omitempty"` // article title
	Score   float64 `json:"score"`           // engine's own score
	Cosine  float64 `json:"cosine,omitempty"`
	Share   float64 `json:"share,omitempty"` // fraction of neighbour votes
	Correct bool    `json:"correct"`
}

// Neighbor is a similar past ticket.
type Neighbor struct {
	ID     any     `json:"id"`
	Title  string  `json:"title"`
	Team   string  `json:"team"`
	KB     string  `json:"kb"`
	Cosine float64 `json:"cosine"`
}

// Decision is everything the desk did with one ticket, and why.
type Decision struct {
	Ticket    TicketView `json:"ticket"`
	Tokens    int        `json:"tokens"`
	Neighbors []Neighbor `json:"neighbors"` // similar resolved tickets

	Answer        Pick `json:"answer"`        // article, from similar resolved tickets
	DirectAnswer  Pick `json:"directAnswer"`  // article, $vectorSearch on the KB
	KeywordAnswer Pick `json:"keywordAnswer"` // article, $search on the KB
	HybridAnswer  Pick `json:"hybridAnswer"`  // article, $rankFusion on the KB
	KeywordCase   Pick `json:"keywordCase"`   // article, from $search hits on resolved tickets
	Route         Pick `json:"route"`         // team, from similar resolved tickets
	KeywordRoute  Pick `json:"keywordRoute"`  // team, from $search on resolved tickets
	AutoAnswered  bool `json:"autoAnswered"`
	Novel         bool `json:"novel"` // nothing resolved resembles it (or it resembles past outages)

	Duplicate *Neighbor `json:"duplicate,omitempty"`
	Incident  *Incident `json:"incident,omitempty"`

	EmbedMs   float64 `json:"embedMs"`
	VectorMs  float64 `json:"vectorMs"`
	KeywordMs float64 `json:"keywordMs"`
	Engine    string  `json:"engine"`

	Pipelines map[string]string `json:"pipelines"`

	resolveAfter time.Duration
	dupRight     bool
}

// Incident is a cluster of tickets about the same platform problem.
type Incident struct {
	ID        int       `json:"id"`
	Title     string    `json:"title"`
	FirstSeen time.Time `json:"firstSeen"`
	Tickets   []int64   `json:"tickets"`
	Truth     string    `json:"truth"` // archetype of the ticket that raised it
}

// Process runs one ticket through the desk.
func (e *Engine) Process(ctx context.Context, t corpus.Ticket) (*Decision, error) {
	id, err := e.B.NextTicketNumber(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().Truncate(time.Millisecond)
	kb, _ := corpus.ArticleBySlug(t.TruthKB)
	d := &Decision{
		Ticket: TicketView{ID: id, Subject: t.Subject, Body: t.Body, Customer: t.Customer, Plan: t.Plan, CreatedAt: now, Status: "open",
			Truth: Truth{Archetype: t.Archetype, Team: t.TruthTeam, KB: t.TruthKB, KBTitle: kb.Title}},
		Pipelines:    map[string]string{},
		resolveAfter: time.Duration(45+e.rng.Intn(60)) * time.Second,
	}

	// 1. Text → vector.
	t0 := time.Now()
	vec := e.Model.Embed(t.EmbedText())
	d.EmbedMs = msSince(t0)
	d.Tokens = len(e.Model.Tokenize(t.EmbedText()))

	// 2. Similar resolved tickets — the desk's memory. Everything it decides with
	//    confidence comes from here: what the closest past cases were about, which
	//    team solved them, which article closed them.
	nb := e.Search.Vector(ctx, search.VectorQuery{Collection: store.Tickets, Vector: vec, K: 10, NumCandidates: 200,
		Filter: search.Filter{Status: []string{"resolved"}}})
	d.Engine = nb.Engine
	d.VectorMs = nb.Ms
	d.Pipelines["similar"] = nb.Pipeline
	for _, h := range nb.Hits {
		d.Neighbors = append(d.Neighbors, Neighbor{ID: h.ID, Title: h.Title, Team: h.Team, KB: h.KB, Cosine: h.Cosine})
	}
	d.Route = vote(d.Neighbors, 10, func(n Neighbor) string { return n.Team })
	d.Route.Correct = d.Route.Value == t.TruthTeam
	d.Answer = vote(d.Neighbors, 5, func(n Neighbor) string { return n.KB })
	d.Answer.Correct = d.Answer.Value == t.TruthKB
	if a, ok := corpus.ArticleBySlug(d.Answer.Value); ok {
		d.Answer.Title = a.Title
	}

	// 3. The knowledge base directly, three ways.
	dv := e.Search.Vector(ctx, search.VectorQuery{Collection: store.KB, Vector: vec, K: 3, NumCandidates: 50})
	d.Pipelines["kbVector"] = dv.Pipeline
	d.DirectAnswer = top(dv.Hits, t.TruthKB)
	kw := e.Search.Keyword(ctx, store.KB, t.EmbedText(), 3, false)
	d.KeywordMs = kw.Ms
	d.Pipelines["kbKeyword"] = kw.Pipeline
	d.KeywordAnswer = top(kw.Hits, t.TruthKB)
	hy := e.Search.Hybrid(ctx, search.VectorQuery{Collection: store.KB, Vector: vec, K: 3, NumCandidates: 50}, t.EmbedText(), false, e.HybridOpts())
	d.Pipelines["kbHybrid"] = hy.Pipeline
	d.HybridAnswer = top(hy.Hits, t.TruthKB)

	// 4. Keyword over the same history: the same two votes, over $search hits.
	kr := e.Search.Keyword(ctx, store.Tickets, t.EmbedText(), 10, true)
	d.Pipelines["keywordRoute"] = kr.Pipeline
	var kn []Neighbor
	for _, h := range kr.Hits {
		kn = append(kn, Neighbor{ID: h.ID, Title: h.Title, Team: h.Team, KB: h.KB, Cosine: h.Score})
	}
	d.KeywordRoute = vote(kn, 10, func(n Neighbor) string { return n.Team })
	d.KeywordRoute.Correct = d.KeywordRoute.Value == t.TruthTeam
	d.KeywordCase = vote(kn, 5, func(n Neighbor) string { return n.KB })
	d.KeywordCase.Correct = d.KeywordCase.Value == t.TruthKB

	// 5. A re-send: the same customer, an open ticket, the same problem in new words.
	dup := e.Search.Vector(ctx, search.VectorQuery{Collection: store.Tickets, Vector: vec, K: 1, NumCandidates: 50,
		Filter: search.Filter{Customer: t.Customer, Status: []string{"open", "auto_answered"}, Since: now.Add(-30 * time.Minute)}})
	d.Pipelines["duplicate"] = dup.Pipeline
	if len(dup.Hits) > 0 && dup.Hits[0].Cosine >= duplicateCos {
		h := dup.Hits[0]
		d.Duplicate = &Neighbor{ID: h.ID, Title: h.Title, Team: h.Team, Cosine: h.Cosine}
		// Was it really the same problem? Read the earlier ticket's ground truth from
		// the database — it may be long gone from the in-memory window.
		d.dupRight = e.B.TicketArchetype(ctx, h.ID) == t.Archetype
	}

	// 6. An outage. A burst alone is not one: at a high arrival rate, everyday
	//    tickets about a common problem cluster too. An outage is a burst of
	//    something *new* — tickets nothing resolved resembles (or that resemble past
	//    outage tickets, closed with the status-page article) — arriving together.
	//    Outage reports are worded very differently from each other ("Frankfurt is
	//    down", "our German users see errors"), so the bar for "together" is lower
	//    than for a duplicate; requiring novelty is what keeps that from firing on
	//    ordinary traffic.
	novel := len(d.Neighbors) == 0 || d.Neighbors[0].Cosine < novelCos || d.Answer.Value == "service-status"
	d.Novel = novel
	var close []int64
	if novel {
		burst := e.Search.Vector(ctx, search.VectorQuery{Collection: store.Tickets, Vector: vec, K: 20, NumCandidates: 200,
			Filter: search.Filter{Since: now.Add(-incidentSpan)}})
		d.Pipelines["incident"] = burst.Pipeline
		e.mu.Lock()
		for _, h := range burst.Hits {
			n, ok := h.ID.(int64)
			if _, isNovel := e.novel[n]; ok && isNovel && h.Cosine >= incidentCos {
				close = append(close, n)
			}
		}
		e.novel[id] = now
		for k, at := range e.novel {
			if now.Sub(at) > 2*incidentSpan {
				delete(e.novel, k)
			}
		}
		e.mu.Unlock()
	}

	// 7. Act.
	team := d.Route.Value
	if team == "" {
		team = "Unassigned"
	}
	status := "open"
	switch {
	case d.Duplicate != nil:
		status = "duplicate"
	case len(d.Neighbors) > 0 && d.Neighbors[0].Cosine >= autoAnswerCos && d.Answer.Share >= autoAnswerAgr:
		status = "auto_answered"
		d.AutoAnswered = true
	}
	d.Ticket.Status, d.Ticket.Team = status, team

	rec := TicketRecord{ID: id, T: t, Vec: vec, Created: now, Status: status, Team: team}
	if d.AutoAnswered {
		rec.SuggestedKB, rec.SuggestedCos = d.Answer.Value, d.Answer.Cosine
	}
	if d.Duplicate != nil {
		rec.DuplicateOf = d.Duplicate.ID
	}
	if err := e.B.InsertTickets(ctx, []TicketRecord{rec}); err != nil {
		return nil, fmt.Errorf("insert ticket: %w", err)
	}
	e.Mirror.Put(store.Tickets, &search.Doc{ID: id, Title: t.Subject, Team: team, Status: status, Plan: t.Plan, Customer: t.Customer, CreatedAt: now, Vec: vec})

	if len(close) >= incidentMin-1 { // the ones found plus this one
		d.Incident = e.raiseIncident(append(close, id), t)
		// Part of an outage: the answer is the status page and the owner is the
		// incident team, whatever the nearest everyday tickets were about.
		d.Answer = Pick{Value: "service-status", Title: "Checking service status during an incident", Share: 1, Cosine: d.Answer.Cosine}
		d.Route = Pick{Value: "Incident Response", Share: 1}
		d.Answer.Correct = d.Answer.Value == t.TruthKB
		d.Route.Correct = d.Route.Value == t.TruthTeam
		e.B.UpdateTicket(ctx, id, TicketSet{Team: "Incident Response", Incident: d.Incident.ID})
		e.Mirror.Update(id, "", "Incident Response", "")
		d.Ticket.Team = "Incident Response"
	}

	e.score(d, t)
	// Published before the lifecycle loop can see it, so the event is encoded from a
	// decision nothing else is changing yet.
	e.Bus.Publish("ticket", d)
	e.mu.Lock()
	e.recent = append([]*Decision{d}, e.recent...)
	if len(e.recent) > 150 {
		e.recent = e.recent[:150]
	}
	e.mu.Unlock()
	return d, nil
}

func (e *Engine) score(d *Decision, t corpus.Ticket) {
	e.Stats.Latency(d.EmbedMs, d.VectorMs, d.KeywordMs)
	e.Stats.Update(func(m *Counters) {
		m.Tickets++
		if a, ok := corpus.ArchetypeByID(t.Archetype); ok && a.Incident {
			// Outage tickets have no everyday article to get right; they are scored by
			// whether the incident was raised, not by the answer comparison.
			return
		}
		m.Scored++
		inc := func(p *int, ok bool) {
			if ok {
				*p++
			}
		}
		inc(&m.AnswerVector, d.Answer.Correct)
		inc(&m.AnswerKeywordCase, d.KeywordCase.Correct)
		inc(&m.AnswerDirect, d.DirectAnswer.Correct)
		inc(&m.AnswerKeyword, d.KeywordAnswer.Correct)
		inc(&m.AnswerHybrid, d.HybridAnswer.Correct)
		m.HybridScored++
		inc(&m.RouteVector, d.Route.Correct)
		inc(&m.RouteKeyword, d.KeywordRoute.Correct)
		if d.AutoAnswered {
			m.AutoAnswered++
			inc(&m.AutoCorrect, d.Answer.Correct)
		}
		if d.Duplicate != nil {
			m.Duplicates++
			inc(&m.DupCorrect, d.dupRight)
		}
	})
}

// raiseIncident opens an incident for a burst, or adds to the one already open
// for it — an incident stays one incident however many more customers write in.
func (e *Engine) raiseIncident(ids []int64, t corpus.Ticket) *Incident {
	e.mu.Lock()
	defer e.mu.Unlock()
	member := map[int64]bool{}
	for _, id := range ids {
		member[id] = true
	}
	for _, inc := range e.incidents {
		if time.Since(inc.FirstSeen) > 15*time.Minute {
			continue
		}
		overlap := false
		for _, x := range inc.Tickets {
			if member[x] {
				overlap = true
				break
			}
		}
		if overlap {
			seen := map[int64]bool{}
			for _, x := range inc.Tickets {
				seen[x] = true
			}
			for _, id := range ids {
				if !seen[id] {
					inc.Tickets = append(inc.Tickets, id)
				}
			}
			e.Bus.Publish("incident", inc)
			return inc
		}
	}
	inc := &Incident{ID: len(e.incidents) + 1, Title: t.Subject, FirstSeen: time.Now(), Tickets: ids, Truth: t.Archetype}
	e.incidents = append([]*Incident{inc}, e.incidents...)
	e.Stats.Update(func(m *Counters) { m.Incidents++ })
	e.Bus.Publish("incident", inc)
	return inc
}

// vote is k-nearest-neighbour classification: the label the closest neighbours
// share, each vote weighted by its similarity.
func vote(ns []Neighbor, k int, label func(Neighbor) string) Pick {
	if len(ns) == 0 {
		return Pick{}
	}
	if k > len(ns) {
		k = len(ns)
	}
	w := map[string]float64{}
	total := 0.0
	for _, n := range ns[:k] {
		v := label(n)
		if v == "" {
			continue
		}
		s := n.Cosine
		if s < 0.01 {
			s = 0.01
		}
		w[v] += s
		total += s
	}
	type kv struct {
		k string
		v float64
	}
	var all []kv
	for k, v := range w {
		all = append(all, kv{k, v})
	}
	if len(all) == 0 {
		return Pick{}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	return Pick{Value: all[0].k, Score: all[0].v, Share: all[0].v / total, Cosine: ns[0].Cosine}
}

func top(hs []search.Hit, truth string) Pick {
	if len(hs) == 0 {
		return Pick{}
	}
	return Pick{Value: hs[0].KB, Title: hs[0].Title, Score: hs[0].Score, Correct: hs[0].KB == truth}
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
