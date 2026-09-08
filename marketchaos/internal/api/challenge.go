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
	Active     bool   `json:"active"`
	ID         string `json:"id,omitempty"`
	Title      string `json:"title,omitempty"`
	Category   string `json:"category,omitempty"`
	Difficulty string `json:"difficulty,omitempty"`
	Mechanism  string `json:"mechanism,omitempty"`
	Scenario   string `json:"scenario,omitempty"`
	Symptom    string `json:"symptom,omitempty"`
	State      string `json:"state,omitempty"`
	HintsUsed  int    `json:"hintsUsed"`
	TotalHints int    `json:"totalHints"`
	// RootCause/FixApproach are the learner's currently SELECTED answer ids
	// (echoed back so the panel can restore selection across a reload) —
	// never the correct answer, which lives only in the Challenge struct on
	// the server and is never serialized to the client.
	RootCause   string `json:"rootCause,omitempty"`
	FixApproach string `json:"fixApproach,omitempty"`
	// RootCauseChoices/FixApproachChoices are the 4 radio-button options for
	// each diagnosis question — a deterministic (per challenge) subset of
	// the full pools in catalog.go, computed fresh on every request rather
	// than cached, so they're never stale across a container restart.
	RootCauseChoices   []challenge.DiagOption `json:"rootCauseChoices,omitempty"`
	FixApproachChoices []challenge.DiagOption `json:"fixApproachChoices,omitempty"`
	AppliedVariant     bool                   `json:"appliedVariant"`
	StartedAt          time.Time              `json:"startedAt,omitempty"`
}

func (h *Handler) handleChallengeActive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.activeView())
}

func (h *Handler) activeView() activeChallengeView {
	c, state, active := h.Engine.Challenges.Active()
	if !active {
		return activeChallengeView{Active: false}
	}
	rootCause, fixApproach := h.Engine.Challenges.DiagnosisAnswers()
	rootCauseChoices, fixApproachChoices := h.Engine.Challenges.DiagnosisChoices()
	return activeChallengeView{
		Active: true, ID: c.ID, Title: c.Title, Category: string(c.Category), Difficulty: string(c.Difficulty),
		Mechanism: string(c.Mechanism), Scenario: c.Scenario, Symptom: c.Symptom, State: string(state),
		HintsUsed: h.Engine.Challenges.HintsUsed(), TotalHints: len(c.Hints),
		RootCause: rootCause, FixApproach: fixApproach,
		RootCauseChoices: rootCauseChoices, FixApproachChoices: fixApproachChoices,
		AppliedVariant: h.Engine.Challenges.AppliedVariant(),
		StartedAt:      h.Engine.Challenges.StartedAt(),
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
		RootCause   string `json:"rootCause"`
		FixApproach string `json:"fixApproach"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := h.Engine.Challenges.SetDiagnosisAnswers(body.RootCause, body.FixApproach); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, h.activeView())
}

func (h *Handler) handleChallengeToggleVariant(w http.ResponseWriter, r *http.Request) {
	if err := h.Engine.Challenges.ToggleVariant(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, h.activeView())
}

// handleChallengeBaseline/handleChallengeValidate each block for
// grading.go's gradingWindow (~15s) — a learner clicking "Capture Baseline"
// or "Validate Solution" and watching a brief wait is the expected UX here,
// not a background job with progress polling.
func (h *Handler) handleChallengeBaseline(w http.ResponseWriter, r *http.Request) {
	if err := h.Engine.CaptureBaseline(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, h.activeView())
}

func (h *Handler) handleChallengeValidate(w http.ResponseWriter, r *http.Request) {
	result, err := h.Engine.ValidateSolution(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, result)
}
