package main

import (
	"strings"
	"testing"
)

// The provisioner itself needs a Docker host, so what is locked in here is everything
// that decides what gets written: which backend a line resolves to, what the defaults
// become once that backend is known, the two config files, and the refusals. The
// deployed behaviour behind them was verified against a running stack — see
// IMPLEMENTATION.md §414.

// pgbDoc builds a design with one PgBouncer node linked to each of the backends named.
func pgbDoc(backends ...string) designDoc {
	doc := designDoc{Nodes: []designNode{{ID: "pool", Type: "pgbouncer", Label: "pgbouncer-01"}}}
	for i, b := range backends {
		id := string(rune('a' + i))
		if b == "pg" {
			doc.Nodes = append(doc.Nodes, designNode{ID: id, Type: "pg", Label: "pg-0" + id, PGMajor: "17"})
		} else {
			doc.Frames = append(doc.Frames, designFrame{ID: id, Type: b, Label: b + "-0" + id, PGMajor: "17"})
		}
		doc.Edges = append(doc.Edges, designEdge{
			ID: "e" + id, From: edgeEnd{Node: id}, To: edgeEnd{Node: "pool"}})
	}
	return doc
}

func TestPgBouncerBackendIsExactlyOne(t *testing.T) {
	for _, kind := range []string{"pg", "patroni", "repmgr", "spock"} {
		got, _, _, ok := pgBouncerBackend(pgbDoc(kind), "pool")
		if !ok || got != kind {
			t.Errorf("one %s backend: got %q ok=%v", kind, got, ok)
		}
	}
	// Zero and two are both refusals — a pool fronts exactly one backend.
	if _, _, _, ok := pgBouncerBackend(pgbDoc(), "pool"); ok {
		t.Error("an unlinked pool must not resolve a backend")
	}
	if _, _, _, ok := pgBouncerBackend(pgbDoc("patroni", "repmgr"), "pool"); ok {
		t.Error("two linked backends are ambiguous and must not resolve")
	}
	// A cluster *member* node is not a backend: the frame around it is.
	doc := pgbDoc()
	doc.Nodes = append(doc.Nodes, designNode{ID: "m", Type: "patroni", Label: "patroni-1", FrameID: "f"})
	doc.Edges = append(doc.Edges, designEdge{ID: "e", From: edgeEnd{Node: "m"}, To: edgeEnd{Node: "pool"}})
	if _, _, _, ok := pgBouncerBackend(doc, "pool"); ok {
		t.Error("a line to a cluster member must not resolve as a backend")
	}
	// Nor is a PostgreSQL node that belongs to a frame.
	doc2 := pgbDoc()
	doc2.Nodes = append(doc2.Nodes, designNode{ID: "p", Type: "pg", Label: "pg-01", FrameID: "f"})
	doc2.Edges = append(doc2.Edges, designEdge{ID: "e", From: edgeEnd{Node: "p"}, To: edgeEnd{Node: "pool"}})
	if _, _, _, ok := pgBouncerBackend(doc2, "pool"); ok {
		t.Error("a framed pg node must not resolve as a standalone backend")
	}
}

