package corpus

import (
	"os"
	"sort"
	"testing"

	"supportsim/internal/embed"
)

func TestValidate(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestVectorsFindTheRightArticle is the claim the dashboard makes, checked offline:
// the nearest article to a ticket's embedding is usually the one that answers it.
// It also prints the score distributions the desk's thresholds are chosen from.
func TestVectorsFindTheRightArticle(t *testing.T) {
	dir := os.Getenv("SUPPORTSIM_MODEL_DIR")
	if dir == "" {
		t.Skip("SUPPORTSIM_MODEL_DIR not set")
	}
	m, err := embed.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var kbText []string
	for _, a := range Articles {
		kbText = append(kbText, a.EmbedText())
	}
	kb := m.EmbedBatch(kbText)

	g := NewGenerator(7)
	var tickets []Ticket
	var texts []string
	for i := 0; i < 600; i++ {
		tk := g.Random()
		tickets = append(tickets, tk)
		texts = append(texts, tk.EmbedText())
	}
	vecs := m.EmbedBatch(texts)

	correct := 0
	var rightScores, wrongScores, margins []float64
	for i, v := range vecs {
		best, bestS, second := -1, float32(-2), float32(-2)
		for j, k := range kb {
			s := embed.Cosine(v, k)
			if s > bestS {
				second = bestS
				best, bestS = j, s
			} else if s > second {
				second = s
			}
		}
		if Articles[best].Slug == tickets[i].TruthKB {
			correct++
			rightScores = append(rightScores, float64(bestS))
		} else {
			wrongScores = append(wrongScores, float64(bestS))
		}
		margins = append(margins, float64(bestS-second))
	}
	acc := float64(correct) / float64(len(vecs))
	t.Logf("top-1 article accuracy: %.1f%% (%d/%d)", acc*100, correct, len(vecs))
	t.Logf("top-1 score when right: %s", pct(rightScores))
	t.Logf("top-1 score when wrong: %s", pct(wrongScores))

	// Ticket ↔ ticket: same archetype vs different, for duplicate detection.
	var same, diff []float64
	for i := 0; i < 300; i++ {
		for j := i + 1; j < 300; j++ {
			s := float64(embed.Cosine(vecs[i], vecs[j]))
			if tickets[i].Archetype == tickets[j].Archetype {
				same = append(same, s)
			} else {
				diff = append(diff, s)
			}
		}
	}
	t.Logf("ticket↔ticket same problem: %s", pct(same))
	t.Logf("ticket↔ticket different:    %s", pct(diff))

	// kNN team routing from the other tickets.
	right := 0
	for i := 0; i < 300; i++ {
		type nb struct {
			s    float32
			team string
		}
		var nbs []nb
		for j := 300; j < 600; j++ {
			nbs = append(nbs, nb{embed.Cosine(vecs[i], vecs[j]), tickets[j].TruthTeam})
		}
		sort.Slice(nbs, func(a, b int) bool { return nbs[a].s > nbs[b].s })
		votes := map[string]float32{}
		for _, n := range nbs[:10] {
			votes[n.team] += n.s
		}
		bestT, bestV := "", float32(0)
		for k, v := range votes {
			if v > bestV {
				bestT, bestV = k, v
			}
		}
		if bestT == tickets[i].TruthTeam {
			right++
		}
	}
	t.Logf("kNN team routing accuracy: %.1f%%", float64(right)/3)

	// Article via similar past tickets: the answer the nearest resolved tickets used.
	for _, k := range []int{1, 3, 5} {
		ok := 0
		for i := 0; i < 300; i++ {
			type nb struct {
				s  float32
				kb string
			}
			var nbs []nb
			for j := 300; j < 600; j++ {
				nbs = append(nbs, nb{embed.Cosine(vecs[i], vecs[j]), tickets[j].TruthKB})
			}
			sort.Slice(nbs, func(a, b int) bool { return nbs[a].s > nbs[b].s })
			votes := map[string]float32{}
			for _, n := range nbs[:k] {
				votes[n.kb] += n.s
			}
			bestK, bestV := "", float32(0)
			for kk, v := range votes {
				if v > bestV {
					bestK, bestV = kk, v
				}
			}
			if bestK == tickets[i].TruthKB {
				ok++
			}
		}
		t.Logf("article via %d similar past tickets: %.1f%%", k, float64(ok)/3)
	}
	if acc < 0.6 {
		t.Errorf("top-1 accuracy %.2f is too low for the demo's claim", acc)
	}
}

func pct(xs []float64) string {
	if len(xs) == 0 {
		return "n/a"
	}
	sort.Float64s(xs)
	q := func(p float64) float64 { return xs[int(p*float64(len(xs)-1))] }
	return fmtf("p10=%.2f p50=%.2f p90=%.2f (n=%d)", q(.1), q(.5), q(.9), len(xs))
}
