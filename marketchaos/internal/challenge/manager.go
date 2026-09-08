package challenge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// State is a challenge run's current lifecycle position.
type State string

const (
	StateNone     State = "none"
	StateActive   State = "active"   // Setup applied / app-variant armed, traffic running, no baseline captured yet
	StateBaseline State = "baseline" // baseline measurement captured, waiting for the learner's fix
	StateGraded   State = "graded"   // Validate Solution has run at least once
)

// Manager owns exactly one active challenge run at a time — Start on top of
// an already-active challenge is rejected; Reset must be called first. Pure
// challenge lifecycle only — grading (baseline/validate measurement windows,
// scoring) lives in internal/sim/grading.go, which needs Engine's
// leaderboard/wsrep/serverstats access that this package deliberately
// doesn't depend on.
type Manager struct {
	db *sql.DB

	mu             sync.Mutex
	challenge      Challenge
	state          State
	startedAt      time.Time
	hintsUsed      int
	rootCause      string // selected DiagOption id from RootCauseOptions, "" if unanswered
	fixApproach    string // selected DiagOption id from FixApproachOptions, "" if unanswered
	appliedVariant bool   // for MechanismApp challenges: is the improved-implementation toggle currently on?
}

func NewManager(db *sql.DB) *Manager { return &Manager{db: db, state: StateNone} }

// persistedState is Manager's state, upserted into the shared metrics table
// (id="challenge", the same "one JSON blob row" idiom every sibling sim
// uses for sim_state/metrics) after every mutation, and reloaded once at
// boot via LoadPersisted. Exists because a container restart mid-challenge
// otherwise loses track of Setup SQL already applied against the target —
// found live (stage S4 verification): restarting the container to pick up
// a code change left price_ticks' index dropped with no record any
// challenge had ever touched it, so Reset (state already StateNone in the
// fresh in-memory Manager) correctly did nothing, silently leaving the
// database in a modified state the API had no way to discover or undo.
type persistedState struct {
	ChallengeID    string    `json:"challengeId"`
	State          State     `json:"state"`
	StartedAt      time.Time `json:"startedAt"`
	HintsUsed      int       `json:"hintsUsed"`
	RootCause      string    `json:"rootCause"`
	FixApproach    string    `json:"fixApproach"`
	AppliedVariant bool      `json:"appliedVariant"`
}

// persist upserts the current state — best-effort: a failed write here
// means a subsequent restart might not recover cleanly, but must never
// block or fail the caller's own action (starting/resetting a challenge
// still needs to work even if this particular write races a connection
// blip). Called with the lock already held by every mutating method below.
func (m *Manager) persist() {
	p := persistedState{
		State: m.state, StartedAt: m.startedAt, HintsUsed: m.hintsUsed,
		RootCause: m.rootCause, FixApproach: m.fixApproach, AppliedVariant: m.appliedVariant,
	}
	if m.state != StateNone {
		p.ChallengeID = m.challenge.ID
	}
	b, err := json.Marshal(p)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.db.ExecContext(ctx,
		"INSERT INTO metrics (id, payload, updated_at) VALUES ('challenge', ?, ?) "+
			"ON DUPLICATE KEY UPDATE payload=VALUES(payload), updated_at=VALUES(updated_at)",
		b, time.Now().UTC()); err != nil {
		log.Printf("marketchaos: persist challenge state: %v", err)
	}
}

// LoadPersisted restores Manager's state from a prior process's last
// persist() — called once at boot, before agents start. Never re-runs
// Setup (the database is already in whatever state the prior process left
// it in); this only restores the bookkeeping so Reset/hints/diagnosis
// continue to work correctly against a challenge that was already active.
func (m *Manager) LoadPersisted(ctx context.Context) error {
	var raw []byte
	err := m.db.QueryRowContext(ctx, "SELECT payload FROM metrics WHERE id='challenge'").Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var p persistedState
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if p.State == "" || p.State == StateNone || p.ChallengeID == "" {
		return nil
	}
	c, ok := ByID(p.ChallengeID)
	if !ok {
		return nil
	}
	m.mu.Lock()
	m.challenge = c
	m.state = p.State
	m.startedAt = p.StartedAt
	m.hintsUsed = p.HintsUsed
	m.rootCause = p.RootCause
	m.fixApproach = p.FixApproach
	m.appliedVariant = p.AppliedVariant
	m.mu.Unlock()
	return nil
}

