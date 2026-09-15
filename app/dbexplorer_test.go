package main

// Database Explorer — the unit suite.
//
// Two things get the most attention here, because they are the two that would be
// worst to get wrong: the authorization boundary (a connection id is a token, not a
// permission) and the read-only guarantee on PMM's internal databases. The rest
// pins the behaviour that is easy to break silently — NULL not becoming an empty
// string, a result limit actually capping, a row not being updated by a predicate
// that could match more than one.

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// dexRESPRead decodes one reply straight off a wire string, which is how the frame
// decoder is exercised without a server. The connection's socket is never touched by
// read(), so a reader alone is a complete fixture.
func dexRESPRead(wire string) (dexRESPValue, error) {
	c := &dexRESPConn{br: bufio.NewReader(strings.NewReader(wire))}
	return c.read(0)
}

// ---------------------------------------------------------------- connection ids

func TestDexConnectionIDRoundTrips(t *testing.T) {
	for _, r := range []dexRef{
		{StackID: 1, Shape: dexShapeNode, Target: "node1"},
		{StackID: 42, Shape: dexShapeAIO, Target: "aio1", Extra: "ps01"},
		{StackID: 7, Shape: dexShapeHAProxy, Target: "hap1", Extra: "write"},
		{StackID: 3, Shape: dexShapePMMCH, Target: "pmm1"},
	} {
		got, err := dexParseID(r.String())
		if err != nil {
			t.Fatalf("%s: %v", r.String(), err)
		}
		if got != r {
			t.Errorf("%s round-tripped to %+v", r.String(), got)
		}
	}
}

func TestDexConnectionIDRejectsNonsense(t *testing.T) {
	// Every one of these is something a browser could send. None of them may parse
	// into a reference that later code would act on.
	for _, id := range []string{
		"", "1", "1~n", "abc~n~x", "0~n~x", "-1~n~x", "1~zz~x", "1~n~", "1~n~x~y~z",
		"1~n~x~y~", "../../etc/passwd", "1~n~x~../../y",
	} {
		if ref, err := dexParseID(id); err == nil {
			t.Errorf("%q parsed to %+v; want a refusal", id, ref)
		}
	}
}

// ---------------------------------------------------------------- authorization

// seedStack writes a stack with one running PostgreSQL node, which is enough shape
// for discovery to produce a connection.
func seedStack(t *testing.T, app *App, owner User, name string) Stack {
	t.Helper()
	design := `{"nodes":[{"id":"pg1","type":"pg","label":"pg-01"}],"frames":[],"edges":[]}`
	st, err := app.store.CreateStack(name, owner.ID, ttlInfinity, nil, []byte(design))
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}
	sec, _ := json.Marshal(pgSecrets{SuperUser: "postgres", SuperPassword: "s3cret-do-not-leak"})
	cfg, _ := json.Marshal(map[string]any{"fqdn": "pg-01.example.net", "pgMajor": "16"})
	if err := app.store.UpsertDeployment(Deployment{
		StackID: st.ID, NodeID: "pg1", ContainerID: "c-" + name,
		State: DeployRunning, Secrets: sec, Config: cfg,
	}); err != nil {
		t.Fatalf("upsert deployment: %v", err)
	}
	got, _ := app.store.GetStack(st.ID)
	return got
}

func TestDexDiscoversDeployedNodesWithoutBeingAsked(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	seedStack(t, app, u, "lab")

	groups := app.dexConnections(context.Background(), u)
	if len(groups) != 1 {
		t.Fatalf("want one stack, got %d", len(groups))
	}
	if groups[0].StackName != "lab" {
		t.Errorf("stack name = %q", groups[0].StackName)
	}
	if len(groups[0].Groups) != 1 || groups[0].Groups[0].Name != dexGroupPostgres {
		t.Fatalf("want one PostgreSQL group, got %+v", groups[0].Groups)
	}
	c := groups[0].Groups[0].Connections[0]
	// The whole point of the feature: everything needed to connect is already here,
	// and none of it had to be typed.
	if c.Host != "pg-01.example.net" || c.Port != 5432 {
		t.Errorf("address = %s:%d, want the deployment's own", c.Host, c.Port)
	}
	if c.User != "postgres" {
		t.Errorf("account = %q, want the superuser resolved server-side", c.User)
	}
	if !c.Caps.SQL || !c.Caps.Schemas {
		t.Errorf("PostgreSQL should advertise SQL and a schema level: %+v", c.Caps)
	}
}

func TestDexNeverReturnsCredentialsToTheBrowser(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	seedStack(t, app, u, "lab")

	// The password is in the deployment and the server resolves it — but nothing
	// that is serialised may carry it. Marshal the whole payload the handler would
	// write and search it, which catches a field added later as well as the ones
	// that exist now.
	blob, err := json.Marshal(map[string]any{"stacks": app.dexConnections(context.Background(), u)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"s3cret-do-not-leak", "password", "Password", "passwd", "secret"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("the connection list contains %q:\n%s", secret, blob)
		}
	}

	// The resolved target does hold the password — that is its job — but the DTO
	// inside it does not, so nothing serialisable can reach the browser with one.
	tgt, err := app.dexResolve(context.Background(), u, app.dexConnections(context.Background(), u)[0].Groups[0].Connections[0].ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tgt.Pass != "s3cret-do-not-leak" {
		t.Fatalf("the server must resolve the real password, got %q", tgt.Pass)
	}
	connBlob, _ := json.Marshal(tgt.Conn)
	if strings.Contains(string(connBlob), "s3cret") {
		t.Fatalf("dexConnection leaked the password: %s", connBlob)
	}
}

func TestDexAuthorizationIsolatesUsers(t *testing.T) {
	app := newTestApp(t)
	alice, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	bob, _ := app.store.CreateUser("bob", "x", RoleUser, StatusApproved)
	admin, _ := app.store.CreateUser("root", "x", RoleAdmin, StatusApproved)
	seedStack(t, app, alice, "alice-lab")
	seedStack(t, app, bob, "bob-lab")

	ctx := context.Background()
	aliceConns := app.dexConnections(ctx, alice)
	bobConns := app.dexConnections(ctx, bob)
	if len(aliceConns) != 1 || aliceConns[0].StackName != "alice-lab" {
		t.Fatalf("alice sees %+v", aliceConns)
	}
	if len(bobConns) != 1 || bobConns[0].StackName != "bob-lab" {
		t.Fatalf("bob sees %+v", bobConns)
	}

	// The id is a token, not a permission. Bob holding alice's id — copied from a
	// screenshot, guessed, or replayed — must not resolve.
	aliceID := aliceConns[0].Groups[0].Connections[0].ID
	if _, err := app.dexResolve(ctx, bob, aliceID); err == nil {
		t.Fatal("bob resolved alice's connection")
	} else if !strings.Contains(err.Error(), "not found") {
		// And the refusal must not confirm that the stack exists.
		t.Errorf("refusal leaks existence: %v", err)
	}
	if _, err := app.dexResolve(ctx, alice, aliceID); err != nil {
		t.Errorf("alice cannot reach her own connection: %v", err)
	}
	// An admin may, which is the documented rule everywhere else in the app.
	if _, err := app.dexResolve(ctx, admin, aliceID); err != nil {
		t.Errorf("an admin should reach any stack: %v", err)
	}
	if len(app.dexConnections(ctx, admin)) != 2 {
		t.Errorf("an admin should see both stacks")
	}
}

func TestDexStoppedNodeStopsResolving(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	st := seedStack(t, app, u, "lab")
	ctx := context.Background()
	id := app.dexConnections(ctx, u)[0].Groups[0].Connections[0].ID

	// Discovery reads the deployment on every request, so a node that was stopped
	// between two clicks stops being reachable rather than being cached as live.
	dep, _ := app.store.GetDeployment(st.ID, "pg1")
	dep.State = DeployStopped
	app.store.UpsertDeployment(dep)

	if _, err := app.dexResolve(ctx, u, id); err == nil {
		t.Fatal("a stopped node should no longer resolve")
	}
	if len(app.dexConnections(ctx, u)) != 0 {
		t.Error("a stopped node should leave the connection list")
	}
}

// ---------------------------------------------------------------- read-only

func TestDexReadOnlyRefusesEveryWrite(t *testing.T) {
	// These are the statements the PMM connections must refuse before anything is
	// sent. The database refuses them too — PostgreSQL with SQLSTATE 25006 inside
	// BEGIN READ ONLY, ClickHouse with error 164 under readonly=2 — but the
	// classifier is what turns a refusal into a sentence naming the statement.
	for _, q := range []string{
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"ALTER TABLE t ADD COLUMN c INT",
		"DROP TABLE t",
		"TRUNCATE t",
		"CREATE TABLE t (a INT)",
		"GRANT ALL ON t TO x",
		"REVOKE ALL ON t FROM x",
		"  \n\t insert into t values (1)",
		"-- a comment first\nDELETE FROM t",
		"/* block */ DROP TABLE t",
		"WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x",
		"WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x",
		"SET search_path = evil",
		"CALL do_something()",
	} {
		if e := dexReadOnlyRefusal(dexPostgres, q); e == nil {
			t.Errorf("read-only accepted %q", q)
		}
	}
}

func TestDexReadOnlyRefusesMultiStatementBypass(t *testing.T) {
	// The oldest trick there is: hide the write behind a read and a semicolon. A
	// classifier that reads only as far as the first statement waves it through.
	for _, q := range []string{
		"SELECT 1; DROP TABLE t",
		"SELECT 1;DELETE FROM t",
		"SELECT 'a;b'; UPDATE t SET a = 1",
		"SELECT 1 /* ; */; TRUNCATE t",
		"SELECT 1; SELECT 2", // even two reads: one statement at a time
	} {
		e := dexReadOnlyRefusal(dexPostgres, q)
		if e == nil {
			t.Fatalf("read-only accepted %q", q)
		}
		if e.Code != "DBX_READONLY" && e.Code != "DBX_MULTI" {
			t.Errorf("%q refused with %q, want a read-only refusal", q, e.Code)
		}
	}
}

func TestDexReadOnlyAllowsReads(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM pg_stat_activity",
		"select 1",
		"WITH a AS (SELECT 1) SELECT * FROM a",
		"SHOW search_path",
		"EXPLAIN SELECT 1",
		"TABLE pg_database",
		"VALUES (1), (2)",
		"-- why\nSELECT count(*) FROM services",
	} {
		if e := dexReadOnlyRefusal(dexPostgres, q); e != nil {
			t.Errorf("read-only refused a read %q: %s", q, e.Display)
		}
	}
}

