package main

// Database Explorer — the shared model.
//
// This file holds the vocabulary every engine adapter speaks: the connection a user
// picked, the capabilities that connection has, the tree nodes the object browser
// draws, and the one result envelope the frontend renders whatever produced it.
//
// The separation that matters here is between what DBCanvas knows and what the
// browser is told. DBCanvas provisioned every database on this page, so it already
// holds the hostname, the port, the account and the password. The browser is given a
// connection *id* and nothing else: every request re-resolves that id against the
// caller's own stacks, and the credentials never leave the process. There is no
// endpoint on this feature that accepts a host, a user or a password — a target
// either resolves to a deployment this user may reach, or the request fails.
//
// See docs/DATABASE_EXPLORER.md, and dbexplorer_api.go for the handlers.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The engine families. A family is a wire protocol and a set of metadata queries,
// not a product: Percona Server, PXC, MariaDB and MySQL Community are one family,
// and Patroni, repmgr and Spock are one with PostgreSQL. These are the same four
// names the rest of the app uses (engineForType / scEngineOf), plus ClickHouse,
// which arrives with PMM.
const (
	dexMySQL      = "mysql"
	dexPostgres   = "postgres"
	dexMongoDB    = "mongodb"
	dexValkey     = "valkey"
	dexClickHouse = "clickhouse"
)

// dexCaps is what a connection can do, which is what the frontend adapts to. A
// capability is not a preference: it says whether a panel would work at all, so
// the UI can leave out an Explain tab rather than offering one that errors.
type dexCaps struct {
	SQL           bool `json:"sql"`
	Documents     bool `json:"documents"`
	KeyValue      bool `json:"keyValue"`
	Explain       bool `json:"explain"`
	Transactions  bool `json:"transactions"`
	EditableRows  bool `json:"editableRows"`
	SchemaBrowser bool `json:"schemaBrowser"`
	QueryCancel   bool `json:"queryCancel"`
	Charts        bool `json:"charts"`
	// Schemas says there is a schema level between a database and its objects.
	// PostgreSQL has one; MySQL and ClickHouse do not, and their trees are a level
	// shallower because of it.
	Schemas bool `json:"schemas"`
	// MultiResult says one submission can return several result sets.
	MultiResult bool `json:"multiResult"`
}

