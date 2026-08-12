// Package store is the one place in this app that knows what a database is.
//
// Unlike its five sibling sims — each hard-wired to a single engine — Stock
// Market Sim speaks MySQL, PostgreSQL, MongoDB or Valkey, chosen at deploy
// time. That is what the Store interface below buys: internal/sim and
// internal/api are written once against it, import no driver, and cannot tell
// which backend they are running on beyond the string Engine() returns.
//
// # The namespace-isolation rule
//
// Every object this app creates lives under exactly one name — the schema
// (MySQL/PostgreSQL), the database (MongoDB), or the key prefix (Valkey) given
// by Config.Database, default "stocksim". This is a hard rule, not a
// convention, and it matters more here than in any sibling: a Stock Market Sim
// node can be pointed at a database dbcanvas does not own and did not create
// (see the manual connection mode in app/stocksim.go). Wipe and DropSchema are
// therefore scoped to that one name and nothing else — never the server, never
// a neighbouring schema, and never a reserved system namespace, which Open
// refuses outright.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Engine identifiers. These are exactly the values DB_ENGINE may carry, and
// exactly the values Engine() may return.
const (
	EngineMySQL    = "mysql"
	EnginePostgres = "postgres"
	EngineMongoDB  = "mongodb"
	EngineValkey   = "valkey"
)

// DefaultDatabase is the schema/database/key-prefix used when none is given.
const DefaultDatabase = "stocksim"

// DefaultPort returns the well-known port for an engine, used when a manually
// configured connection leaves the port blank.
func DefaultPort(engine string) int {
	switch engine {
	case EngineMySQL:
		return 3306
	case EnginePostgres:
		return 5432
	case EngineMongoDB:
		return 27017
	case EngineValkey:
		return 6379
	}
	return 0
}

// reservedDatabases are names this app must never claim as its own namespace.
// Creating tables in one would scatter application data through a system
// catalogue; dropping one would be catastrophic and, on an external server,
// not ours to do. Open treats a match as a fatal misconfiguration rather than
// a warning — there is no correct way to continue.
var reservedDatabases = map[string]bool{
	"mysql": true, "sys": true, "information_schema": true, "performance_schema": true,
	"postgres": true, "template0": true, "template1": true,
	"admin": true, "local": true, "config": true,
}

// Config is the resolved connection description, assembled from environment
// variables by main.go. Exactly one of DSN or the host/user/password fields is
// authoritative: a non-empty DSN is passed to the driver verbatim and wins over
// everything else, which is what lets an unusual managed-cloud connection
// string work without this app needing to model its dialect.
type Config struct {
	Engine   string
	DSN      string // full driver-native DSN/URI; when set, beats the fields below
	Host     string
	Port     int
	User     string
	Password string
	Database string // schema / database / key prefix — the namespace we own
	TLS      string // "disable" | "prefer" | "require"
	Params   string // extra driver params, appended verbatim
	// Threads is how many concurrent workers each of the simulation's two heavy
	// agents runs — see PoolSize for what that means for the connection pool,
	// and sim.Engine.Threads for what the agents do with it. 0 takes
	// DefaultThreads.
	Threads int
}

// Thread-count bounds. DefaultThreads is what the sim ran at before the count
// was configurable, so an untouched deployment behaves exactly as it did.
// MaxThreads is not a limit anyone should reach; it exists so a mistyped
// "1000" opens a thousand connections to nobody's database.
const (
	DefaultThreads = 4
	MaxThreads     = 64
)

// ClampThreads normalises a requested thread count.
func ClampThreads(n int) int {
	switch {
	case n <= 0:
		return DefaultThreads
	case n > MaxThreads:
		return MaxThreads
	}
	return n
}

// PoolSize is how many connections a pool may open for a given thread count.
//
// Both heavy agents can be running at once — the backfill writing history while
// the working-set agent reads it — so the two together want 2×Threads
// connections, and starving either of them would quietly cap the load the user
// asked for. The constant on top is headroom for everything else: the seven
// light agents, the dashboard's own reads, and whatever CRUD a person is doing
// by hand while all this is going on. The floor is for the other direction: at
// one or two threads the headroom alone would be most of the pool, and 16 is
// what this app ran on before any of it was configurable.
func (c Config) PoolSize() int {
	if n := 2*ClampThreads(c.Threads) + 12; n > 16 {
		return n
	}
	return 16
}

