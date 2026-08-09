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
	RecentTicks(ctx context.Context, securityID string, limit int) ([]Tick, error)
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
