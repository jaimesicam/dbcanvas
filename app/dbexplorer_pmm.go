package main

// Database Explorer — PMM Server's own databases.
//
// A PMM Server is two interesting databases in a trench coat. Its PostgreSQL holds
// pmm-managed's inventory — every node, service, agent and the settings behind them
// — and its ClickHouse holds Query Analytics: the query digests, their fingerprints
// and the per-minute metric rows the QAN dashboards are drawn from. Both are
// ordinarily invisible, which is a shame, because "what does PMM actually think this
// service is" answers a class of question nothing else does.
//
// -------------------------------------------------------------------- reachability
//
// Neither is reachable over the network, and that is not a guess. On a running
// percona/pmm-server:3 the listeners are:
//
//	127.0.0.1:5432   postgres
//	127.0.0.1:8123   clickhouse (HTTP)
//	127.0.0.1:9000   clickhouse (native)
//
// Loopback only — no bridge address, so nothing outside the container can dial them,
// whether or not a port is published to the host. So these two connections use the
// Docker exec transport the rest of DBCanvas already uses for in-container work, and
// they run the databases' own clients inside the PMM container.
//
// Both clients are asked for machine-readable output rather than the decorated tables
// they print by default:
//
//	psql              -A -F '|' with \gdesc for the column names and their real
//	                  PostgreSQL types, then json_agg(row_to_json(...)) for the rows
//	clickhouse-client --format JSONCompact, which carries meta (name + type) and data
//
// -------------------------------------------------------------------- read-only
//
// Read-only here is enforced by the databases, not by a keyword filter:
//
//	PostgreSQL   every statement runs inside BEGIN READ ONLY. A write is refused by
//	             the server with SQLSTATE 25006 ("cannot execute ... in a read-only
//	             transaction"), including one that a classifier let through.
//	ClickHouse   every query runs with readonly=1. A write is refused by the server
//	             with error 164 (READONLY), and readonly=1 also forbids changing
//	             settings, so the session cannot lift its own restriction.
//
// The application-side classifier (dexReadOnlyRefusal) sits in front of both. It
// gives a better refusal and it is defence in depth — it is not the control.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// The PMM PostgreSQL databases worth showing. `postgres` is the empty maintenance
// database and `template0`/`template1` are the templates; `pmm-managed` is the one
// with the inventory in it and `grafana` holds the dashboards, users and datasources.
var dexPMMPGHidden = map[string]bool{"template0": true, "template1": true}

// dexPMMCHDatabase is where QAN's tables live.
const dexPMMCHDatabase = "pmm"

// dexPMMTargets are the two connections a running PMM Server contributes. They are
// emitted from the deployment record alone — no exec — so listing connections costs
// nothing; the ClickHouse account is read out of the container the first time a
// query actually needs it (dexPMMClickHouseCreds).
func (a *App) dexPMMTargets(ctx context.Context, st Stack, n designNode, dep Deployment, hosts map[string]string, domain string) []dexTarget {
	var cfg struct {
		FQDN    string `json:"fqdn"`
		Version string `json:"version"`
	}
	json.Unmarshal(dep.Config, &cfg)
	host := cfg.FQDN
	if host == "" {
		host = fqdnOf(hosts[n.ID], domain)
	}
	// Read-only is the policy; whether an administrator has allowed it to be lifted
	// is a separate question, asked here rather than assumed, so revoking the
	// setting takes the control away on the next listing.
	unlockable := a.internalWritesAllowed()
	base := dexConnection{
		StackID: st.ID, StackName: st.Name, NodeID: n.ID,
		Group: dexGroupPMM, Role: "instance", Status: "running",
		Host: host, Version: cfg.Version, ReadOnly: true, Policy: dexPMMPolicy,
		Unlockable: unlockable, Warning: dexPMMWarning, Transport: "exec", Preferred: true,
	}

	pg := base
	pg.ID = dexRef{StackID: st.ID, Shape: dexShapePMMPG, Target: n.ID}.String()
	pg.Label = n.Label + " · PostgreSQL — PMM Internal"
	pg.Engine, pg.Kind, pg.Product = dexPostgres, "pmm-postgres", "PMM Internal — Read Only"
	pg.Port = 5432
	pg.User = "postgres"
	pg.Note = "pmm-managed's inventory — the nodes, services and agents PMM believes exist."
	pg.Caps = dexCapsFor(dexPostgres, true)

	ch := base
	ch.ID = dexRef{StackID: st.ID, Shape: dexShapePMMCH, Target: n.ID}.String()
	ch.Label = n.Label + " · ClickHouse — PMM Query Analytics"
	ch.Engine, ch.Kind, ch.Product = dexClickHouse, "pmm-clickhouse", "PMM Internal — Read Only"
	// The HTTP interface, which is what the exec transport actually talks to — the
	// native port next to it is there, but its bundled client cannot insert through
	// it (see dexCHExec). Reporting 9000 would name a port nothing uses.
	ch.Port = dexCHHTTPPort
	ch.User = "default"
	ch.Note = "the Query Analytics store — query digests, fingerprints and per-minute metrics."
	ch.Caps = dexCapsFor(dexClickHouse, true)

	mk := func(c dexConnection) dexTarget {
		return dexTarget{
			Stack: st, DialNodeID: n.ID, ContainerID: dep.ContainerID,
			ExecContainer: dep.ContainerID, Port: c.Port, User: c.User, Conn: c,
			CHDB: dexPMMCHDatabase,
		}
	}
	return []dexTarget{mk(pg), mk(ch)}
}