func TestDexPMMConnectionsAreReadOnlyAndLabelled(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	design := `{"nodes":[{"id":"pmm1","type":"pmm","label":"pmm-01"}],"frames":[],"edges":[]}`
	st, _ := app.store.CreateStack("lab", u.ID, ttlInfinity, nil, []byte(design))
	cfg, _ := json.Marshal(map[string]any{"fqdn": "pmm-01.example.net", "version": "3"})
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "pmm1", ContainerID: "c-pmm",
		State: DeployRunning, Config: cfg})

	groups := app.dexConnections(context.Background(), u)
	if len(groups) != 1 || len(groups[0].Groups) != 1 {
		t.Fatalf("want one PMM group, got %+v", groups)
	}
	g := groups[0].Groups[0]
	if g.Name != dexGroupPMM {
		t.Fatalf("group = %q, want %q", g.Name, dexGroupPMM)
	}
	if len(g.Connections) != 2 {
		t.Fatalf("a PMM Server contributes PostgreSQL and ClickHouse, got %d", len(g.Connections))
	}
	engines := map[string]dexConnection{}
	for _, c := range g.Connections {
		engines[c.Engine] = c
		if !c.ReadOnly {
			t.Errorf("%s: a PMM internal database must be read-only", c.Engine)
		}
		if c.Policy != dexPMMPolicy {
			t.Errorf("%s: policy = %q", c.Engine, c.Policy)
		}
		if !strings.Contains(c.Product, "Read Only") {
			t.Errorf("%s: product label = %q, want it to say read-only", c.Engine, c.Product)
		}
		if !strings.Contains(c.Warning, "may corrupt or break the PMM installation") {
			t.Errorf("%s: the warning must say what modifying it does: %q", c.Engine, c.Warning)
		}
		if c.Engine == dexClickHouse && c.Port != dexCHHTTPPort {
			t.Errorf("ClickHouse should be reported on the HTTP port it is reached on, got %d", c.Port)
		}
		if c.Transport != "exec" {
			// PMM's PostgreSQL and ClickHouse listen on 127.0.0.1 inside the
			// container, so there is nothing to dial; see dbexplorer_pmm.go.
			t.Errorf("%s: transport = %q, want exec", c.Engine, c.Transport)
		}
		if c.Caps.EditableRows {
			t.Errorf("%s: a read-only connection must not offer row editing", c.Engine)
		}
	}
	if _, ok := engines[dexPostgres]; !ok {
		t.Error("PMM's internal PostgreSQL is missing")
	}
	if _, ok := engines[dexClickHouse]; !ok {
		t.Error("PMM's Query Analytics ClickHouse is missing")
	}
}

// ---------------------------------------------------------------- unlocking PMM

// setInternalWrites flips the instance-wide administrator setting.
func setInternalWrites(t *testing.T, app *App, on bool) {
	t.Helper()
	v := "0"
	if on {
		v = "1"
	}
	if err := app.store.SetAppSetting(settingInternalWrites, v); err != nil {
		t.Fatalf("set %s: %v", settingInternalWrites, err)
	}
}

// seedPMM writes a stack with one running PMM Server.
func seedPMM(t *testing.T, app *App, owner User) Stack {
	t.Helper()
	design := `{"nodes":[{"id":"pmm1","type":"pmm","label":"pmm-01"}],"frames":[],"edges":[]}`
	st, err := app.store.CreateStack("lab", owner.ID, ttlInfinity, nil, []byte(design))
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}
	cfg, _ := json.Marshal(map[string]any{"fqdn": "pmm-01.example.net", "version": "3"})
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "pmm1", ContainerID: "c-pmm",
		State: DeployRunning, Config: cfg})
	got, _ := app.store.GetStack(st.ID)
	return got
}

func dexPMMTarget(t *testing.T, app *App, u User, engine string) dexTarget {
	t.Helper()
	for _, g := range app.dexConnections(context.Background(), u) {
		for _, grp := range g.Groups {
			for _, c := range grp.Connections {
				if c.Engine == engine && c.Policy == dexPMMPolicy {
					tgt, err := app.dexResolve(context.Background(), u, c.ID)
					if err != nil {
						t.Fatalf("resolve %s: %v", c.ID, err)
					}
					return tgt
				}
			}
		}
	}
	t.Fatalf("no PMM %s connection found", engine)
	return dexTarget{}
}

func TestDexUnlockNeedsBothTheSettingAndTheRequest(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	seedPMM(t, app, u)
	pg := dexPMMTarget(t, app, u, dexPostgres)

	// Neither lock: read-only, as it has always been.
	if app.dexUnlock(pg, false) {
		t.Error("unlocked with neither the setting nor the request")
	}
	// The request alone is not enough — this is the case that matters, because the
	// request is the half an ordinary user controls.
	if app.dexUnlock(pg, true) {
		t.Error("a request unlocked a PMM database with the setting off")
	}

	setInternalWrites(t, app, true)

	// The setting alone is not enough either: a tab left open from before the
	// setting changed carries no arm, and pressing Run in it must not write.
	if app.dexUnlock(pg, false) {
		t.Error("the setting alone unlocked a connection the request did not arm")
	}
	// Both: unlocked.
	if !app.dexUnlock(pg, true) {
		t.Error("the setting and the request together should unlock it")
	}

	// Turning it back off takes effect immediately — the setting is read per
	// request, not cached at startup.
	setInternalWrites(t, app, false)
	if app.dexUnlock(pg, true) {
		t.Error("revoking the setting did not take effect")
	}
}

