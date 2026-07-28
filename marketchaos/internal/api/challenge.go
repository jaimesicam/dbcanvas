package api

import (
	"encoding/json"
	"net/http"
	"time"

	"marketchaos/internal/challenge"
)

// challengeSummary is the catalog listing's shape — deliberately omits
// Setup/Teardown SQL and the full hint list (hints are revealed
// progressively via POST /api/challenges/hint, not dumped up front).
type challengeSummary struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Category       string `json:"category"`
	Difficulty     string `json:"difficulty"`
	Mechanism      string `json:"mechanism"`
	RequiresFamily string `json:"requiresFamily,omitempty"`
	Scenario       string `json:"scenario"`
	Symptom        string `json:"symptom"`
	HintCount      int    `json:"hintCount"`
}

// handleChallengeCatalog lists every challenge whose RequiresFamily matches
// this deployment's target family ("" always matches) — a PXC-only
// challenge simply doesn't appear on a standalone Percona Server target,
// rather than appearing and immediately erroring if started.
func (h *Handler) handleChallengeCatalog(w http.ResponseWriter, r *http.Request) {
	family := h.Engine.Kind.Family()
	out := make([]challengeSummary, 0, len(challenge.Catalog))
	for _, c := range challenge.Catalog {
		if c.RequiresFamily != "" && c.RequiresFamily != family {
			continue
		}
		out = append(out, challengeSummary{
			ID: c.ID, Title: c.Title, Category: string(c.Category), Difficulty: string(c.Difficulty),
			Mechanism: string(c.Mechanism), RequiresFamily: c.RequiresFamily,
			Scenario: c.Scenario, Symptom: c.Symptom, HintCount: len(c.Hints),
		})
	}
	writeJSON(w, out)
}

type activeChallengeView struct {
	Active         bool      `json:"active"`
	ID             string    `json:"id,omitempty"`
	Title          string    `json:"title,omitempty"`
	Category       string    `json:"category,omitempty"`
	Difficulty     string    `json:"difficulty,omitempty"`
	Mechanism      string    `json:"mechanism,omitempty"`
	Scenario       string    `json:"scenario,omitempty"`
	Symptom        string    `json:"symptom,omitempty"`
	State          string    `json:"state,omitempty"`
	HintsUsed      int       `json:"hintsUsed"`
	TotalHints     int       `json:"totalHints"`
	Diagnosis      string    `json:"diagnosis,omitempty"`
	AppliedVariant bool      `json:"appliedVariant"`
	StartedAt      time.Time `json:"startedAt,omitempty"`
}

func (h *Handler) handleChallengeActive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.activeView())
}

func (h *Handler) activeView() activeChallengeView {
	c, state, active := h.Engine.Challenges.Active()
	if !active {
		return activeChallengeView{Active: false}
	}
	return activeChallengeView{
		Active: true, ID: c.ID, Title: c.Title, Category: string(c.Category), Difficulty: string(c.Difficulty),
		Mechanism: string(c.Mechanism), Scenario: c.Scenario, Symptom: c.Symptom, State: string(state),
		HintsUsed: h.Engine.Challenges.HintsUsed(), TotalHints: len(c.Hints),
		Diagnosis: h.Engine.Challenges.Diagnosis(), AppliedVariant: h.Engine.Challenges.AppliedVariant(),
		StartedAt: h.Engine.Challenges.StartedAt(),
	}
}

func (h *Handler) handleChallengeStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, ok := challenge.ByID(id)
	if !ok {
		http.Error(w, "unknown challenge "+id, http.StatusNotFound)
		return
	}
	if family := h.Engine.Kind.Family(); c.RequiresFamily != "" && c.RequiresFamily != family {
		http.Error(w, "challenge "+id+" requires a PXC-family target, this deployment is "+family, http.StatusBadRequest)
		return
	}
	if err := h.Engine.Challenges.Start(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, h.activeView())
}

func (h *Handler) handleChallengeReset(w http.ResponseWriter, r *http.Request) {
	if err := h.Engine.Challenges.Reset(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, h.activeView())
}

func (h *Handler) handleChallengeHint(w http.ResponseWriter, r *http.Request) {
	hint, ok := h.Engine.Challenges.UnlockHint()
	if !ok {
		http.Error(w, "no more hints (or no challenge active)", http.StatusConflict)
		return
	}
	writeJSON(w, hint)
}

func (h *Handler) handleChallengeDiagnosis(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	h.Engine.Challenges.SetDiagnosis(body.Text)
	writeJSON(w, h.activeView())
}

func (h *Handler) handleChallengeApplyVariant(w http.ResponseWriter, r *http.Request) {
	if err := h.Engine.Challenges.ApplyVariant(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, h.activeView())
}