// The two defaults that differ per topology are the point of passing the backend in
// at all; everything else is the same wherever the pool is pointed.
func TestPgBouncerDefaultsFollowTheBackend(t *testing.T) {
	for _, tc := range []struct{ backend, routing, database string }{
		{"pg", "rw", "postgres"},
		{"patroni", "rw-ro", "postgres"},
		{"repmgr", "rw-ro", "postgres"},
		{"spock", "mesh", spockDefaultDB},
	} {
		d := pgBouncerDefaults(designNode{}, tc.backend)
		if d.PgbRouting != tc.routing {
			t.Errorf("%s: routing = %q, want %q", tc.backend, d.PgbRouting, tc.routing)
		}
		if d.PgbDatabase != tc.database {
			t.Errorf("%s: database = %q, want %q", tc.backend, d.PgbDatabase, tc.database)
		}
		if d.PgbPoolMode != "transaction" || d.PgbAuthType != "scram-sha-256" ||
			d.PgbMaxClientConn != 500 || d.PgbDefaultPoolSize != 20 || d.PgbWatchInterval != 5 {
			t.Errorf("%s: unexpected shared defaults: %+v", tc.backend, d)
		}
		// Every one of these is a driver that cannot connect without it.
		for _, p := range []string{"extra_float_digits", "options", "search_path"} {
			if !strings.Contains(d.PgbIgnoreStartupParams, p) {
				t.Errorf("%s: ignore_startup_parameters omits %s", tc.backend, p)
			}
		}
	}
	// A value the user chose is never overwritten by the backend's default.
	chosen := pgBouncerDefaults(designNode{PgbRouting: "rw", PgbDatabase: "app", PgbPoolMode: "session"}, "spock")
	if chosen.PgbRouting != "rw" || chosen.PgbDatabase != "app" || chosen.PgbPoolMode != "session" {
		t.Errorf("explicit options were overwritten: %+v", chosen)
	}
}

// The watcher exists to follow a role. A standalone server has none, so the option is
// ignored there rather than installing a timer that can only find the same answer.
func TestPgBouncerFollowsRoleOnlyWhereThereIsOne(t *testing.T) {
	on := designNode{PgbFollowPrimary: true}
	for _, backend := range []string{"patroni", "repmgr", "spock"} {
		if !pgBouncerFollowsRole(on, backend) {
			t.Errorf("%s: the watcher should run", backend)
		}
	}
	if pgBouncerFollowsRole(on, "pg") {
		t.Error("a standalone backend has no role to follow")
	}
	if pgBouncerFollowsRole(on, "") {
		t.Error("an unlinked pool has nothing to watch")
	}
	if pgBouncerFollowsRole(designNode{}, "patroni") {
		t.Error("the watcher must not run when it was not asked for")
	}
}

func TestPgBouncerDatabasesINI(t *testing.T) {
	members := []string{"pg-1.example.net", "pg-2.example.net", "pg-3.example.net"}

	rw := pgBouncerDatabasesINI(pgBouncerDefaults(designNode{}, "pg"), "pg", members[:1])
	if !strings.Contains(rw, "* = host=pg-1.example.net port=5432") {
		t.Errorf("rw: no wildcard pool:\n%s", rw)
	}
	if strings.Contains(rw, "_ro") {
		t.Errorf("rw routing must publish no named pools:\n%s", rw)
	}

	// The read-only alias goes on a *different* member from the writer, or the split
	// is a split in name only.
	split := pgBouncerDatabasesINI(pgBouncerDefaults(designNode{}, "patroni"), "patroni", members)
	if !strings.Contains(split, "* = host=pg-1.example.net port=5432") ||
		!strings.Contains(split, "postgres_ro = host=pg-2.example.net port=5432 dbname=postgres") {
		t.Errorf("rw-ro: wrong pools:\n%s", split)
	}
	// Down to one member there is nowhere else to put it, and a pool that refuses to
	// resolve would be worse than one that reads from the writer.
	lone := pgBouncerDatabasesINI(pgBouncerDefaults(designNode{}, "patroni"), "patroni", members[:1])
	if !strings.Contains(lone, "postgres_ro = host=pg-1.example.net") {
		t.Errorf("rw-ro with one member should fall back to it:\n%s", lone)
	}

	mesh := pgBouncerDatabasesINI(pgBouncerDefaults(designNode{}, "spock"), "spock", members)
	for _, want := range []string{
		"* = host=pg-1.example.net port=5432",
		"spockdemo_pg_1 = host=pg-1.example.net port=5432 dbname=spockdemo",
		"spockdemo_pg_2 = host=pg-2.example.net port=5432 dbname=spockdemo",
		"spockdemo_pg_3 = host=pg-3.example.net port=5432 dbname=spockdemo",
	} {
		if !strings.Contains(mesh, want) {
			t.Errorf("mesh: missing %q:\n%s", want, mesh)
		}
	}

	// No members at all is still a parseable file: the watcher replaces it, and a
	// half-written config would stop PgBouncer from starting at all.
	if got := pgBouncerDatabasesINI(designNode{}, "patroni", nil); got != "[databases]\n" {
		t.Errorf("no members: got %q", got)
	}
}