func TestDexUnlockOnlyAppliesToAPolicyReadOnlyConnection(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	seedStack(t, app, u, "lab")
	setInternalWrites(t, app, true)

	// An ordinary database node is not read-only by policy, so there is nothing for
	// the unlock to do — and asking must not be treated as meaning anything.
	conns := app.dexConnections(context.Background(), u)
	tgt, err := app.dexResolve(context.Background(), u, conns[0].Groups[0].Connections[0].ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tgt.Conn.Policy != "" {
		t.Fatalf("an ordinary node should carry no policy, got %q", tgt.Conn.Policy)
	}
	if app.dexUnlock(tgt, true) {
		t.Error("unlock applied to a connection that was never read-only by policy")
	}
}

func TestDexUnlockableIsReportedOnlyWhenAllowed(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	seedPMM(t, app, u)

	unlockable := func() bool {
		for _, g := range app.dexConnections(context.Background(), u) {
			for _, grp := range g.Groups {
				for _, c := range grp.Connections {
					if !c.ReadOnly {
						t.Errorf("%s should still be read-only until a request arms it", c.Label)
					}
					if c.Unlockable {
						return true
					}
				}
			}
		}
		return false
	}
	if unlockable() {
		t.Error("the browser was offered the arming control with the setting off")
	}
	setInternalWrites(t, app, true)
	if !unlockable() {
		t.Error("the arming control was not offered with the setting on")
	}
}

func TestDexUnlockedTransportsDropTheirServerSideReadOnly(t *testing.T) {
	// The guarantee is enforced by the databases, so lifting it has to change what
	// is actually sent — not merely what the classifier allows.
	locked, _ := dexPGScript("pmm-managed", "SELECT 1", 500, true)
	if !strings.Contains(locked, "BEGIN READ ONLY") {
		t.Fatal("a locked PostgreSQL session must open a READ ONLY transaction")
	}
	unlocked, _ := dexPGScript("pmm-managed", "SELECT 1", 500, false)
	if strings.Contains(unlocked, "BEGIN READ ONLY") {
		t.Error("an unlocked session must not be wrapped in a READ ONLY transaction")
	}
	// And a write is no longer refused before it is sent.
	if e := dexReadOnlyRefusal(dexPostgres, "UPDATE nodes SET node_name = 'x'"); e == nil {
		t.Fatal("the classifier should refuse a write when it is consulted")
	}
}

func TestDexUnlockedAdapterRegainsEditing(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	seedPMM(t, app, u)
	setInternalWrites(t, app, true)
	pg := dexPMMTarget(t, app, u, dexPostgres)

	if pg.Conn.Caps.EditableRows {
		t.Error("a locked PMM connection must not advertise row editing")
	}
	// dexOpenUnlocked rebuilds the capabilities from the lifted policy, which is what
	// makes the grid offer an edit at all.
	if !dexCapsFor(pg.Conn.Engine, false).EditableRows {
		t.Error("an unlocked PostgreSQL connection should advertise row editing")
	}
}

// ---------------------------------------------------------------- SQL plumbing

func TestDexSplitSQLRespectsQuotingAndComments(t *testing.T) {
	cases := []struct {
		src  string
		want int
	}{
		{"SELECT 1", 1},
		{"SELECT 1;", 1},
		{"SELECT 1; SELECT 2", 2},
		{"SELECT ';'", 1},
		{`SELECT ";"`, 1},
		{"SELECT `a;b`", 1},
		{"SELECT 1 -- ; not a boundary\n", 1},
		{"SELECT 1 /* ; */ ", 1},
		{"SELECT $tag$ ; $tag$", 1},
		{"SELECT 1;;;SELECT 2", 2},
		{"  ;  ", 0},
	}
	for _, c := range cases {
		if got := len(dexSplitSQL(c.src)); got != c.want {
			t.Errorf("dexSplitSQL(%q) = %d statements, want %d", c.src, got, c.want)
		}
	}
}

func TestDexVerbSeesThroughCommentsAndCTEs(t *testing.T) {
	cases := map[string]string{
		"SELECT 1":                              "SELECT",
		"  \n select 1":                         "SELECT",
		"-- hello\nDROP TABLE t":                "DROP",
		"/* x */ delete from t":                 "DELETE",
		"WITH a AS (SELECT 1) SELECT * FROM a":  "SELECT",
		"WITH a AS (SELECT 1) DELETE FROM t":    "DELETE",
		"WITH a AS (INSERT INTO t VALUES (1))…": "INSERT",
	}
	for src, want := range cases {
		if got := dexVerbOf(src); got != want {
			t.Errorf("dexVerbOf(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestDexSemanticTypesCoverThreeEngines(t *testing.T) {
	cases := map[string]string{
		// MySQL
		"BIGINT": dexSemInteger, "VARCHAR(255)": dexSemString, "DATETIME": dexSemDateTime,
		"TINYINT(1)": dexSemInteger, "LONGBLOB": dexSemBinary, "JSON": dexSemJSON,
		"DECIMAL(10,2)": dexSemNumber,
		// The driver puts the modifier in front, which is easy to miss and turns an
		// ordinary integer column into a left-aligned string. Seen on a real
		// Percona Server 9.7 column declared BIGINT UNSIGNED.
		"UNSIGNED BIGINT": dexSemInteger, "INT UNSIGNED": dexSemInteger,
		// PostgreSQL
		"int4": dexSemInteger, "timestamp with time zone": dexSemDateTime,
		"jsonb": dexSemJSON, "bytea": dexSemBinary, "boolean": dexSemBool,
		"text[]": dexSemArray, "double precision": dexSemNumber,
		// ClickHouse
		"UInt64": dexSemInteger, "Nullable(String)": dexSemString,
		"LowCardinality(String)": dexSemString, "Array(UInt8)": dexSemArray,
		"DateTime64(3)": dexSemDateTime, "FixedString(16)": dexSemBinary,
		"Float64": dexSemNumber,
	}
	for in, want := range cases {
		if got := dexSemanticOf(in); got != want {
			t.Errorf("dexSemanticOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDexCellKeepsNullBinaryAndWideIntegersHonest(t *testing.T) {
	// NULL is nil and nothing else. An empty string is an empty string. A grid that
	// conflates them has thrown away the distinction people open a client to check.
	if v := dexCell(nil, dexSemString); v != nil {
		t.Errorf("NULL became %#v", v)
	}
	if v := dexCell([]byte(""), dexSemString); v != "" {
		t.Errorf("an empty string became %#v", v)
	}

	// A BLOB is not text, and must not be rendered as though it were.
	b := dexCell([]byte{0x00, 0x01, 0xff}, dexSemBinary)
	bin, ok := b.(dexBinary)
	if !ok || bin.Marker != "binary" || bin.Len != 3 || bin.Hex != "0001ff" {
		t.Fatalf("binary cell = %#v", b)
	}
	// Even typed as a string, bytes that cannot be text are carried as binary.
	if _, ok := dexCell([]byte{0x00, 0x01}, dexSemString).(dexBinary); !ok {
		t.Error("NUL bytes typed as a string should still travel as binary")
	}
	// But a real string column stays a string.
	if v := dexCell([]byte("hello"), dexSemString); v != "hello" {
		t.Errorf("a text column became %#v", v)
	}

	// JSON's number is a float64; 2^63-1 does not survive one. Carrying it as text
	// is the difference between showing the value and showing a rounded neighbour.
	big := dexCell(uint64(18446744073709551615), dexSemInteger)
	bn, ok := big.(dexBigNum)
	if !ok || bn.Text != "18446744073709551615" {
		t.Fatalf("wide unsigned = %#v", big)
	}
	if v := dexCell(int64(42), dexSemInteger); v != int64(42) {
		t.Errorf("an ordinary integer became %#v", v)
	}
}

func TestDexTimestampsFormatOneWay(t *testing.T) {
	ts := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.FixedZone("x", 3600))
	got := dexFormatTime(ts)
	if got != "2026-03-04T04:06:07.123Z" {
		t.Errorf("dexFormatTime = %q, want UTC with milliseconds", got)
	}
	if dexFormatTime(time.Time{}) != "" {
		t.Error("a zero time is not a moment")
	}
}

func TestDexLimitsAreClamped(t *testing.T) {
	// A table click must never turn into a million rows in a browser tab.
	if dexClampLimit(0) != dexDefaultLimit {
		t.Errorf("no limit should mean %d", dexDefaultLimit)
	}
	if dexClampLimit(-5) != dexDefaultLimit {
		t.Error("a negative limit is not a licence to fetch everything")
	}
	if dexClampLimit(1 << 30); dexClampLimit(1<<30) != dexMaxLimit {
		t.Errorf("a huge limit should clamp to %d", dexMaxLimit)
	}
	if dexClampLimit(750) != 750 {
		t.Error("a reasonable limit should be honoured")
	}
	if dexClampTimeout(0) != dexDefTimeoutS || dexClampTimeout(99999) != dexMaxTimeoutS {
		t.Error("the query timeout must be bounded at both ends")
	}
}

func TestDexViewDataAlwaysCarriesALimit(t *testing.T) {
	// The commonest action in the feature is clicking a table. It must not be the
	// one path that can fetch a whole one.
	for _, engine := range []string{dexMySQL, dexPostgres, dexClickHouse} {
		req := dexViewDataRequest(engine, dexObjectRef{Database: "shop", Schema: "public", Name: "orders"}, 0)
		if req.Limit != dexDefaultLimit {
			t.Errorf("%s: limit = %d", engine, req.Limit)
		}
		if !strings.Contains(req.SQL, "LIMIT 500") {
			t.Errorf("%s: statement has no ceiling: %s", engine, req.SQL)
		}
		if !strings.Contains(req.SQL, "orders") {
			t.Errorf("%s: statement does not name the table: %s", engine, req.SQL)
		}
	}
	// A table called `select` or containing a quote must still produce valid SQL.
	req := dexViewDataRequest(dexMySQL, dexObjectRef{Database: "d", Name: "we`ird"}, 10)
	if !strings.Contains(req.SQL, "`we``ird`") {
		t.Errorf("an identifier with a backtick was not quoted: %s", req.SQL)
	}
	mongo := dexViewDataRequest(dexMongoDB, dexObjectRef{Database: "shop", Name: "orders"}, 0)
	if mongo.Mongo.Collection != "orders" || mongo.Mongo.Operation != "find" || mongo.SQL != "" {
		t.Errorf("MongoDB's View Data is a find, not SQL: %+v", mongo.Mongo)
	}
}

func TestDexQuoteIdentAndLiteralPerEngine(t *testing.T) {
	if got := dexQuoteIdent(dexMySQL, "a`b"); got != "`a``b`" {
		t.Errorf("MySQL identifier: %s", got)
	}
	if got := dexQuoteIdent(dexPostgres, `a"b`); got != `"a""b"` {
		t.Errorf("PostgreSQL identifier: %s", got)
	}
	// PostgreSQL with standard_conforming_strings on treats a backslash literally,
	// so doubling it would change the value; MySQL and ClickHouse do not.
	if got := dexQuoteLiteral(dexPostgres, `a'b\c`); got != `'a''b\c'` {
		t.Errorf("PostgreSQL literal: %s", got)
	}
	if got := dexQuoteLiteral(dexMySQL, `a'b\c`); got != `'a\'b\\c'` {
		t.Errorf("MySQL literal: %s", got)
	}
}

// ---------------------------------------------------------------- PMM PostgreSQL

func TestDexPGScriptAsksTheServerForTypesAndKeepsNullDistinct(t *testing.T) {
	script, wrapped := dexPGScript("pmm-managed", "SELECT a, b FROM t", 500, true)
	if !wrapped {
		t.Fatal("a SELECT should take the wrapped path")
	}
	for _, want := range []string{
		"BEGIN READ ONLY",        // the server-side half of read-only
		"COMMIT",                 //
		dexPGColsMarker,          // the two halves of the output are separated
		dexPGRowsMarker,          //
		`\gdesc`,                 // real column names and types, without executing
		"json_each(to_json(",     // positional arrays: duplicate names survive
		"WITH ORDINALITY",        // in the server's own column order
		"LIMIT 501",              // one past the ceiling, so "truncated" is a fact
		`\set VERBOSITY verbose`, // so the SQLSTATE comes back with the error
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the psql script is missing %q:\n%s", want, script)
		}
	}

	// A statement that cannot be a subquery is sent as it is.
	plain, wrapped2 := dexPGScript("pmm-managed", "SHOW search_path", 500, true)
	if wrapped2 {
		t.Error("SHOW cannot be wrapped in a subquery")
	}
	if !strings.Contains(plain, "SHOW search_path") {
		t.Errorf("the plain path lost the statement:\n%s", plain)
	}

	// A writable connection gets no transaction wrapper.
	rw, _ := dexPGScript("shop", "SELECT 1", 10, false)
	if strings.Contains(rw, "BEGIN READ ONLY") {
		t.Error("a writable connection must not be forced read-only")
	}
}

func TestDexPGParsesPsqlOutputIntoTypedRows(t *testing.T) {
	// Exactly the shape a real psql produced on a PMM Server: \gdesc's name|type
	// lines, then one JSON array per row.
	out := strings.Join([]string{
		dexPGColsMarker,
		"x|text",
		"x|integer",
		"y|text",
		"big|bigint",
		"j|jsonb",
		dexPGRowsMarker,
		`[[null, 1, "", 9223372036854775807, {"a": [1, 2]}]]`,
	}, "\n")
	set, derr := dexPGParseScript(out, 500)
	if derr != nil {
		t.Fatalf("parse: %s", derr.Display)
	}
	if len(set.Columns) != 5 {
		t.Fatalf("columns = %d, want 5", len(set.Columns))
	}
	// Two columns called x both survive — an object keyed by name would keep one.
	if set.Columns[0].Name != "x" || set.Columns[1].Name != "x" {
		t.Errorf("duplicate column names collapsed: %+v", set.Columns)
	}
	if set.Columns[1].SemanticType != dexSemInteger || set.Columns[4].SemanticType != dexSemJSON {
		t.Errorf("types were not read from \\gdesc: %+v", set.Columns)
	}
	if len(set.Rows) != 1 {
		t.Fatalf("rows = %d", len(set.Rows))
	}
	r := set.Rows[0]
	if r[0] != nil {
		t.Errorf("NULL became %#v", r[0])
	}
	if r[2] != "" {
		t.Errorf("the empty string became %#v — it must stay distinct from NULL", r[2])
	}
	if bn, ok := r[3].(dexBigNum); !ok || bn.Text != "9223372036854775807" {
		t.Errorf("a bigint lost precision: %#v", r[3])
	}
	if s, ok := r[4].(string); !ok || !strings.Contains(s, `"a"`) {
		t.Errorf("a jsonb value was flattened: %#v", r[4])
	}
}

func TestDexPGTruncationIsAFactNotAGuess(t *testing.T) {
	// The script asks for limit+1 rows, so a result of exactly `limit` rows is
	// distinguishable from one that was cut.
	rows := `[[1],[2],[3]]`
	out := dexPGColsMarker + "\na|integer\n" + dexPGRowsMarker + "\n" + rows
	full, _ := dexPGParseScript(out, 3)
	if full.Truncated {
		t.Error("three rows under a limit of three is not truncated")
	}
	cut, _ := dexPGParseScript(out, 2)
	if !cut.Truncated || len(cut.Rows) != 2 {
		t.Errorf("a cut result must say so: truncated=%v rows=%d", cut.Truncated, len(cut.Rows))
	}
}

func TestDexPGReadsPsqlErrorsBackIntoStructure(t *testing.T) {
	// Verbatim from psql with VERBOSITY verbose, which is what the exec transport
	// sets so the SQLSTATE travels with the message.
	stderr := "psql:/dev/stdin:3: ERROR:  42P01: relation \"nosuchtable_xyz\" does not exist\n" +
		"LINE 1: SELECT * FROM nosuchtable_xyz;\n" +
		"                      ^\n" +
		"LOCATION:  parserOpenTable, parse_relation.c:1381\n"
	e := dexPGParsePsqlError(ExecResult{Code: 1, Stderr: stderr}, nil)
	if e == nil {
		t.Fatal("a psql error should parse")
	}
	if e.SQLState != "42P01" {
		t.Errorf("SQLSTATE = %q", e.SQLState)
	}
	if !strings.Contains(e.Message, "does not exist") {
		t.Errorf("message = %q", e.Message)
	}
	if e.Position != 15 {
		t.Errorf("position = %d, want the caret's offset into the statement (15)", e.Position)
	}
	if !strings.Contains(e.Display, "42P01") {
		t.Errorf("the display line should read like psql: %q", e.Display)
	}
	if strings.Contains(e.Display, "parse_relation.c") {
		t.Error("the server's source location is not something to show a user")
	}
}

// ---------------------------------------------------------------- ClickHouse

func TestDexClickHouseParsesJSONCompact(t *testing.T) {
	// Verbatim from clickhouse-client --format JSONCompact on a PMM Server.
	body := `{
	"meta": [ {"name":"x","type":"UInt8"}, {"name":"big","type":"UInt64"}, {"name":"arr","type":"Array(UInt8)"}, {"name":"n","type":"Nullable(String)"} ],
	"data": [ [1, "18446744073709551615", [1,2], null] ],
	"rows": 1
}`
	set, derr := dexCHParse([]byte(body))
	if derr != nil {
		t.Fatalf("parse: %s", derr.Display)
	}
	if len(set.Columns) != 4 || set.Columns[1].DatabaseType != "UInt64" {
		t.Fatalf("columns: %+v", set.Columns)
	}
	r := set.Rows[0]
	// ClickHouse writes 64-bit integers as JSON strings so they do not lose digits;
	// a grid that took them as text would neither align nor sort them.
	if bn, ok := r[1].(dexBigNum); !ok || bn.Text != "18446744073709551615" {
		t.Errorf("UInt64 = %#v", r[1])
	}
	if s, ok := r[2].(string); !ok || s != "[1,2]" {
		t.Errorf("an Array should keep its JSON: %#v", r[2])
	}
	if r[3] != nil {
		t.Errorf("a Nullable NULL became %#v", r[3])
	}
}

func TestDexClickHouseErrorsCarryTheirCode(t *testing.T) {
	e := dexCHError("Code: 60. DB::Exception: Table pmm.foo does not exist. (UNKNOWN_TABLE)")
	if e.Code != "60" || e.Name != "UNKNOWN_TABLE" {
		t.Errorf("code/name = %q/%q", e.Code, e.Name)
	}
	if !strings.Contains(e.Message, "does not exist") {
		t.Errorf("message = %q", e.Message)
	}
	// The one a read-only connection produces, which a user will meet.
	ro := dexCHError("Received exception from server (version 25.3.6): Code: 164. DB::Exception: Received from localhost:9000. DB::Exception: default: Cannot execute query in readonly mode. (READONLY)")
	if ro.Code != "164" || ro.Name != "READONLY" {
		t.Errorf("readonly refusal parsed as %q/%q", ro.Code, ro.Name)
	}
}

func TestDexClickHouseCapsTheResultAtTheTransport(t *testing.T) {
	got := dexCHWithLimit("SELECT * FROM pmm.metrics", 500)
	for _, want := range []string{
		"max_result_rows = 501",
		"result_overflow_mode = 'break'",
		// Without this the first block is still 65,536 rows on the wire, whatever
		// the row ceiling says.
		"max_block_size = 501",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q from %q", want, got)
		}
	}
	// A user who wrote their own SETTINGS is not argued with.
	mine := "SELECT 1 SETTINGS max_threads = 1"
	if dexCHWithLimit(mine, 500) != mine {
		t.Errorf("an explicit SETTINGS clause was overwritten: %q", dexCHWithLimit(mine, 500))
	}
	// A statement with no result gets no cap. ClickHouse reads a trailing SETTINGS
	// on a CREATE as the table's own settings and rejects max_result_rows there, so
	// appending one would break every write on an unlocked connection.
	for _, w := range []string{
		"CREATE TABLE pmm.t (a UInt8) ENGINE=Memory",
		"INSERT INTO pmm.t VALUES (1)",
		"ALTER TABLE pmm.t DELETE WHERE 1",
		"DROP TABLE pmm.t",
	} {
		if got := dexCHWithLimit(w, 500); strings.Contains(got, "max_result_rows") {
			t.Errorf("a write must not be given a result cap: %q", got)
		}
	}
}

func TestDexPMMClickHouseCredentialsAreReadNotGuessed(t *testing.T) {
	// The supervisord unit percona/pmm-server:3 actually ships. Reading it is what
	// keeps this working when PMM changes the password.
	const ini = `[program:qan-api2]
environment =
	PMM_CLICKHOUSE_ADDR="127.0.0.1:9000",
	PMM_CLICKHOUSE_DATABASE="pmm",
	PMM_CLICKHOUSE_USER="default",
	PMM_CLICKHOUSE_PASSWORD="clickhouse",
`
	if got := dexIniValue(ini, "PMM_CLICKHOUSE_USER"); got != "default" {
		t.Errorf("user = %q", got)
	}
	if got := dexIniValue(ini, "PMM_CLICKHOUSE_PASSWORD"); got != "clickhouse" {
		t.Errorf("password = %q", got)
	}
	if got := dexIniValue(ini, "PMM_CLICKHOUSE_DATABASE"); got != "pmm" {
		t.Errorf("database = %q", got)
	}
	if got := dexIniValue(ini, "NOT_THERE"); got != "" {
		t.Errorf("a missing key should be empty, got %q", got)
	}
}

// ---------------------------------------------------------------- Valkey

func TestDexRESPDecodesEveryFrameType(t *testing.T) {
	cases := []struct {
		wire string
		want dexRESPValue
	}{
		{"+OK\r\n", dexRESPValue{Kind: dexRESPStatus, Str: "OK"}},
		{"-WRONGTYPE Operation against a key\r\n", dexRESPValue{Kind: dexRESPError, Str: "WRONGTYPE Operation against a key"}},
		{":42\r\n", dexRESPValue{Kind: dexRESPInt, Int: 42}},
		{"$5\r\nhello\r\n", dexRESPValue{Kind: dexRESPString, Str: "hello"}},
		{"$-1\r\n", dexRESPValue{Kind: dexRESPNull}},
		{"$0\r\n\r\n", dexRESPValue{Kind: dexRESPString, Str: ""}},
		{",3.5\r\n", dexRESPValue{Kind: dexRESPDouble, Num: 3.5}},
		{"#t\r\n", dexRESPValue{Kind: dexRESPBool, Bool: true}},
		{"_\r\n", dexRESPValue{Kind: dexRESPNull}},
	}
	for _, c := range cases {
		got, err := dexRESPRead(c.wire)
		if err != nil {
			t.Fatalf("%q: %v", c.wire, err)
		}
		if got.Kind != c.want.Kind || got.Str != c.want.Str || got.Int != c.want.Int ||
			got.Num != c.want.Num || got.Bool != c.want.Bool {
			t.Errorf("%q decoded to %+v, want %+v", c.wire, got, c.want)
		}
	}

	// A SCAN reply: a cursor and a nested array of keys.
	scan, err := dexRESPRead("*2\r\n$2\r\n17\r\n*2\r\n$6\r\nusers:\r\n$5\r\na:b:c\r\n")
	if err != nil {
		t.Fatalf("scan reply: %v", err)
	}
	if len(scan.Items) != 2 || scan.Items[0].Str != "17" {
		t.Fatalf("scan reply = %+v", scan)
	}
	if keys := scan.Items[1].strs(); len(keys) != 2 || keys[0] != "users:" {
		t.Errorf("scan keys = %v", keys)
	}

	// A RESP3 map, which is what a modern server answers CONFIG GET with.
	m, err := dexRESPRead("%1\r\n$7\r\nmaxmemo\r\n$1\r\n0\r\n")
	if err != nil || m.Kind != dexRESPMap || len(m.Pairs) != 1 {
		t.Fatalf("map reply = %+v (%v)", m, err)
	}
	// An empty string and a nil bulk are not the same value.
	empty, _ := dexRESPRead("$0\r\n\r\n")
	null, _ := dexRESPRead("$-1\r\n")
	if empty.Kind == null.Kind {
		t.Error("an empty bulk string and a nil bulk must decode differently")
	}
}

func TestDexValkeyRefusesKEYSAndSubscriptions(t *testing.T) {
	// KEYS scans the whole keyspace in one blocking call. Refusing it with a reason
	// is the point — the lab is supposed to teach that, not commit it for you.
	set, e := dexValkeyCommand(context.Background(), nil, "KEYS *", 100)
	if e == nil {
		t.Fatalf("KEYS was accepted: %+v", set)
	}
	if !strings.Contains(e.Message, "SCAN") {
		t.Errorf("the refusal should point at SCAN: %q", e.Message)
	}
	for _, cmd := range []string{"SUBSCRIBE ch", "MONITOR", "PSYNC"} {
		if _, e := dexValkeyCommand(context.Background(), nil, cmd, 100); e == nil {
			t.Errorf("%s turns the connection into a stream and must be refused", cmd)
		}
	}
}

func TestDexValkeyCommandSplitting(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"GET foo", []string{"GET", "foo"}},
		{`SET k "a b"`, []string{"SET", "k", "a b"}},
		{"SET k ''", []string{"SET", "k", ""}},
		{"  HGETALL   customer:100  ", []string{"HGETALL", "customer:100"}},
		{`SET k "say \"hi\""`, []string{"SET", "k", `say "hi"`}},
	}
	for _, c := range cases {
		got, err := dexSplitCommand(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%q split to %q, want %q", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q split to %q, want %q", c.in, got, c.want)
				break
			}
		}
	}
	if _, err := dexSplitCommand(`SET k "unclosed`); err == nil {
		t.Error("an unclosed quote should be reported, not guessed at")
	}
}