// dexConnection is one endpoint as the browser sees it. Note what is absent: there
// is no password field, and User is the account name only — shown so a person can
// tell which identity a query will run as, which is a fact about the connection and
// not a credential.
type dexConnection struct {
	ID        string `json:"id"`
	StackID   int64  `json:"stackId"`
	StackName string `json:"stackName"`
	NodeID    string `json:"nodeId"`
	Label     string `json:"label"`
	Engine    string `json:"engine"`
	Kind      string `json:"kind"`
	Product   string `json:"product"`
	Version   string `json:"version,omitempty"`
	// Group is the heading this connection sits under in the tree ("MySQL / PXC",
	// "PostgreSQL", "MongoDB", "Valkey", "PMM Server"). Derived, so a stack with
	// two PXC clusters and a Patroni still reads as three groups and not eight
	// nodes in a list.
	Group string `json:"group"`
	Role  string `json:"role"`
	// Preferred marks the endpoint a client should normally use — HAProxy's write
	// port rather than a PXC member, mongos rather than a shard, the replica set
	// rather than one of its members. The individual members are still offered,
	// because connecting to one on purpose is most of what a lab is for.
	Preferred bool     `json:"preferred"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Hosts     []string `json:"hosts,omitempty"`
	Status    string   `json:"status"`
	ReadOnly  bool     `json:"readOnly"`
	// Unlockable says this connection is read-only *by policy* rather than by
	// nature, and that an administrator has allowed that policy to be lifted. The
	// browser uses it to offer the arming control; it is never itself permission to
	// write — every write is re-checked server-side against the setting AND against
	// the request having asked (dexQueryRequest.AllowWrites).
	Unlockable bool    `json:"unlockable,omitempty"`
	Policy     string  `json:"policy,omitempty"`
	Warning    string  `json:"warning,omitempty"`
	Note       string  `json:"note,omitempty"`
	Caps       dexCaps `json:"capabilities"`
	User       string  `json:"user,omitempty"`
	// Exposable says this is a Kubernetes endpoint with no address of its own that
	// could be given one, and ExposedBy names the companion Service if it already
	// has. Together they are what the tree's "Expose for tools" control reads.
	Exposable bool   `json:"exposable,omitempty"`
	ExposedBy string `json:"exposedBy,omitempty"`
	// Transport says how DBCanvas reaches this database: "network" (a driver over
	// the stack's Docker network) or "exec" (a client run inside the container,
	// because the server listens on loopback only — which is PMM's case).
	Transport string `json:"transport"`
}

// dexConnGroup is one stack's connections, grouped for the tree.
type dexConnGroup struct {
	StackID     int64           `json:"stackId"`
	StackName   string          `json:"stackName"`
	Groups      []dexEngineList `json:"groups"`
	Connections int             `json:"connections"`
}

type dexEngineList struct {
	Name        string          `json:"name"`
	Engine      string          `json:"engine"`
	Connections []dexConnection `json:"connections"`
}

// ---------------------------------------------------------------- connection ids

// A connection id is the only handle the browser ever holds. It is a token this
// process can re-resolve, not a description of a server: it names a stack, a shape,
// and the node or frame the shape hangs off, and every field of the real connection
// — address, port, account, password, TLS — is looked up again from the deployment
// on each request. That is why an id is safe to put in a URL and why substituting
// somebody else's stack id in one gets a 403 rather than their database.
//
// "~" separates the parts: stack ids are decimal, and node/frame ids are canvas
// tokens (base-36 timestamps) that never contain it.
const dexIDSep = "~"

// The shapes an id can name.
const (
	dexShapeNode     = "n"   // a database node, exactly as deployed
	dexShapeAIO      = "aio" // one instance inside an All-in-One node
	dexShapeHAProxy  = "hap" // an HAProxy port in front of a cluster
	dexShapePgBounce = "pgb" // a PgBouncer pool in front of a PostgreSQL backend
	dexShapeProxySQL = "psq" // a ProxySQL client port
	dexShapeRouter   = "rtr" // a MySQL Router port on a Group Replication member
	dexShapeReplSet  = "rs"  // a MongoDB replica set as a whole
	dexShapeVKC      = "vkc" // a Valkey cluster as a whole
	dexShapePMMPG    = "ppg" // PMM's internal PostgreSQL
	dexShapePMMCH    = "pch" // PMM's ClickHouse (Query Analytics)
	dexShapeK8s      = "k8s" // a database an operator deployed inside a K3D frame
)

// dexRef is a parsed connection id.
type dexRef struct {
	StackID int64
	Shape   string
	// Target is the node or frame the shape hangs off: the node itself, the
	// HAProxy node, the frame of a replica set, the PMM node.
	Target string
	// Extra qualifies the shape — an All-in-One instance name, "write"/"read" for
	// an HAProxy port, "rw"/"ro" for a router port, "pool"/"pool-ro" for a
	// PgBouncer pool.
	Extra string
}

func (r dexRef) String() string {
	s := strconv.FormatInt(r.StackID, 10) + dexIDSep + r.Shape + dexIDSep + r.Target
	if r.Extra != "" {
		s += dexIDSep + r.Extra
	}
	return s
}

// dexParseID parses a connection id. It validates shape only — whether the caller
// may reach what it names is decided later, by re-resolving it against their own
// stacks (dexResolve). An id is never trusted for anything beyond "this is what was
// asked for".
func dexParseID(id string) (dexRef, error) {
	parts := strings.Split(id, dexIDSep)
	if len(parts) < 3 || len(parts) > 4 {
		return dexRef{}, fmt.Errorf("malformed connection id")
	}
	sid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || sid <= 0 {
		return dexRef{}, fmt.Errorf("malformed connection id")
	}
	switch parts[1] {
	case dexShapeNode, dexShapeAIO, dexShapeHAProxy, dexShapePgBounce, dexShapeProxySQL,
		dexShapeRouter, dexShapeReplSet, dexShapeVKC, dexShapePMMPG, dexShapePMMCH,
		dexShapeK8s:
	default:
		return dexRef{}, fmt.Errorf("malformed connection id")
	}
	if !dexSafeSegment(parts[2]) {
		return dexRef{}, fmt.Errorf("malformed connection id")
	}
	r := dexRef{StackID: sid, Shape: parts[1], Target: parts[2]}
	if len(parts) == 4 {
		if !dexSafeSegment(parts[3]) {
			return dexRef{}, fmt.Errorf("malformed connection id")
		}
		r.Extra = parts[3]
	}
	return r, nil
}

// dexSafeSegment bounds what a target or a qualifier may contain. Every value that
// legitimately lands here is a canvas node/frame id, a DNS label, or one of a handful
// of fixed words, so the charset is narrow — and narrowing it means a segment can
// never be a path, a traversal or an argument, whatever it is later concatenated
// into. Ownership is still checked separately; this only stops a malformed id from
// being carried any further than the parser.
func dexSafeSegment(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	// "." is legal inside a name but a segment that is only dots is a path.
	return strings.Trim(s, ".") != ""
}

// ---------------------------------------------------------------- the object tree

// dexNodeKind is what a tree node is, which decides its icon and which actions its
// context menu offers.
const (
	dexKindDatabase   = "database"
	dexKindSchema     = "schema"
	dexKindTable      = "table"
	dexKindView       = "view"
	dexKindMatView    = "matview"
	dexKindSequence   = "sequence"
	dexKindFunction   = "function"
	dexKindProcedure  = "procedure"
	dexKindTrigger    = "trigger"
	dexKindDictionary = "dictionary"
	dexKindCollection = "collection"
	dexKindKey        = "key"
	dexKindKeyspace   = "keyspace" // a Valkey logical database or cluster
	dexKindFolder     = "folder"   // a grouping level with no object of its own
)

// dexTreeNode is one row in the object browser. Children are never included: the
// tree is expanded lazily, one level per request, because a database with ten
// thousand tables should cost one click and not one page load (see ListObjects).
type dexTreeNode struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Detail   string `json:"detail,omitempty"`
	Badge    string `json:"badge,omitempty"`
	Rows     *int64 `json:"rows,omitempty"`
	Bytes    *int64 `json:"bytes,omitempty"`
	System   bool   `json:"system,omitempty"`
	HasChild bool   `json:"hasChildren"`
	// Folder is the group this object belongs under ("Tables", "Views", …), so one
	// listing call can fill several folders at once.
	Folder string `json:"folder,omitempty"`
}

// dexObjectPage is one page of a listing. Cursor is opaque and engine-specific —
// Valkey's is a SCAN cursor, everything else's is empty because a database's object
// list is bounded.
type dexObjectPage struct {
	Nodes   []dexTreeNode `json:"nodes"`
	Cursor  string        `json:"cursor,omitempty"`
	More    bool          `json:"more"`
	Folders []string      `json:"folders,omitempty"`
}

// dexObjectRef names one object to describe.
type dexObjectRef struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// dexColumnInfo is one column of a table, as the Columns tab shows it.
type dexColumnInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Default  string `json:"default,omitempty"`
	Key      string `json:"key,omitempty"`
	Extra    string `json:"extra,omitempty"`
	Comment  string `json:"comment,omitempty"`
	Position int    `json:"position"`
}

type dexIndexInfo struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
	Primary bool     `json:"primary"`
	Type    string   `json:"type,omitempty"`
	Size    *int64   `json:"size,omitempty"`
	Partial string   `json:"partial,omitempty"`
}

type dexConstraintInfo struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Columns    []string `json:"columns,omitempty"`
	Definition string   `json:"definition,omitempty"`
}

type dexForeignKeyInfo struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	RefSchema  string   `json:"refSchema,omitempty"`
	RefTable   string   `json:"refTable"`
	RefColumns []string `json:"refColumns"`
	OnUpdate   string   `json:"onUpdate,omitempty"`
	OnDelete   string   `json:"onDelete,omitempty"`
}

// dexObjectDetail is everything the Overview / Columns / Indexes / DDL tabs draw.
// Props is the engine-specific half — ClickHouse's engine and sorting key, MongoDB's
// document count and storage size — kept as ordered label/value pairs so an adapter
// can say something true about its own object without the model having to grow a
// field for it.
type dexObjectDetail struct {
	Ref         dexObjectRef        `json:"ref"`
	Kind        string              `json:"kind"`
	Columns     []dexColumnInfo     `json:"columns,omitempty"`
	Indexes     []dexIndexInfo      `json:"indexes,omitempty"`
	Constraints []dexConstraintInfo `json:"constraints,omitempty"`
	ForeignKeys []dexForeignKeyInfo `json:"foreignKeys,omitempty"`
	Triggers    []dexTreeNode       `json:"triggers,omitempty"`
	PrimaryKey  []string            `json:"primaryKey,omitempty"`
	RowEstimate *int64              `json:"rowEstimate,omitempty"`
	Bytes       *int64              `json:"bytes,omitempty"`
	DDL         string              `json:"ddl,omitempty"`
	Props       []dexProp           `json:"props,omitempty"`
	// Editable says a row in this object can be identified uniquely, which is the
	// precondition for the grid offering edit and delete. False leaves the grid
	// read-only rather than guessing at a WHERE clause — see dexRowIdentity.
	Editable   bool   `json:"editable"`
	EditReason string `json:"editReason,omitempty"`
}

type dexProp struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ---------------------------------------------------------------- results

// The semantic types. They exist so the grid can right-align a number, render a
// boolean as a mark, pretty-print JSON and show a binary value as a length and a
// hex preview — without the frontend having to know that MySQL spells an integer
// BIGINT UNSIGNED and ClickHouse spells it UInt64.
const (
	dexSemString   = "string"
	dexSemNumber   = "number"
	dexSemInteger  = "integer"
	dexSemBool     = "boolean"
	dexSemDateTime = "datetime"
	dexSemDate     = "date"
	dexSemTime     = "time"
	dexSemJSON     = "json"
	dexSemBinary   = "binary"
	dexSemArray    = "array"
	dexSemDocument = "document"
	dexSemUnknown  = "unknown"
)

type dexColumn struct {
	Name         string `json:"name"`
	DatabaseType string `json:"databaseType,omitempty"`
	SemanticType string `json:"semanticType"`
}

// Result-set kinds. A submission can produce more than one, and they are not all
// rows: an UPDATE produces an affected count, an EXPLAIN produces a plan, a Valkey
// command produces a typed reply.
const (
	dexSetRows      = "rows"
	dexSetAffected  = "affected"
	dexSetExplain   = "explain"
	dexSetDocuments = "documents"
	dexSetReply     = "reply"
)

// dexResultSet is one result. Rows are positional — [][]any in column order — rather
// than objects, because a SQL result may repeat a column name and an object would
// silently drop one of them.
//
// A cell is a JSON null for SQL NULL, a string, a number, a bool, or one of the
// wrapper objects below for a value JSON cannot carry honestly (binary, or a number
// too wide for a float64).
type dexResultSet struct {
	Kind         string      `json:"kind"`
	Columns      []dexColumn `json:"columns,omitempty"`
	Rows         [][]any     `json:"rows,omitempty"`
	RowCount     int         `json:"rowCount"`
	AffectedRows int64       `json:"affectedRows"`
	InsertID     int64       `json:"insertId,omitempty"`
	Truncated    bool        `json:"truncated"`
	Statement    string      `json:"statement,omitempty"`
	Message      string      `json:"message,omitempty"`
	// Documents carries MongoDB results in their own shape, alongside the flattened
	// Rows the table view uses. Destroying the nesting to fit a grid would make the
	// document view impossible, so both travel.
	Documents []json.RawMessage `json:"documents,omitempty"`
	// Payload is the engine-specific structured reply — a Valkey typed value, a
	// PostgreSQL JSON plan — kept whole for the viewer that understands it.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// dexBinary is how a byte string reaches the browser: never as mojibake, and never
// silently truncated into something that looks like text.
type dexBinary struct {
	Marker string `json:"__dbx"` // always "binary"
	Base64 string `json:"b64"`
	Len    int    `json:"len"`
	Hex    string `json:"hex,omitempty"` // first bytes, for the grid preview
}

// dexBigNum carries an integer that does not survive a float64 — MySQL's BIGINT
// UNSIGNED and ClickHouse's UInt64/Int128 both exceed it, and JSON.parse would
// round them. The grid renders Text and offers the exact value on copy.
type dexBigNum struct {
	Marker string `json:"__dbx"` // always "bignum"
	Text   string `json:"text"`
}

// dexError is a database error, presented. Every field is something a database
// actually told us: nothing here is a Go error string dressed up, and nothing here
// can contain a credential — the adapters build these from the driver's own
// structured error, and the message is the server's.
type dexError struct {
	Engine    string  `json:"engine"`
	Message   string  `json:"message"`
	Code      string  `json:"code,omitempty"`     // MySQL 1146, ClickHouse 47, Mongo 13
	Name      string  `json:"name,omitempty"`     // Mongo "Unauthorized", CH "UNKNOWN_TABLE"
	SQLState  string  `json:"sqlState,omitempty"` // 42S02 / 42P01
	Position  int     `json:"position,omitempty"` // 1-based character offset into the statement
	Detail    string  `json:"detail,omitempty"`
	Hint      string  `json:"hint,omitempty"`
	ElapsedMs float64 `json:"elapsedMs"`
	// Display is the one line a client would have printed, assembled from the parts
	// above so the panel reads like a database client and not like a stack trace.
	Display string `json:"display,omitempty"`
}

func (e *dexError) Error() string {
	if e == nil {
		return ""
	}
	if e.Display != "" {
		return e.Display
	}
	return e.Message
}

// dexResult is one submission's whole outcome.
type dexResult struct {
	Engine     string         `json:"engine"`
	Sets       []dexResultSet `json:"sets"`
	DurationMs float64        `json:"durationMs"`
	Warnings   []string       `json:"warnings,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Error      *dexError      `json:"error,omitempty"`
	Limit      int            `json:"limit"`
	// ReadOnly records that the connection refused to consider anything but a read,
	// so the UI can say why a statement was rejected before it was ever sent.
	ReadOnly bool `json:"readOnly"`
	// Unlocked records that a connection which is normally read-only ran this
	// submission writable. It is reported back so the result panel can say so:
	// a write that succeeded against PMM's own inventory is worth seeing stated.
	Unlocked bool `json:"unlocked,omitempty"`
}

