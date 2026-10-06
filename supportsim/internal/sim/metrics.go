package sim

import (
	"sort"
	"sync"
)

// Metrics are the desk's running scoreboard. Every accuracy figure is computed
// against the simulator's ground truth, for keyword and vector side by side on
// the same tickets — the comparison is the dashboard's argument, so it has to be
// measured, not assumed.
type Metrics struct {
	mu sync.Mutex
	c  Counters

	embed, vector, keyword []float64
}

// Counters are the scoreboard's numbers.
type Counters struct {
	Tickets           int `json:"tickets"`
	AutoAnswered      int `json:"autoAnswered"`
	AutoCorrect       int `json:"autoCorrect"`
	AutoReopened      int `json:"autoReopened"`
	Duplicates        int `json:"duplicates"`
	DupCorrect        int `json:"dupCorrect"`
	Incidents         int `json:"incidents"`
	AnswerVector      int `json:"answerVector"`      // article via similar resolved tickets — right
	AnswerKeywordCase int `json:"answerKeywordCase"` // article via $search hits on resolved tickets — right
	AnswerDirect      int `json:"answerDirect"`      // article via $vectorSearch on the KB — right
	AnswerKeyword     int `json:"answerKeyword"`     // article via $search on the KB — right
	AnswerHybrid      int `json:"answerHybrid"`      // article via hybrid on the KB — right
	RouteVector       int `json:"routeVector"`       // team via similar resolved tickets — right
	RouteKeyword      int `json:"routeKeyword"`      // team via $search on resolved tickets — right
	Scored            int `json:"scored"`            // tickets the comparisons above are out of
	HybridScored      int `json:"hybridScored"`      // tickets AnswerHybrid is out of (reset when the fusion changes)
}

const latWindow = 300

func push(xs []float64, v float64) []float64 {
	xs = append(xs, v)
	if len(xs) > latWindow {
		xs = xs[len(xs)-latWindow:]
	}
	return xs
}

// Latency records one ticket's timings.
func (m *Metrics) Latency(embedMs, vectorMs, keywordMs float64) {
	m.mu.Lock()
	m.embed = push(m.embed, embedMs)
	m.vector = push(m.vector, vectorMs)
	m.keyword = push(m.keyword, keywordMs)
	m.mu.Unlock()
}

// Update applies f to the counters under the lock.
func (m *Metrics) Update(f func(c *Counters)) {
	m.mu.Lock()
	f(&m.c)
	m.mu.Unlock()
}

// Lat is p50/p95 of a latency window.
type Lat struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
}

func pct(xs []float64) Lat {
	if len(xs) == 0 {
		return Lat{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return Lat{P50: s[len(s)/2], P95: s[(len(s)*95)/100]}
}

// View is the scoreboard as the dashboard reads it.
type View struct {
	Counters
	MinutesSaved int `json:"minutesSaved"`
	EmbedLat     Lat `json:"embedLat"`
	VectorLat    Lat `json:"vectorLat"`
	KeywordLat   Lat `json:"keywordLat"`
}

// Snapshot copies the scoreboard.
func (m *Metrics) Snapshot() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := View{Counters: m.c}
	// A first answer an agent did not have to write is about twelve minutes; a
	// duplicate merged before anyone worked it is about eight. Rough, stated in the
	// UI, and only counted when the answer was actually right.
	v.MinutesSaved = m.c.AutoCorrect*12 + m.c.DupCorrect*8
	v.EmbedLat, v.VectorLat, v.KeywordLat = pct(m.embed), pct(m.vector), pct(m.keyword)
	return v
}