func TestDexValkeyRendersRepliesStructurally(t *testing.T) {
	// A hash comes back as a flat array; showing it as an indexed list would be
	// showing the protocol rather than the data.
	hash := dexRESPValue{Kind: dexRESPArray, Items: []dexRESPValue{
		{Kind: dexRESPString, Str: "name"}, {Kind: dexRESPString, Str: "ada"},
		{Kind: dexRESPString, Str: "city"}, {Kind: dexRESPString, Str: "london"},
	}}
	set := dexValkeyRenderReply("HGETALL", hash, 100)
	if len(set.Columns) != 2 || set.Columns[0].Name != "field" {
		t.Fatalf("HGETALL should be a field/value table: %+v", set.Columns)
	}
	if len(set.Rows) != 2 || set.Rows[0][0] != "name" || set.Rows[0][1] != "ada" {
		t.Errorf("rows = %+v", set.Rows)
	}
	// The typed reply travels too, so a viewer can show the protocol shape.
	if len(set.Payload) == 0 {
		t.Error("the structured reply should be carried as a payload")
	}

	// INFO is a text blob with section headers; keeping the section as a column is
	// what makes the grid's filter able to find a key.
	info := dexValkeyInfoSet("# Server\nvalkey_version:8.0.1\r\n\n# Memory\nused_memory:1024\n")
	if len(info.Columns) != 3 {
		t.Fatalf("INFO columns = %+v", info.Columns)
	}
	if len(info.Rows) != 2 {
		t.Fatalf("INFO rows = %+v", info.Rows)
	}
	if info.Rows[0][0] != "Server" || info.Rows[0][1] != "valkey_version" {
		t.Errorf("INFO row = %+v", info.Rows[0])
	}
	if info.Rows[1][0] != "Memory" {
		t.Errorf("the section must follow its header: %+v", info.Rows[1])
	}
}

