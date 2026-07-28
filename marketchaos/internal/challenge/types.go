// Package challenge is the catalog of deliberately-injected problems a
// learner diagnoses and fixes — the "Unoptimized MySQL Challenge" part of
// MarketChaos. Every challenge fixes into one of two mechanisms:
//
//   - DB-fixable: Setup runs real DDL/index changes against this app's own
//     schema (or, for the one PXC challenge where touching a live table's
//     primary key would be genuinely risky, against an isolated scratch
//     table created just for that challenge). The learner's fix is real SQL
//     they run through DBCanvas's own Terminal/Query Runner — this app never
//     executes their fix, it only measures the database state before/after.
//   - App-variant: the "bad" behavior is something a live agent's own Go
//     code does (a query shape, a transaction's shape, which member it talks
//     to) that no amount of learner SQL could touch. Per the written plan's
//     §5.1, the learner's "fix" for these is a toggle in this app's own
//     challenge panel ("apply the improved implementation"), unlocked after
//     a hint tier and a diagnosis — never raw SQL from this app's side,
//     since app code isn't something Terminal/Query Runner can reach either.
//
// A challenge is never "on" by default — Start applies Setup (or arms an
// app-variant), Reset (via the challenge manager, not Engine.Reset) removes
// it. Exactly one challenge is active at a time.
package challenge

import (
	"context"
	"database/sql"
)

type Category string

const (
	CategoryIndexing     Category = "indexing"
	CategoryQueryRewrite Category = "query-rewrite"
	CategoryJoins        Category = "joins"
	CategorySorting      Category = "sorting"
	CategoryNPlusOne     Category = "n-plus-one"
	CategoryDML          Category = "dml"
	CategoryLocking      Category = "locking"
	CategorySchema       Category = "schema"
	CategoryPXC          Category = "pxc"
)

type Difficulty string

const (
	Beginner     Difficulty = "beginner"
	Intermediate Difficulty = "intermediate"
)

// Mechanism distinguishes how a challenge's "bad" state is injected and
// how the learner is expected to fix it — see the package doc comment.
type Mechanism string

const (
	MechanismDB  Mechanism = "db"  // Setup/Teardown SQL; learner fixes via their own SQL
	MechanismApp Mechanism = "app" // an agent's own code branches on this challenge being active; learner fixes via the challenge panel's variant toggle
)

type Hint struct {
	Tier int    `json:"tier"`
	Text string `json:"text"`
}

// Challenge is one catalog entry — a Go struct literal, not embedded YAML
// (see the written plan's §5.1 for why: this repo's own precedent,
// app/labs.go, is plain Go data too, and half of every challenge here is
// inherently behavioral — functional checks and Setup/Teardown are Go funcs,
// not data a YAML file could carry without still needing a matching Go
// implementation on the other end).
type Challenge struct {
	ID         string
	Title      string
	Category   Category
	Difficulty Difficulty
	Mechanism  Mechanism

	// RequiresFamily gates a challenge to a target family — "" means any
	// target, "pxc" means the PXC-specific pack (pxcnode/pxc/haproxy-pxc).
	RequiresFamily string

	Scenario string // narrative: what's supposedly happened
	Symptom  string // what the learner will actually observe on the dashboard

	// Setup/Teardown are only used when Mechanism==MechanismDB — real SQL
	// statements run against the target. Teardown must exactly undo Setup;
	// ChallengeManager.Reset relies on that, not on ChallengeManager trying
	// to infer the original state.
	Setup    []string
	Teardown []string

	// ShapeIDs are the leaderboard shape IDs (see internal/sim/shapes.go)
	// this challenge's grading watches for its performance comparison.
	ShapeIDs []string

	Hints []Hint

	// FunctionalCheck runs after Setup (and again during grading) — returns
	// a human-readable failure reason, or "" if the check passes. Every
	// challenge gets the engine's own always-on invariants too (see
	// internal/sim/invariants.go) — this is ADDITIONAL, challenge-specific
	// verification, e.g. "the index this challenge is about actually
	// exists again."
	FunctionalCheck func(ctx context.Context, db *sql.DB) string

	// MaxNewIndexes bounds the regression check's tolerance for new indexes
	// added while this challenge is active — 0 uses the grading engine's
	// own default (2).
	MaxNewIndexes int
}