// Validate rejects a configuration that cannot safely be used, before any
// connection is attempted.
func (c Config) Validate() error {
	switch c.Engine {
	case EngineMySQL, EnginePostgres, EngineMongoDB, EngineValkey:
	case "":
		return fmt.Errorf("DB_ENGINE is required (one of %s, %s, %s, %s)",
			EngineMySQL, EnginePostgres, EngineMongoDB, EngineValkey)
	default:
		return fmt.Errorf("unsupported DB_ENGINE %q (want one of %s, %s, %s, %s)",
			c.Engine, EngineMySQL, EnginePostgres, EngineMongoDB, EngineValkey)
	}
	if c.DSN == "" && c.Host == "" {
		return fmt.Errorf("no target: set a DSN, or a host to connect to")
	}
	if reservedDatabases[strings.ToLower(strings.TrimSpace(c.Database))] {
		return fmt.Errorf(
			"refusing to use %q as this application's namespace: it is a reserved system database. "+
				"Choose another name (the default is %q)", c.Database, DefaultDatabase)
	}
	if c.Engine == EngineValkey && strings.TrimSpace(c.Database) == "" {
		// A blank prefix would make DropSchema's SCAN pattern "*", i.e. the
		// entire keyspace of a server we may not own. Never allow it.
		return fmt.Errorf("a non-empty key prefix is required for Valkey")
	}
	return nil
}

// Store is everything the simulation engine and the HTTP API need from a
// database. One implementation per engine; see mysql.go and its siblings.
//
// Implementations must treat a nil/absent row as (zero value, ErrNotFound)
// rather than a driver-specific sentinel, so handlers can map it to a 404
// without knowing the backend.
type Store interface {
	// Identity and liveness.
	Engine() string
	Ping(ctx context.Context) error
	ServerVersion(ctx context.Context) (string, error)
	Database() string
	// Location describes, in words, where on the server this app's objects
	// actually are — "database x", "schema y inside database x", "keys under
	// prefix z". Database() names the namespace the user asked for; this says
	// what the server made of that request, which is not always the same thing
	// and is otherwise only discoverable by going and looking.
	Location() string
	Close() error

	// Namespace lifecycle. EnsureSchema is idempotent and safe to call on every
	// start; Wipe empties this app's objects but leaves them in place;
	// DropSchema removes them entirely. All three are scoped to Database().
	EnsureSchema(ctx context.Context) error
	Wipe(ctx context.Context) error
	DropSchema(ctx context.Context) error
	Objects(ctx context.Context) ([]ObjectInfo, error)

	// Securities CRUD.
	ListSecurities(ctx context.Context, q ListQuery) ([]Security, int, error)
	GetSecurity(ctx context.Context, id string) (Security, error)
	CreateSecurity(ctx context.Context, s Security) (Security, error)
	UpdateSecurity(ctx context.Context, s Security) (Security, error)
	DeleteSecurity(ctx context.Context, id string) error

	// Portfolios CRUD.
	ListPortfolios(ctx context.Context, q ListQuery) ([]Portfolio, int, error)
	GetPortfolio(ctx context.Context, id string) (Portfolio, error)
	CreatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error)
	UpdatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error)
	DeletePortfolio(ctx context.Context, id string) error

	// Orders CRUD.
	ListOrders(ctx context.Context, q ListQuery) ([]Order, int, error)
	GetOrder(ctx context.Context, id string) (Order, error)
	CreateOrder(ctx context.Context, o Order) (Order, error)
	UpdateOrder(ctx context.Context, o Order) (Order, error)
	DeleteOrder(ctx context.Context, id string) error

	// Simulation writes.
	AppendTicks(ctx context.Context, ticks []Tick) error
	// TicksBefore reads one security's price history at or before at,
	// oldest-first so a sparkline can be drawn straight through it. A zero at
	// means "the newest there are", which is what the dashboard asks for; any
	// other value is a random dive into history, which is what the working-set
	// agent asks for (see sim/workingset.go).
	TicksBefore(ctx context.Context, securityID string, at time.Time, limit int) ([]Tick, error)
	// TickSpan reports the oldest and newest tick timestamps held for one
	// security. Both are zero when it has no history yet. This is how the
	// working-set agent sizes the slice of the dataset it keeps hot, so it must
	// stay an indexed lookup rather than a scan — see each implementation.
	TickSpan(ctx context.Context, securityID string) (oldest, newest time.Time, err error)
	OpenOrders(ctx context.Context, limit int) ([]Order, error)
	// RecordFill settles one order atomically where the engine supports it:
	// the order moves to filled, the trade is written, the holding is upserted
	// and the portfolio's cash moves. Implementations that cannot do this in
	// one transaction say so in their own doc comment.
	RecordFill(ctx context.Context, o Order, t Trade) error
	ListHoldings(ctx context.Context, portfolioID string) ([]Holding, error)
	ApplyQuotes(ctx context.Context, quotes []Quote) error
	CountOrdersByStatus(ctx context.Context) (map[string]int64, error)
	RecentTrades(ctx context.Context, limit int) ([]Trade, error)
	// TradeTotals is the all-time trade count and share volume, which the
	// report needs and RecentTrades (deliberately capped) cannot supply.
	TradeTotals(ctx context.Context) (count, volume int64, err error)

	// Shared blob / heartbeat / event idiom, identical to the sibling sims.
	PutMetrics(ctx context.Context, id string, payload any) error
	GetMetrics(ctx context.Context, id string) (json.RawMessage, error)
	PutState(ctx context.Context, id string, payload any) error
	GetState(ctx context.Context, id string) (json.RawMessage, error)
	Heartbeat(ctx context.Context, agent, status, detail string) error
	AllHeartbeats(ctx context.Context) ([]AgentHeartbeat, error)
	AppendEvent(ctx context.Context, e Event) error
	EventsSince(ctx context.Context, afterID string, limit int) ([]Event, error)

	// Report.
	ReportData(ctx context.Context, limitTrades int) (Report, error)
}