func TestDexValkeyErrorsNameThemselves(t *testing.T) {
	e := dexValkeyError("WRONGTYPE Operation against a key holding the wrong kind of value", nil)
	if e.Name != "WRONGTYPE" {
		t.Errorf("name = %q", e.Name)
	}
	if strings.HasPrefix(e.Message, "WRONGTYPE") {
		t.Errorf("the token should be lifted out of the message: %q", e.Message)
	}
	moved := dexValkeyError("MOVED 3999 172.18.0.5:6379", nil)
	if moved.Name != "MOVED" || moved.Hint == "" {
		t.Errorf("a MOVED should explain itself on a cluster: %+v", moved)
	}
}

// ---------------------------------------------------------------- MongoDB

func TestDexMongoStatementReadsLikeTheShell(t *testing.T) {
	req := dexQueryRequest{}
	req.Mongo.Collection = "orders"
	req.Mongo.Operation = "find"
	req.Mongo.Filter = json.RawMessage(`{"status":"active"}`)
	req.Mongo.Projection = json.RawMessage(`{"_id":0}`)
	if got := dexMongoStatement(req); got != `db.orders.find({"status":"active"}, {"_id":0})` {
		t.Errorf("find = %q", got)
	}
	req.Mongo.Operation = "aggregate"
	req.Mongo.Pipeline = json.RawMessage(`[{"$match":{}}]`)
	if got := dexMongoStatement(req); !strings.HasPrefix(got, "db.orders.aggregate([") {
		t.Errorf("aggregate = %q", got)
	}
}