// ---------------------------------------------------------------- requests

// dexQueryRequest is one submission. It names a connection by id and nothing else:
// there is deliberately no host, user or password field anywhere in this struct,
// because an endpoint that accepted them would be an endpoint that could be pointed
// at something the caller was never authorized to reach.
type dexQueryRequest struct {
	ConnectionID string `json:"connectionId"`
	QueryID      string `json:"queryId"` // client-chosen, so Cancel has something to name
	Database     string `json:"database"`
	Schema       string `json:"schema"`
	// SQL for the SQL engines; Command for Valkey; the Mongo fields for MongoDB.
	SQL     string `json:"sql"`
	Command string `json:"command"`
	Mongo   struct {
		Collection string          `json:"collection"`
		Operation  string          `json:"operation"` // find | aggregate | count | indexes | stats
		Filter     json.RawMessage `json:"filter"`
		Projection json.RawMessage `json:"projection"`
		Sort       json.RawMessage `json:"sort"`
		Pipeline   json.RawMessage `json:"pipeline"`
		Skip       int64           `json:"skip"`
	} `json:"mongo"`
	// Limit is the row ceiling the caller asked for, clamped server-side. Explain
	// turns the submission into a plan request rather than an execution.
	Limit    int  `json:"limit"`
	Offset   int  `json:"offset"`
	Explain  bool `json:"explain"`
	TimeoutS int  `json:"timeoutS"`
	// AllowWrites arms this one submission against a connection that is read-only
	// by policy. It is the second of two locks: the first is the instance-wide
	// administrator setting, and neither alone is enough. A tab left open from
	// before the setting changed does not carry this, so pressing Run in it cannot
	// write into a PMM database.
	AllowWrites bool `json:"allowWrites"`
	Selection   bool `json:"selection"` // the text was a selection, for history's benefit
}