// ---------------------------------------------------------------- ClickHouse creds

// dexCHCreds is the ClickHouse account inside a PMM container.
type dexCHCreds struct {
	User string
	Pass string
	DB   string
}

var dexCHCredCache = struct {
	sync.Mutex
	m map[string]dexCHCreds
}{m: map[string]dexCHCreds{}}

// dexPMMClickHouseCreds reads the ClickHouse account PMM's own QAN service uses, out
// of the supervisord unit that starts it. Reading it rather than hard-coding it is
// what keeps this working when PMM changes the password: the file is the source PMM
// itself reads, so the two cannot drift.
//
// The fall-back is the account PMM has shipped for years. It is used only when the
// unit cannot be read at all, and a wrong guess surfaces as an authentication error
// from ClickHouse rather than as silence.
func (a *App) dexPMMClickHouseCreds(ctx context.Context, eng Engine, containerID string) dexCHCreds {
	dexCHCredCache.Lock()
	if c, ok := dexCHCredCache.m[containerID]; ok {
		dexCHCredCache.Unlock()
		return c
	}
	dexCHCredCache.Unlock()

	creds := dexCHCreds{User: "default", Pass: "clickhouse", DB: dexPMMCHDatabase}
	res, err := eng.Exec(ctx, containerID, []string{"cat", "/etc/supervisord.d/qan-api2.ini"}, nil)
	if err == nil && res.Code == 0 {
		if v := dexIniValue(res.Stdout, "PMM_CLICKHOUSE_USER"); v != "" {
			creds.User = v
		}
		if v := dexIniValue(res.Stdout, "PMM_CLICKHOUSE_PASSWORD"); v != "" {
			creds.Pass = v
		}
		if v := dexIniValue(res.Stdout, "PMM_CLICKHOUSE_DATABASE"); v != "" {
			creds.DB = v
		}
	}
	dexCHCredCache.Lock()
	dexCHCredCache.m[containerID] = creds
	dexCHCredCache.Unlock()
	return creds
}

// dexIniValue pulls one KEY="value" out of a supervisord environment block. The
// values are quoted and comma-separated across continuation lines, so this is a
// scan for the key rather than an ini parse.
func dexIniValue(src, key string) string {
	i := strings.Index(src, key+"=")
	if i < 0 {
		return ""
	}
	rest := src[i+len(key)+1:]
	if len(rest) == 0 {
		return ""
	}
	if rest[0] == '"' {
		if j := strings.IndexByte(rest[1:], '"'); j >= 0 {
			return rest[1 : 1+j]
		}
		return ""
	}
	for j := 0; j < len(rest); j++ {
		if rest[j] == ',' || rest[j] == '\n' || rest[j] == '\r' {
			return strings.TrimSpace(rest[:j])
		}
	}
	return strings.TrimSpace(rest)
}

// ---------------------------------------------------------------- exec transport

// dexExecRunner runs a command inside a node's container. It is the seam both PMM
// adapters sit on, and it is what lets the ClickHouse adapter be a single generic
// implementation with two transports rather than a lab one and an unrelated PMM one.
type dexExecRunner interface {
	run(ctx context.Context, argv []string, env []string, stdin []byte) (ExecResult, error)
}

type dexContainerExec struct {
	eng Engine
	id  string
	// user is the OS account the client runs as inside the container. Empty means
	// the image's own default, which for percona/pmm-server is `pmm` — and `pmm`
	// is what PostgreSQL's local `trust` rule admits, so nothing needs to be root.
	user string
}

func (d dexContainerExec) run(ctx context.Context, argv, env []string, stdin []byte) (ExecResult, error) {
	if len(stdin) > 0 {
		return d.eng.ExecInput(ctx, d.id, d.user, argv, env, stdin)
	}
	if d.user != "" {
		return d.eng.ExecAs(ctx, d.id, d.user, argv, env)
	}
	return d.eng.Exec(ctx, d.id, argv, env)
}

// dexExecErr turns a failed client invocation into a presentable database error,
// after the adapters have had their chance to parse the engine's own error format.
func dexExecErr(engine string, res ExecResult, err error) *dexError {
	if err != nil {
		return &dexError{Engine: engine, Message: err.Error(), Display: err.Error()}
	}
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("the client exited with status %d and said nothing", res.Code)
	}
	return &dexError{Engine: engine, Message: msg, Display: msg}
}