func (m *Manager) Active() (Challenge, State, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.challenge, m.state, m.state != StateNone
}

func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Start applies Setup (for DB-mechanism challenges) and arms the app-variant
// (for app-mechanism challenges — ActiveVariantID is what agent code checks
// to actually change behavior; Manager itself never touches agent code).
func (m *Manager) Start(ctx context.Context, id string) error {
	c, ok := ByID(id)
	if !ok {
		return fmt.Errorf("unknown challenge %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateNone {
		return fmt.Errorf("challenge %q is already active — reset it first", m.challenge.ID)
	}
	if c.Mechanism == MechanismDB {
		for _, stmt := range c.Setup {
			if _, err := m.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("setup: %w", err)
			}
		}
	}
	m.challenge = c
	m.state = StateActive
	m.startedAt = time.Now()
	m.hintsUsed = 0
	m.rootCause = ""
	m.fixApproach = ""
	m.appliedVariant = false
	m.persist()
	return nil
}

// Reset tears down the active challenge (Teardown SQL for DB-mechanism
// challenges; an app-mechanism challenge just stops being read the instant
// state goes back to StateNone) — safe to call with nothing active.
func (m *Manager) Reset(ctx context.Context) error {
	m.mu.Lock()
	c := m.challenge
	st := m.state
	m.mu.Unlock()
	if st == StateNone {
		return nil
	}
	if c.Mechanism == MechanismDB {
		for _, stmt := range c.Teardown {
			if _, err := m.db.ExecContext(ctx, stmt); err != nil && !alreadyUndone(err) {
				return fmt.Errorf("teardown: %w", err)
			}
		}
	}
	m.mu.Lock()
	m.challenge = Challenge{}
	m.state = StateNone
	m.persist()
	m.mu.Unlock()
	return nil
}

func (m *Manager) UnlockHint() (Hint, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateNone || m.hintsUsed >= len(m.challenge.Hints) {
		return Hint{}, false
	}
	h := m.challenge.Hints[m.hintsUsed]
	m.hintsUsed++
	m.persist()
	return h, true
}

// SetDiagnosisAnswers records the learner's selected answers for the two
// multiple-choice diagnosis questions (root cause, fix approach) — replacing
// an earlier free-text field that stored anything at all, which meant
// grading it required a human to read the prose and judge it against the
// actual performance result. Both ids are validated against the fixed,
// catalog-wide option pools; an unrecognized id is rejected outright rather
// than silently stored, since that closed vocabulary is what makes
// automatic grading possible. A challenge must be active.
func (m *Manager) SetDiagnosisAnswers(rootCause, fixApproach string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateNone {
		return fmt.Errorf("no challenge active")
	}
	if rootCause != "" && !validDiagOption(RootCauseOptions, rootCause) {
		return fmt.Errorf("unrecognized root cause option %q", rootCause)
	}
	if fixApproach != "" && !validDiagOption(FixApproachOptions, fixApproach) {
		return fmt.Errorf("unrecognized fix approach option %q", fixApproach)
	}
	m.rootCause = rootCause
	m.fixApproach = fixApproach
	m.persist()
	return nil
}

// DiagnosisAnswers returns the learner's currently selected answer ids
// (each "" if unanswered) — not whether they're correct; grading.go compares
// these against the active Challenge's own RootCause/FixApproach fields.
func (m *Manager) DiagnosisAnswers() (rootCause, fixApproach string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rootCause, m.fixApproach
}