// A pgbouncer database name is an ini key a person types into a connection string.
func TestPgBouncerAlias(t *testing.T) {
	for in, want := range map[string]string{
		"pg-1.example.net": "pg_1",
		"PG-Node.EXAMPLE":  "pg_node",
		"plain":            "plain",
	} {
		if got := pgBouncerAlias(in); got != want {
			t.Errorf("pgBouncerAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPgBouncerINI(t *testing.T) {
	sec := pgSecrets{SuperUser: "postgres", SuperPassword: "pw", ReplUser: "replicator", ReplPassword: "rpw"}
	n := pgBouncerDefaults(designNode{PgbAuthQuery: true, PgbReservePoolSize: 5}, "patroni")
	ini := pgBouncerINI(n, "patroni", false, sec)

	for _, want := range []string{
		"%include /etc/pgbouncer/databases.ini",
		"[pgbouncer]",
		"listen_port = 6432",
		"pool_mode = transaction",
		"auth_type = scram-sha-256",
		"auth_user = postgres",
		"auth_query = SELECT usename, passwd FROM pg_shadow WHERE usename = $1",
		"reserve_pool_timeout = 5",
		"client_tls_sslmode = disable",
		"server_tls_sslmode = prefer",
	} {
		if !strings.Contains(ini, want) {
			t.Errorf("pgbouncer.ini missing %q:\n%s", want, ini)
		}
	}
	// The include has to come before [pgbouncer], or the included file's own
	// [databases] header would put the settings after it in the wrong section.
	if strings.Index(ini, "%include") > strings.Index(ini, "[pgbouncer]") {
		t.Errorf("the %%include must precede [pgbouncer]:\n%s", ini)
	}

	// trust uses neither the file nor the query, so emitting them would be a lie
	// about how the pool authenticates.
	trust := pgBouncerINI(pgBouncerDefaults(designNode{PgbAuthType: "trust", PgbAuthQuery: true}, "pg"), "pg", false, sec)
	for _, unwanted := range []string{"auth_file", "auth_query", "auth_user"} {
		if strings.Contains(trust, unwanted) {
			t.Errorf("auth_type=trust must not emit %s:\n%s", unwanted, trust)
		}
	}

	// auth_query off means userlist.txt is the only source, so there is no auth_user.
	noQuery := pgBouncerINI(pgBouncerDefaults(designNode{}, "pg"), "pg", false, sec)
	if strings.Contains(noQuery, "auth_query") {
		t.Errorf("auth_query off must not emit the query:\n%s", noQuery)
	}
	if !strings.Contains(noQuery, "auth_file") {
		t.Errorf("auth_query off still needs auth_file:\n%s", noQuery)
	}

	// With a certificate the pool terminates TLS under its own name.
	tls := pgBouncerINI(n, "patroni", true, sec)
	for _, want := range []string{
		"client_tls_sslmode = require",
		"client_tls_cert_file = /etc/pgbouncer/server.crt",
		"client_tls_key_file = /etc/pgbouncer/server.key",
		"client_tls_ca_file = /etc/pgbouncer/ca.crt",
	} {
		if !strings.Contains(tls, want) {
			t.Errorf("TLS config missing %q:\n%s", want, tls)
		}
	}
}

// userlist.txt exists so auth_user can authenticate at all; with auth_query on it is
// the *only* thing that has to be in there.
func TestPgBouncerUserlist(t *testing.T) {
	got := pgBouncerUserlist(pgSecrets{SuperUser: "postgres", SuperPassword: "pw", ReplUser: "replicator", ReplPassword: "rpw"})
	if got != "\"postgres\" \"pw\"\n\"replicator\" \"rpw\"\n" {
		t.Errorf("userlist.txt = %q", got)
	}
	// One role, not the same role twice.
	same := pgBouncerUserlist(pgSecrets{SuperUser: "postgres", SuperPassword: "pw", ReplUser: "postgres"})
	if strings.Count(same, "postgres") != 1 {
		t.Errorf("a repeated role must appear once: %q", same)
	}
}

// The watcher is given the superuser password, so its env file has to survive one.
func TestPgBouncerWatchEnvQuotes(t *testing.T) {
	sec := pgSecrets{SuperUser: "postgres", SuperPassword: "p w'$`d"}
	env := pgBouncerWatchEnv(pgBouncerDefaults(designNode{}, "patroni"), "patroni",
		[]string{"a.example.net", "b.example.net"}, sec)
	for _, want := range []string{
		"MODE=patroni",
		"MEMBERS='a.example.net b.example.net'",
		"RESTPORT=8008",
		`PGPASSWORD='p w'\''$` + "`" + `d'`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("backend.env missing %q:\n%s", want, env)
		}
	}
}

// pgx and pgjdbc both break under transaction pooling, and both are told through this.
func TestPgBouncerDSNExtra(t *testing.T) {
	for _, mode := range []string{"transaction", "statement"} {
		if got := pgBouncerDSNExtra(mode); got != "default_query_exec_mode=simple_protocol" {
			t.Errorf("%s pooling: got %q", mode, got)
		}
	}
	// Session pooling holds one server connection for the session, so prepared
	// statements survive and nothing has to be added.
	if got := pgBouncerDSNExtra("session"); got != "" {
		t.Errorf("session pooling should add nothing, got %q", got)
	}
}

func TestLedgerSimParamsBehindAPool(t *testing.T) {
	pooled := stockSimResolved{engine: "postgres", noPrepare: true}
	if got := ledgerSimParams(designNode{}, pooled); got != "prepareThreshold=0" {
		t.Errorf("pooled with no params: got %q", got)
	}
	if got := ledgerSimParams(designNode{LSParams: "ApplicationName=ledger"}, pooled); got != "ApplicationName=ledger&prepareThreshold=0" {
		t.Errorf("pooled with params: got %q", got)
	}
	// Somebody who typed prepareThreshold in has a reason; do not append a second one.
	if got := ledgerSimParams(designNode{LSParams: "prepareThreshold=3"}, pooled); got != "prepareThreshold=3" {
		t.Errorf("explicit prepareThreshold must win: got %q", got)
	}
	// Direct to a server, or a MySQL endpoint: nothing to add.
	if got := ledgerSimParams(designNode{}, stockSimResolved{engine: "postgres"}); got != "" {
		t.Errorf("unpooled: got %q", got)
	}
	if got := ledgerSimParams(designNode{}, stockSimResolved{engine: "mysql", noPrepare: true}); got != "" {
		t.Errorf("mysql: got %q", got)
	}
}

// pgBouncerPGMajor keeps the pooler on the backend's own distribution release unless
// the mismatch is what somebody is demonstrating.
func TestPgBouncerPGMajorFollowsTheBackend(t *testing.T) {
	if got := pgBouncerPGMajor(designNode{}, "pg", designFrame{}, designNode{PGMajor: "18"}); got != "18" {
		t.Errorf("standalone backend: got %q", got)
	}
	if got := pgBouncerPGMajor(designNode{}, "patroni", designFrame{PGMajor: "15"}, designNode{}); got != "15" {
		t.Errorf("frame backend: got %q", got)
	}
	if got := pgBouncerPGMajor(designNode{PGMajor: "13"}, "patroni", designFrame{PGMajor: "15"}, designNode{}); got != "13" {
		t.Errorf("an explicit series must win: got %q", got)
	}
	// Nothing anywhere still has to produce an installable repo.
	if got := pgBouncerPGMajor(designNode{}, "patroni", designFrame{}, designNode{}); got == "" {
		t.Error("an unset series must still resolve to a default")
	}
}

func TestPgBouncerIssues(t *testing.T) {
	errorsIn := func(is []issue) []string {
		var out []string
		for _, i := range is {
			if i.Level == "error" {
				out = append(out, i.Message)
			}
		}
		return out
	}
	node := func(d designDoc) designNode { return d.Nodes[0] }

	// The association rule, in both directions.
	if got := errorsIn(pgBouncerIssues(node(pgbDoc()), pgbDoc())); len(got) != 1 ||
		!strings.Contains(got[0], "must be linked") {
		t.Errorf("unlinked: %v", got)
	}
	two := pgbDoc("patroni", "spock")
	if got := errorsIn(pgBouncerIssues(node(two), two)); len(got) != 1 ||
		!strings.Contains(got[0], "only one backend") {
		t.Errorf("two backends: %v", got)
	}
	// One linked backend and stock options is clean.
	one := pgbDoc("patroni")
	if got := errorsIn(pgBouncerIssues(node(one), one)); len(got) != 0 {
		t.Errorf("a linked pool should validate: %v", got)
	}

	// Each option that can only fail at runtime is caught here instead.
	for _, tc := range []struct{ field, value, want string }{
		{"pool", "roundrobin", "pool mode"},
		{"auth", "ldap", "auth type"},
		{"routing", "split", "routing"},
	} {
		d := pgbDoc("patroni")
		switch tc.field {
		case "pool":
			d.Nodes[0].PgbPoolMode = tc.value
		case "auth":
			d.Nodes[0].PgbAuthType = tc.value
		case "routing":
			d.Nodes[0].PgbRouting = tc.value
		}
		got := errorsIn(pgBouncerIssues(d.Nodes[0], d))
		if len(got) != 1 || !strings.Contains(got[0], tc.want) {
			t.Errorf("%s=%s: %v", tc.field, tc.value, got)
		}
	}

	// A read-only pool on a multi-master cluster is not read-only — a warning, not a
	// refusal, because it still deploys and still works.
	sp := pgbDoc("spock")
	sp.Nodes[0].PgbRouting = "rw-ro"
	warned := false
	for _, i := range pgBouncerIssues(sp.Nodes[0], sp) {
		if i.Level == "warn" && strings.Contains(i.Message, "not read-only") {
			warned = true
		}
	}
	if !warned {
		t.Error("a read-only pool on Spock should warn")
	}
	// A pool that can never fill is a configuration nobody meant.
	cap := pgbDoc("patroni")
	cap.Nodes[0].PgbDefaultPoolSize, cap.Nodes[0].PgbMaxDBConnections = 20, 5
	warned = false
	for _, i := range pgBouncerIssues(cap.Nodes[0], cap) {
		if i.Level == "warn" && strings.Contains(i.Message, "never fill") {
			warned = true
		}
	}
	if !warned {
		t.Error("max_db_connections below default_pool_size should warn")
	}
}

// The compose kind is what makes the node reachable from the CLI, and its EdgeTo list
// is the same four backends the canvas accepts.
func TestPgBouncerComposeKind(t *testing.T) {
	k, ok := composeKindByName("pgbouncer")
	if !ok {
		t.Fatal("compose has no pgbouncer kind")
	}
	if k.Type != "pgbouncer" {
		t.Errorf("Type = %q", k.Type)
	}
	for _, want := range []string{"pg", "patroni", "repmgr", "spock"} {
		found := false
		for _, e := range k.EdgeTo {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("EdgeTo omits %q: %v", want, k.EdgeTo)
		}
	}
	if !k.takes("mode") {
		t.Error("the pool mode must be settable from the CLI")
	}
	// Car Rental Sim reaches its database through the pool as well as directly.
	cs, _ := composeKindByName("carsim")
	found := false
	for _, e := range cs.EdgeTo {
		if e == "pgbouncer" {
			found = true
		}
	}
	if !found {
		t.Errorf("carsim cannot be linked to a pool: %v", cs.EdgeTo)
	}
}

// Shaping a pool has to cover both sides of it: the interesting failure for a pooler
// is a slow backend, not a slow client.
func TestPgBouncerNetemPorts(t *testing.T) {
	ports := netemPortsFor("pgbouncer")
	if len(ports) != 2 || ports[0] != patroniPGPort || ports[1] != pgBouncerPort {
		t.Errorf("netemPortsFor(pgbouncer) = %v", ports)
	}
}

// --------------------------------------------------------------- cert auth
//
// Every expectation below was checked against PgBouncer 1.25.2 and PostgreSQL 16 on
// a running stack; the messages quoted in the comments are the ones those two
// actually produce. See IMPLEMENTATION.md §415.

func TestPgBouncerCertAuthConfig(t *testing.T) {
	sec := pgSecrets{SuperUser: "postgres", SuperPassword: "pw", ReplUser: "replicator", ReplPassword: "rpw"}
	n := pgBouncerDefaults(designNode{PgbAuthType: "cert", PgbAuthQuery: true, GenerateCert: true}, "patroni")
	ini := pgBouncerINI(n, "patroni", true, sec)

	// Not a preference: PgBouncer refuses to start otherwise with
	// "auth_type=cert requires client_tls_sslmode=SSLMODE_VERIFY_FULL".
	if !strings.Contains(ini, "client_tls_sslmode = verify-full") {
		t.Errorf("cert auth must force verify-full:\n%s", ini)
	}
	// The certificate authenticates the client; the pool still has to authenticate
	// itself to the server, and userlist.txt is the only secret it has for that.
	if !strings.Contains(ini, "auth_file") {
		t.Errorf("cert auth still needs auth_file for the server leg:\n%s", ini)
	}
	// auth_query would hand back a SCRAM verifier, which cannot be used to
	// authenticate as a client — so a role found that way would pass the pool and
	// fail behind it.
	for _, unwanted := range []string{"auth_query", "auth_user"} {
		if strings.Contains(ini, unwanted) {
			t.Errorf("cert auth must not emit %s:\n%s", unwanted, ini)
		}
	}
	// The server leg's own certificate, for a backend whose pg_hba puts
	// `hostssl … cert` first — without it that backend answers
	// "FATAL connection requires a valid client certificate".
	for _, want := range []string{
		"server_tls_cert_file = /etc/pgbouncer/client/postgres.crt",
		"server_tls_key_file = /etc/pgbouncer/client/postgres.key",
		"server_tls_ca_file = /etc/pgbouncer/client/ca.crt",
	} {
		if !strings.Contains(ini, want) {
			t.Errorf("missing %q:\n%s", want, ini)
		}
	}
	// A pool told not to use TLS to the backend has no server leg to certify.
	noTLS := pgBouncerINI(pgBouncerDefaults(designNode{PgbServerTLS: "disable", GenerateCert: true}, "pg"), "pg", true, sec)
	if strings.Contains(noTLS, "server_tls_cert_file") {
		t.Errorf("server_tls_sslmode=disable must not carry a certificate:\n%s", noTLS)
	}
	// And a pool with no certificate at all carries neither half.
	plain := pgBouncerINI(pgBouncerDefaults(designNode{}, "pg"), "pg", false, sec)
	if strings.Contains(plain, "server_tls_cert_file") || !strings.Contains(plain, "client_tls_sslmode = disable") {
		t.Errorf("an uncertificated pool should have no TLS material:\n%s", plain)
	}
}

func TestPgBouncerCertAuthIssues(t *testing.T) {
	errs := func(is []issue) []string {
		var out []string
		for _, i := range is {
			if i.Level == "error" {
				out = append(out, i.Message)
			}
		}
		return out
	}
	// cert without a certificate: PgBouncer would not start.
	d := pgbDoc("patroni")
	d.Nodes[0].PgbAuthType = "cert"
	got := errs(pgBouncerIssues(d.Nodes[0], d))
	if len(got) != 1 || !strings.Contains(got[0], "needs a certificate") {
		t.Errorf("cert without generateCert: %v", got)
	}
	// With one, it validates.
	d.Nodes[0].GenerateCert = true
	if got := errs(pgBouncerIssues(d.Nodes[0], d)); len(got) != 0 {
		t.Errorf("cert with generateCert should validate: %v", got)
	}
	// A simulator cannot present a certificate — it fails at the TLS handshake with
	// "tlsv13 alert certificate required", after deploying successfully.
	withSim := pgbDoc("patroni")
	withSim.Nodes[0].PgbAuthType, withSim.Nodes[0].GenerateCert = "cert", true
	withSim.Nodes = append(withSim.Nodes, designNode{ID: "sim", Type: "carsim", Label: "carsim-01"})
	withSim.Edges = append(withSim.Edges, designEdge{ID: "es", From: edgeEnd{Node: "pool"}, To: edgeEnd{Node: "sim"}})
	got = errs(pgBouncerIssues(withSim.Nodes[0], withSim))
	if len(got) != 1 || !strings.Contains(got[0], "carsim-01") {
		t.Errorf("a linked simulator should be refused under cert auth: %v", got)
	}

	// The server leg: a backend that judges TLS clients by certificate, and a pool
	// with none to present.
	back := pgbDoc("patroni")
	back.Frames[0].PGHostAuth = "cert,scram-sha-256"
	got = errs(pgBouncerIssues(back.Nodes[0], back))
	if len(got) != 1 || !strings.Contains(got[0], "authenticates TLS clients by certificate") {
		t.Errorf("cert-requiring backend with an uncertificated pool: %v", got)
	}
	// Giving the pool a certificate fixes it...
	back.Nodes[0].GenerateCert = true
	if got := errs(pgBouncerIssues(back.Nodes[0], back)); len(got) != 0 {
		t.Errorf("a certificated pool should reach a cert-requiring backend: %v", got)
	}
	// ...and so does refusing TLS on the server leg, which falls through to the
	// password rule below the cert one.
	back.Nodes[0].GenerateCert, back.Nodes[0].PgbServerTLS = false, "disable"
	if got := errs(pgBouncerIssues(back.Nodes[0], back)); len(got) != 0 {
		t.Errorf("server_tls_sslmode=disable should reach the password rule: %v", got)
	}
	// A backend whose password rule is first shadows its own cert rule, so the pool
	// is never asked for one.
	shadowed := pgbDoc("patroni")
	shadowed.Frames[0].PGHostAuth = "scram-sha-256,cert"
	if got := errs(pgBouncerIssues(shadowed.Nodes[0], shadowed)); len(got) != 0 {
		t.Errorf("a shadowed cert rule should not affect the pool: %v", got)
	}
	// The same question of a standalone backend, which keeps it on the node.
	pgBack := pgbDoc("pg")
	for i := range pgBack.Nodes {
		if pgBack.Nodes[i].Type == "pg" {
			pgBack.Nodes[i].PGHostAuth = "cert,scram-sha-256"
		}
	}
	if got := errs(pgBouncerIssues(pgBack.Nodes[0], pgBack)); len(got) != 1 {
		t.Errorf("a standalone cert-requiring backend should be caught too: %v", got)
	}
}

// --------------------------------------------------- pg_hba, on the server side

func TestPGHostAuthLines(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want []string
	}{
		// Unset is what every design before this field had.
		{"", []string{"host all all 0.0.0.0/0 scram-sha-256"}},
		{"md5", []string{"host all all 0.0.0.0/0 md5"}},
		// cert is hostssl, because the method needs a client certificate and a rule
		// that also matched plaintext could only ever refuse it.
		{"cert", []string{"hostssl all all 0.0.0.0/0 cert"}},
		{"cert,scram-sha-256", []string{
			"hostssl all all 0.0.0.0/0 cert",
			"host all all 0.0.0.0/0 scram-sha-256",
		}},
		// Order is preserved, because order is what decides which rule is reachable.
		{"scram-sha-256,cert", []string{
			"host all all 0.0.0.0/0 scram-sha-256",
			"hostssl all all 0.0.0.0/0 cert",
		}},
		{" CERT , Scram-SHA-256 , cert ", []string{
			"hostssl all all 0.0.0.0/0 cert",
			"host all all 0.0.0.0/0 scram-sha-256",
		}},
		// Something unrecognised is dropped from the file (and reported separately),
		// never written into pg_hba where it would stop PostgreSQL from starting.
		{"ldap", []string{"host all all 0.0.0.0/0 scram-sha-256"}},
	} {
		got := pgHostAuthLines(tc.spec)
		if len(got) != len(tc.want) {
			t.Errorf("%q → %v, want %v", tc.spec, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q line %d = %q, want %q", tc.spec, i, got[i], tc.want[i])
			}
		}
	}
}

// The question a pooler asks of its backend, which is not "does it accept
// certificates" but "will it demand one from me".
func TestPGHostAuthRequiresClientCert(t *testing.T) {
	for spec, want := range map[string]bool{
		"":                   false,
		"scram-sha-256":      false,
		"cert":               true,
		"cert,scram-sha-256": true,
		// A plain host rule above it matches TLS connections too, so the cert rule
		// below is never consulted.
		"scram-sha-256,cert": false,
		"trust,cert":         false,
	} {
		if got := pgHostAuthRequiresClientCert(spec); got != want {
			t.Errorf("pgHostAuthRequiresClientCert(%q) = %v, want %v", spec, got, want)
		}
	}
}

func TestPGHostAuthIssues(t *testing.T) {
	level := func(is []issue, l string) []string {
		var out []string
		for _, i := range is {
			if i.Level == l {
				out = append(out, i.Message)
			}
		}
		return out
	}
	// The default is clean.
	if got := pgHostAuthIssues("node x", "", false); len(got) != 0 {
		t.Errorf("the default should be clean: %v", got)
	}
	// cert needs the server to be listening for TLS with a CA to verify against.
	got := level(pgHostAuthIssues("node x", "cert,scram-sha-256", false), "error")
	if len(got) != 1 || !strings.Contains(got[0], "listening for TLS") {
		t.Errorf("cert without TLS: %v", got)
	}
	if got := level(pgHostAuthIssues("node x", "cert,scram-sha-256", true), "error"); len(got) != 0 {
		t.Errorf("cert with TLS should not error: %v", got)
	}
	// ...and with TLS it still warns about what it means for every TLS client.
	warns := level(pgHostAuthIssues("node x", "cert,scram-sha-256", true), "warn")
	if len(warns) != 1 || !strings.Contains(warns[0], "must present a certificate") {
		t.Errorf("cert-first should warn about TLS clients: %v", warns)
	}
	// A plain rule above anything makes what follows unreachable, and the warning
	// names the dead rules rather than calling the list invalid.
	warns = level(pgHostAuthIssues("node x", "scram-sha-256,cert", true), "warn")
	if len(warns) != 1 || !strings.Contains(warns[0], "can never be reached") {
		t.Errorf("a shadowed rule should warn: %v", warns)
	}
	// An unknown method is named rather than silently dropped.
	got = level(pgHostAuthIssues("node x", "ldap", true), "error")
	if len(got) != 1 || !strings.Contains(got[0], "ldap") {
		t.Errorf("an unknown method: %v", got)
	}
	// trust says what it costs.
	if w := level(pgHostAuthIssues("node x", "trust", true), "warn"); len(w) == 0 {
		t.Error("trust should warn")
	}
}