// dexAdapter is what every engine implements. It is deliberately not a SQL
// interface: ListSchemas is empty for the engines that have no schema level,
// DescribeObject returns whatever that engine can honestly say about an object, and
// Query takes the whole request so a document or key/value engine can read the
// fields that belong to it rather than being handed a string of SQL it would have
// to invent a meaning for.
type dexAdapter interface {
	Capabilities() dexCaps
	Version(ctx context.Context) (string, error)
	TestConnection(ctx context.Context) error
	ListDatabases(ctx context.Context) ([]dexTreeNode, error)
	ListSchemas(ctx context.Context, database string) ([]dexTreeNode, error)
	ListObjects(ctx context.Context, database, schema, folder, cursor, filter string) (dexObjectPage, error)
	DescribeObject(ctx context.Context, database string, ref dexObjectRef) (dexObjectDetail, error)
	Query(ctx context.Context, req dexQueryRequest) (dexResult, error)
	Close() error
}

// dexDefaultLimit is what a result is capped at when the caller does not say. A
// table click must never turn into a million rows in a browser tab, so every path
// that returns rows goes through dexClampLimit — including the ones a user did not
// type, like View Data.
const (
	dexDefaultLimit = 500
	dexMaxLimit     = 50000
	dexMaxTimeoutS  = 300
	dexDefTimeoutS  = 30
)

func dexClampLimit(n int) int {
	if n <= 0 {
		return dexDefaultLimit
	}
	if n > dexMaxLimit {
		return dexMaxLimit
	}
	return n
}

func dexClampTimeout(n int) int {
	if n <= 0 {
		return dexDefTimeoutS
	}
	if n > dexMaxTimeoutS {
		return dexMaxTimeoutS
	}
	return n
}