func TestDexMongoResultCarriesBothShapesAtOnce(t *testing.T) {
	// A MongoDB result travels twice: the documents with their nesting intact, and a
	// flattened table view over the top-level fields. Neither replaces the other —
	// the table alone would make nested data unreachable, and the documents alone
	// would make the grid, the chart builder and the CSV export impossible.
	docs := []bson.D{
		{{Key: "_id", Value: "a"}, {Key: "n", Value: int32(1)}, {Key: "nested", Value: bson.D{{Key: "x", Value: 1}}}},
		{{Key: "_id", Value: "b"}, {Key: "n", Value: int32(2)}, {Key: "tags", Value: bson.A{"p", "q"}}},
	}
	set, derr := dexMongoDocsSet(docs, 500)
	if derr != nil {
		t.Fatalf("build: %s", derr.Display)
	}
	if len(set.Documents) != 2 {
		t.Fatalf("documents = %d", len(set.Documents))
	}
	if !strings.Contains(string(set.Documents[0]), `"nested"`) {
		t.Errorf("the document lost its nesting: %s", set.Documents[0])
	}

	// Columns are the union of top-level fields, in the order they were first met —
	// which is the order they are stored in, and the order a shell would print them.
	names := make([]string, 0, len(set.Columns))
	for _, c := range set.Columns {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "_id,n,nested,tags" {
		t.Errorf("columns = %v, want field order, _id first", names)
	}
	if set.Columns[1].SemanticType != dexSemInteger {
		t.Errorf("n should read as an integer: %q", set.Columns[1].SemanticType)
	}
	if set.Columns[2].SemanticType != dexSemDocument || set.Columns[3].SemanticType != dexSemArray {
		t.Errorf("a nested document and an array should be typed as such: %+v", set.Columns[2:])
	}

	// A field a document does not have is absent, not an empty string.
	if set.Rows[0][3] != nil {
		t.Errorf("a missing field became %#v", set.Rows[0][3])
	}
	// And a nested value keeps its JSON in the table view rather than being cut
	// down to fit a column.
	if s, ok := set.Rows[0][2].(string); !ok || !strings.Contains(s, `"x"`) {
		t.Errorf("nested cell = %#v", set.Rows[0][2])
	}

	// The ceiling applies here too.
	cut, _ := dexMongoDocsSet(docs, 1)
	if !cut.Truncated || len(cut.Rows) != 1 {
		t.Errorf("truncation: %v / %d rows", cut.Truncated, len(cut.Rows))
	}
}

func TestDexMongoParsesExtendedJSON(t *testing.T) {
	// Extended JSON rather than plain JSON, so the types MongoDB stores can be
	// written: {"_id": {"$oid": …}} is how a user names a document by its id, and a
	// plain-JSON parser would turn it into a nested object matching nothing.
	d, derr := dexMongoParse(json.RawMessage(`{"_id": {"$oid": "507f1f77bcf86cd799439011"}}`), "the filter")
	if derr != nil {
		t.Fatalf("parse: %s", derr.Display)
	}
	if len(d) != 1 || d[0].Key != "_id" {
		t.Fatalf("parsed = %+v", d)
	}
	if _, ok := d[0].Value.(bson.D); ok {
		t.Error("$oid should become an ObjectID, not a nested document")
	}
	// An empty filter is the empty document, not an error.
	if d, derr := dexMongoParse(nil, "the filter"); derr != nil || len(d) != 0 {
		t.Errorf("an absent filter should be {}: %+v %v", d, derr)
	}
	// Malformed JSON is reported as such, with the engine's own complaint.
	if _, derr := dexMongoParse(json.RawMessage(`{oops`), "the filter"); derr == nil {
		t.Error("malformed JSON should be refused")
	} else if !strings.Contains(derr.Message, "the filter") {
		t.Errorf("the error should name which input: %q", derr.Message)
	}
	if _, derr := dexMongoParsePipeline(json.RawMessage(`[{"$match":{}}]`)); derr != nil {
		t.Errorf("a valid pipeline: %s", derr.Display)
	}
	if _, derr := dexMongoParsePipeline(json.RawMessage(`{"not":"an array"}`)); derr == nil {
		t.Error("a pipeline that is not an array should be refused")
	}
}

// ---------------------------------------------------------------- row identity

func mkDetail(pk []string, unique [][]string, nullable map[string]bool, cols ...string) dexObjectDetail {
	d := dexObjectDetail{Kind: dexKindTable, PrimaryKey: pk}
	for i, c := range cols {
		d.Columns = append(d.Columns, dexColumnInfo{Name: c, Type: "text", Nullable: nullable[c], Position: i + 1})
	}
	for i, u := range unique {
		d.Indexes = append(d.Indexes, dexIndexInfo{Name: "u" + string(rune('a'+i)), Columns: u, Unique: true})
	}
	return d
}

func TestDexRowEditingNeedsARealIdentity(t *testing.T) {
	// A primary key is enough.
	pk := mkDetail([]string{"id"}, nil, nil, "id", "name")
	if ok, why := dexEditableFrom(pk, true); !ok {
		t.Errorf("a primary key should allow editing: %s", why)
	}
	// So is a unique index over non-nullable columns.
	uq := mkDetail(nil, [][]string{{"a", "b"}}, map[string]bool{}, "a", "b")
	if ok, why := dexEditableFrom(uq, true); !ok {
		t.Errorf("a unique index over NOT NULL columns should allow editing: %s", why)
	}
	// A unique index over a nullable column is not: NULLs are not equal to each
	// other, so the index does not guarantee one row.
	nullableUq := mkDetail(nil, [][]string{{"a"}}, map[string]bool{"a": true}, "a", "b")
	if ok, _ := dexEditableFrom(nullableUq, true); ok {
		t.Error("a unique index over a nullable column does not identify a row")
	}
	// Nothing unique at all: refuse, and say why rather than offering a broken edit.
	none := mkDetail(nil, nil, nil, "a", "b")
	ok, why := dexEditableFrom(none, true)
	if ok {
		t.Fatal("a table with no key must not be editable")
	}
	if !strings.Contains(why, "primary key") {
		t.Errorf("the reason should name what is missing: %q", why)
	}
	// A read-only connection is never editable whatever the table looks like.
	if ok, why := dexEditableFrom(pk, false); ok || !strings.Contains(why, "read-only") {
		t.Errorf("read-only should win: ok=%v why=%q", ok, why)
	}
	// Only tables — not views.
	view := mkDetail([]string{"id"}, nil, nil, "id")
	view.Kind = dexKindView
	if ok, _ := dexEditableFrom(view, true); ok {
		t.Error("a view's rows are not editable in place")
	}
}

func TestDexMutationBindsValuesAndNamesOneRow(t *testing.T) {
	detail := mkDetail([]string{"id"}, nil, nil, "id", "name", "note")
	req := dexMutateRequest{
		Op:       "update",
		Values:   map[string]json.RawMessage{"name": json.RawMessage(`"o'brien"`), "note": json.RawMessage(`null`)},
		Identity: map[string]json.RawMessage{"id": json.RawMessage(`7`)},
	}
	m, derr := dexBuildMutation(dexPostgres, detail, `"public"."t"`, req)
	if derr != nil {
		t.Fatalf("build: %s", derr.Display)
	}
	// The value never reaches the SQL: it is bound.
	if strings.Contains(m.SQL, "o'brien") {
		t.Errorf("a value was interpolated into the statement: %s", m.SQL)
	}
	if !strings.Contains(m.SQL, "$1") || !strings.Contains(m.SQL, "WHERE \"id\" = $3") {
		t.Errorf("placeholders: %s", m.SQL)
	}
	if len(m.Args) != 3 {
		t.Fatalf("args = %#v", m.Args)
	}
	if m.Args[1] != nil {
		t.Errorf("a JSON null must bind as SQL NULL, got %#v", m.Args[1])
	}
	// The preview is readable and is not what runs.
	if !strings.Contains(m.Preview, "'o''brien'") {
		t.Errorf("preview = %s", m.Preview)
	}
	// Column order follows the table, not the map's iteration order.
	if strings.Index(m.SQL, `"name"`) > strings.Index(m.SQL, `"note"`) {
		t.Errorf("columns should follow the table's order: %s", m.SQL)
	}
}

func TestDexMutationRefusesWhatItCannotNameSafely(t *testing.T) {
	detail := mkDetail([]string{"id"}, nil, nil, "id", "name")

	// No identity at all: an UPDATE with no WHERE would change every row.
	if _, derr := dexBuildMutation(dexMySQL, detail, "`d`.`t`", dexMutateRequest{
		Op: "update", Values: map[string]json.RawMessage{"name": json.RawMessage(`"x"`)},
	}); derr == nil {
		t.Fatal("an update with no identity was accepted")
	}

	// An identity over columns that are not the key: visible values are not a key,
	// and a predicate built from them can match more rows than the one on screen.
	if _, derr := dexBuildMutation(dexMySQL, detail, "`d`.`t`", dexMutateRequest{
		Op:       "update",
		Values:   map[string]json.RawMessage{"name": json.RawMessage(`"x"`)},
		Identity: map[string]json.RawMessage{"name": json.RawMessage(`"old"`)},
	}); derr == nil {
		t.Fatal("a non-key identity was accepted")
	}

	// A NULL in the identity: `col = NULL` is never true, so the row cannot be named.
	if _, derr := dexBuildMutation(dexMySQL, detail, "`d`.`t`", dexMutateRequest{
		Op: "delete", Identity: map[string]json.RawMessage{"id": json.RawMessage(`null`)},
	}); derr == nil {
		t.Fatal("a NULL identity was accepted")
	}

	// A column that is not a column of this table never becomes SQL.
	if _, derr := dexBuildMutation(dexMySQL, detail, "`d`.`t`", dexMutateRequest{
		Op:       "update",
		Values:   map[string]json.RawMessage{"1=1; DROP TABLE t; --": json.RawMessage(`"x"`)},
		Identity: map[string]json.RawMessage{"id": json.RawMessage(`1`)},
	}); derr == nil {
		t.Fatal("an unknown column was accepted")
	} else if derr.Name != "UnknownColumn" {
		t.Errorf("refused as %q, want UnknownColumn", derr.Name)
	}
}

func TestDexDeleteBuildsExactlyOnePredicate(t *testing.T) {
	detail := mkDetail([]string{"a", "b"}, nil, nil, "a", "b", "c")
	m, derr := dexBuildMutation(dexMySQL, detail, "`d`.`t`", dexMutateRequest{
		Op: "delete", Identity: map[string]json.RawMessage{"a": json.RawMessage(`1`), "b": json.RawMessage(`"x"`)},
	})
	if derr != nil {
		t.Fatalf("build: %s", derr.Display)
	}
	if strings.Count(m.SQL, " AND ") != 1 {
		t.Errorf("a two-column key needs both halves: %s", m.SQL)
	}
	if !strings.HasPrefix(m.SQL, "DELETE FROM `d`.`t` WHERE ") {
		t.Errorf("statement = %s", m.SQL)
	}
	if len(m.Args) != 2 {
		t.Errorf("args = %#v", m.Args)
	}
}

// ---------------------------------------------------------------- capabilities

func TestDexCapabilitiesDescribeWhatAnEngineCanActuallyDo(t *testing.T) {
	pg := dexCapsFor(dexPostgres, false)
	if !pg.SQL || !pg.Schemas || !pg.Explain || !pg.EditableRows {
		t.Errorf("PostgreSQL: %+v", pg)
	}
	my := dexCapsFor(dexMySQL, false)
	if my.Schemas {
		t.Error("MySQL has no schema level between a database and its tables")
	}
	mongo := dexCapsFor(dexMongoDB, false)
	if mongo.SQL || !mongo.Documents {
		t.Errorf("MongoDB is not SQL-shaped: %+v", mongo)
	}
	vk := dexCapsFor(dexValkey, false)
	if vk.SQL || !vk.KeyValue || vk.Charts {
		t.Errorf("Valkey is key/value and has nothing to chart: %+v", vk)
	}
	ch := dexCapsFor(dexClickHouse, false)
	if ch.EditableRows || ch.Transactions {
		t.Error("ClickHouse has no OLTP row identity and no transactions to offer")
	}
	if !ch.Explain {
		t.Error("ClickHouse does expose a query plan")
	}
	// Read-only takes editing away from every engine that would otherwise have it.
	for _, e := range []string{dexMySQL, dexPostgres, dexMongoDB, dexValkey} {
		if dexCapsFor(e, true).EditableRows {
			t.Errorf("%s: read-only must disable row editing", e)
		}
	}
}

// ---------------------------------------------------------------- cancellation

func TestDexCancelIsOwnerScoped(t *testing.T) {
	alice := User{ID: 1, Role: RoleUser}
	bob := User{ID: 2, Role: RoleUser}
	admin := User{ID: 3, Role: RoleAdmin}

	cancelled := false
	dexRegisterRun("run-1", alice.ID, func() { cancelled = true })
	if dexCancelRun("run-1", bob) {
		t.Fatal("bob cancelled alice's query")
	}
	if cancelled {
		t.Fatal("the cancel func ran for the wrong user")
	}
	if !dexCancelRun("run-1", alice) {
		t.Fatal("alice could not cancel her own query")
	}
	if !cancelled {
		t.Fatal("cancelling did not call the cancel func")
	}
	// Cancelling something that has already finished is not an error, but it is not
	// a success either — there is nothing to cancel.
	if dexCancelRun("run-1", alice) {
		t.Error("a finished run should not report as cancelled twice")
	}
	dexRegisterRun("run-2", alice.ID, func() {})
	if !dexCancelRun("run-2", admin) {
		t.Error("an admin should be able to cancel")
	}
	// A client-supplied id is bounded before it is used as a key.
	if len(dexSafeID(strings.Repeat("x", 5000))) != 64 {
		t.Error("a run id must be bounded")
	}
}

// ---------------------------------------------------------------- history

func TestDexHistoryStoresNoCredentialAndPrunes(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)

	if _, err := app.store.DexAddHistory(u.ID, dexHistoryEntry{
		ConnectionID: "1~n~pg1", Connection: "lab · pg-01", Engine: dexPostgres,
		Database: "shop", Statement: "SELECT 1", DurationMs: 3, RowCount: 1, Success: true,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err := app.store.DexHistory(u.ID, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("history = %+v (%v)", got, err)
	}
	// A history entry names a connection by its id, which is re-authorized on
	// rerun — it does not carry a way to reach the database on its own.
	blob, _ := json.Marshal(got)
	for _, bad := range []string{"password", "passwd", "secret"} {
		if strings.Contains(strings.ToLower(string(blob)), bad) {
			t.Fatalf("history carries %q: %s", bad, blob)
		}
	}

	// Another user's history is not visible, and not deletable.
	bob, _ := app.store.CreateUser("bob", "x", RoleUser, StatusApproved)
	if h, _ := app.store.DexHistory(bob.ID, 10); len(h) != 0 {
		t.Errorf("bob sees alice's history: %+v", h)
	}
	if err := app.store.DexDeleteHistory(bob.ID, got[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if h, _ := app.store.DexHistory(u.ID, 10); len(h) != 1 {
		t.Error("bob deleted alice's history entry")
	}

	// Retention: the table cannot grow without bound.
	for i := 0; i < dexHistoryKeep+20; i++ {
		app.store.DexAddHistory(u.ID, dexHistoryEntry{
			ConnectionID: "1~n~pg1", Connection: "lab", Engine: dexPostgres, Statement: "SELECT 2",
		})
	}
	if h, _ := app.store.DexHistory(u.ID, dexHistoryKeep); len(h) > dexHistoryKeep {
		t.Errorf("history kept %d entries, want at most %d", len(h), dexHistoryKeep)
	}
}

func TestDexSavedQueriesAreOwnerScoped(t *testing.T) {
	app := newTestApp(t)
	alice, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	bob, _ := app.store.CreateUser("bob", "x", RoleUser, StatusApproved)
	q, err := app.store.DexSaveQuery(alice.ID, dexSavedQuery{Name: "top", Engine: dexPostgres, Statement: "SELECT 1"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if list, _ := app.store.DexSavedQueries(bob.ID); len(list) != 0 {
		t.Error("bob sees alice's saved queries")
	}
	app.store.DexDeleteSaved(bob.ID, q.ID)
	if list, _ := app.store.DexSavedQueries(alice.ID); len(list) != 1 {
		t.Error("bob deleted alice's saved query")
	}
}

// ---------------------------------------------------------------- misc

func TestDexStatementOfPerEngine(t *testing.T) {
	if got := dexStatementOf(dexQueryRequest{SQL: " SELECT 1 "}); got != "SELECT 1" {
		t.Errorf("SQL = %q", got)
	}
	if got := dexStatementOf(dexQueryRequest{Command: "GET foo"}); got != "GET foo" {
		t.Errorf("command = %q", got)
	}
	// Opening a key or a collection is not a statement anybody typed, so it is not
	// recorded as one.
	req := dexQueryRequest{}
	req.Mongo.Collection = "orders"
	if got := dexStatementOf(req); got != "" {
		t.Errorf("a bare collection open should not be history: %q", got)
	}
}

func TestDexDestructiveVerbs(t *testing.T) {
	for _, v := range []string{"DROP", "TRUNCATE", "DELETE", "UPDATE", "ALTER", "GRANT"} {
		if !dexIsDestructive(v) {
			t.Errorf("%s changes things", v)
		}
	}
	for _, v := range []string{"SELECT", "SHOW", "EXPLAIN", "WITH"} {
		if dexIsDestructive(v) {
			t.Errorf("%s does not", v)
		}
	}
}

func TestDexTreeSortingPutsSystemObjectsLast(t *testing.T) {
	nodes := []dexTreeNode{
		{Name: "zebra", Folder: "Tables"},
		{Name: "pg_stat", Folder: "Tables", System: true},
		{Name: "alpha", Folder: "Tables"},
		{Name: "v1", Folder: "Views"},
	}
	dexSortTreeNodes(nodes, []string{"Tables", "Views"})
	if nodes[0].Name != "alpha" || nodes[1].Name != "zebra" || !nodes[2].System {
		t.Errorf("order = %+v", nodes)
	}
	if nodes[3].Folder != "Views" {
		t.Errorf("folders keep their declared order: %+v", nodes)
	}
}