// DiagnosisChoices returns the 4 radio-button choices for each of the
// active challenge's two diagnosis questions — nil, nil if no challenge is
// active. See diagnosisChoices for how the 4-of-N subset is picked.
func (m *Manager) DiagnosisChoices() (rootCause, fixApproach []DiagOption) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateNone {
		return nil, nil
	}
	return diagnosisChoices(RootCauseOptions, m.challenge.RootCause, m.challenge.ID+"|root"),
		diagnosisChoices(FixApproachOptions, m.challenge.FixApproach, m.challenge.ID+"|fix")
}

// ToggleVariant flips the "improved implementation" toggle for an
// app-mechanism challenge back and forth between the bad and reference
// behavior — turning it on is gated on at least 1 hint used and both
// diagnosis questions answered, per the written plan's §5.1 (this is the
// only "fix" mechanism app-only challenges have, since no learner SQL can
// reach their own Go code); turning it back off to compare against the bad
// implementation is always allowed once that gate has been cleared once.
// Answering isn't the same as answering correctly — correctness is graded
// separately in grading.go, not enforced at this gate, so a learner who
// guesses wrong can still apply the real fix and earn functional/
// performance credit; only the diagnosis points themselves depend on
// getting the two questions right.
func (m *Manager) ToggleVariant() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateNone {
		return fmt.Errorf("no challenge active")
	}
	if m.challenge.Mechanism != MechanismApp {
		return fmt.Errorf("challenge %q is fixed via SQL, not a variant toggle", m.challenge.ID)
	}
	if !m.appliedVariant && (m.hintsUsed == 0 || m.rootCause == "" || m.fixApproach == "") {
		return fmt.Errorf("use at least one hint and answer both diagnosis questions before applying the fix")
	}
	m.appliedVariant = !m.appliedVariant
	m.persist()
	return nil
}

// ActiveVariantID returns the active challenge's ID only while it's an
// app-mechanism challenge whose variant hasn't been fixed yet — this is
// exactly what agent code checks (via Engine.ActiveChallenge()) to decide
// whether to run its bad or reference behavior.
func (m *Manager) ActiveVariantID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateNone || m.challenge.Mechanism != MechanismApp || m.appliedVariant {
		return ""
	}
	return m.challenge.ID
}

func (m *Manager) MarkBaseline() {
	m.mu.Lock()
	if m.state == StateActive {
		m.state = StateBaseline
		m.persist()
	}
	m.mu.Unlock()
}

func (m *Manager) MarkGraded() {
	m.mu.Lock()
	if m.state == StateBaseline || m.state == StateGraded {
		m.state = StateGraded
		m.persist()
	}
	m.mu.Unlock()
}

func (m *Manager) HintsUsed() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hintsUsed
}

func (m *Manager) AppliedVariant() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appliedVariant
}

func (m *Manager) StartedAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startedAt
}

// alreadyUndone recognizes the handful of MySQL errors that mean "the
// learner already fixed this themselves before clicking Reset" — a
// Teardown statement re-creating an index/table the learner already
// restored (1061 duplicate key name, 1050 table already exists) or
// dropping something the learner already removed (1091 can't drop, doesn't
// exist) isn't a real failure, it's Teardown finding the state it wanted
// already there. Found live (stage S4 verification): fixing
// idx-price-history by hand and then clicking Reset failed outright before
// this, because Teardown's CREATE INDEX collided with the index the
// learner had just recreated.
func alreadyUndone(err error) bool {
	var me *mysqldriver.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	switch me.Number {
	case 1061, 1050, 1091, 1826: // dup keyname, table exists, can't drop/doesn't exist, dup FK constraint name
		return true
	}
	return false
}

// RunFunctionalCheck runs the active challenge's own FunctionalCheck (if it
// has one) — used by grading, and by Start itself isn't appropriate to call
// automatically here since a check right after Setup is expected to fail
// (that's the whole point of Setup).
func (m *Manager) RunFunctionalCheck(ctx context.Context) string {
	m.mu.Lock()
	c := m.challenge
	m.mu.Unlock()
	if c.FunctionalCheck == nil {
		return ""
	}
	return c.FunctionalCheck(ctx, m.db)
}