// Quote is a price update for one security, applied in bulk by the price
// agent. Kept separate from Security so a quote write only ever touches the
// four price/volume columns — a user concurrently editing a security's name or
// sector through the CRUD API never has that edit clobbered by a tick landing
// a millisecond later.
type Quote struct {
	SecurityID string
	Price      float64
	Volume     int64
	TS         time.Time
}

// ErrNotFound is returned by every Get*/Update*/Delete* when the id does not
// exist, so handlers can answer 404 without engine-specific error matching.
var ErrNotFound = fmt.Errorf("not found")

// ErrConflict is returned when a uniqueness rule would be violated — today
// only a duplicate security symbol.
var ErrConflict = fmt.Errorf("already exists")

// Open validates cfg and returns the Store implementation for cfg.Engine.
// Engines not yet implemented return a clear error naming what is supported,
// rather than a nil Store the caller would have to guard against.
func Open(ctx context.Context, cfg Config) (Store, error) {
	if cfg.Database == "" {
		cfg.Database = DefaultDatabase
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort(cfg.Engine)
	}
	switch cfg.Engine {
	case EngineMySQL:
		return openMySQL(ctx, cfg)
	case EnginePostgres:
		return openPostgres(ctx, cfg)
	case EngineMongoDB:
		return openMongo(ctx, cfg)
	case EngineValkey:
		return openValkey(ctx, cfg)
	}
	return nil, fmt.Errorf("unsupported engine %q", cfg.Engine)
}

// Implemented reports which engines this build can actually open. The node
// form in dbcanvas offers exactly this set, so a user can never configure a
// target the binary would then refuse at startup.
func Implemented() []string {
	return []string{EngineMySQL, EnginePostgres, EngineMongoDB, EngineValkey}
}

// CanGrowToSize reports whether an engine's dataset can be driven to an
// arbitrary size by writing to it — which is what the backfill agent needs and
// what makes a size target meaningful.
//
// It is false for exactly one engine, and for a reason worth stating: Valkey's
// tick history is an XADD stream capped at MaxLen 500 per security (see
// AppendTicks in valkey.go), so writing to it harder does not make it bigger,
// it just rolls entries off the far end sooner. Uncapping it would not be a
// disk test either — it would be a memory test, ending in the container being
// OOM-killed somewhere short of the target. dbcanvas therefore offers no size
// target for a Valkey-backed node rather than offering one that cannot be met.
func CanGrowToSize(engine string) bool {
	return engine != EngineValkey
}
