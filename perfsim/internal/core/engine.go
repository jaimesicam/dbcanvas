package core

import (
	"context"
	"time"
)

// Engine is everything Performance Sim needs from one database family. The MySQL
// family is the first (internal/mysqlx); the others implement the same methods.
type Engine interface {
	// Init creates the sim's own schema (heartbeat, workload table, saved state)
	// and learns the topology. It is retried until it succeeds.
	Init(ctx context.Context) error
	Info() TargetInfo

	// Heartbeat writes the current time on the writer. Lag is how far behind
	// that write each member's copy is, which measures every kind of lag the same
	// way: a stopped applier, a delayed replica, a Galera node, a GR secondary.
	Heartbeat(ctx context.Context) error
	// Sample reads every member once. It must return within about a second even
	// when a member is stuck; a member that does not answer is reported down.
	Sample(ctx context.Context) ([]Member, ServerSample)

	Dials() []Dial
	Start(ctx context.Context, id string, p Params, targets []string, emit func(level, text string)) (Running, error)

	// Op runs one statement of the steady workload against the writer.
	Op(ctx context.Context, write bool, n int) error

	Datasets(ctx context.Context) ([]Dataset, error)
	Prepare(ctx context.Context, name string, bytes int64, pk bool, progress func(float64)) error
	Drop(ctx context.Context, name string) error

	// Sessions lists what is running on each member. held marks the connections
	// the dials keep open, per member.
	Sessions(ctx context.Context, held map[string][]string) ([]Session, error)

	// Save and Load keep small JSON documents (settings, recorded runs) on the
	// target, so they survive a redeploy of the sim.
	Save(ctx context.Context, key string, v []byte) error
	Load(ctx context.Context, prefix string) (map[string][]byte, error)
	Delete(ctx context.Context, key string) error

	Close()
}

// sampleTimeout bounds one Sample call.
const sampleTimeout = 900 * time.Millisecond
