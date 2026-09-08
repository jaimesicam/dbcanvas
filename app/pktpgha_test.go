package main

// pktpgha_test.go — the non-PostgreSQL traffic around a PostgreSQL cluster: Patroni's
// REST API, etcd, and the PostgreSQL/Patroni server logs.
//
// The live Patroni cluster is what these are checked against. Its 20-second capture
// held 55 Patroni REST frames, 19 etcd client frames and 596 etcd raft frames next to
// 18 000 PostgreSQL ones — the health-check and lock-renewal traffic is a constant
// background hum, and being able to name it (rather than call it "TCP data") is what
// makes the rest of a capture readable.

import (
	"strings"
	"testing"
	"time"
)

// haCap builds a capture of one exchange on a cluster port, with the port's role
// declared the way a real capture of a Patroni node declares it.
func haCap(t *testing.T, port int, exchanges ...[]byte) *pktDecoded {
	t.Helper()
	b := newPcap(pktLinkEther)
	cseq, sseq := uint32(1000), uint32(5000)
	b.frame(0, ethIPv4TCP(cliIP, srvIP, cliPort, port, cseq, 0, tcpSYN, 64240, nil))
	cseq++
	b.frame(time.Millisecond, ethIPv4TCP(srvIP, cliIP, port, cliPort, sseq, cseq, tcpSYN|tcpACK, 64240, nil))
	sseq++
	for i, payload := range exchanges {
		at := time.Duration(i+2) * time.Millisecond
		if i%2 == 0 { // client
			b.frame(at, ethIPv4TCP(cliIP, srvIP, cliPort, port, cseq, sseq, tcpACK|tcpPSH, 64240, payload))
			cseq += uint32(len(payload))
		} else {
			b.frame(at, ethIPv4TCP(srvIP, cliIP, port, cliPort, sseq, cseq, tcpACK|tcpPSH, 64240, payload))
			sseq += uint32(len(payload))
		}
	}
	d, err := pktDecode(b.buf, pktDecodeOpts{
		ServerPort: pgClientPort, Engine: pktEnginePostgres,
		PortRoles: pktPGPortRoles(pgClientPort),
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return d
}

func TestPatroniRESTDecodes(t *testing.T) {
	// HAProxy's write-port health check, answered by a replica: 503 is the correct
	// answer, not a fault, and must not be flagged.
	d := haCap(t, patroniRESTPort,
		[]byte("GET /primary HTTP/1.1\r\nHost: patroni02:8008\r\nUser-Agent: HAProxy\r\n\r\n"),
		[]byte("HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\n\r\n{\"state\": \"running\", \"role\": \"replica\", \"timeline\": 3}"))
	if !infoHas(d, "GET /primary") {
		t.Error("the request line was not decoded")
	}
	if !infoHas(d, `"am I the leader?"`) {
		t.Error("the endpoint's purpose was not explained")
	}
	if !infoHas(d, "HTTP 503 Service Unavailable — this member is not the leader") {
		t.Error("503 on /primary was not read as 'not the leader'")
	}
	if !infoHas(d, "role=replica") {
		t.Error("the JSON body's role was not read")
	}
	for _, p := range d.Packets {
		if len(p.Issues) > 0 {
			t.Errorf("a normal health check raised %q", p.Issues)
		}
	}
	if d.Streams[0].RoleLabel != "Patroni/REST" {
		t.Errorf("role label = %q", d.Streams[0].RoleLabel)
	}
}

func TestPatroniRESTLeaderAndFailure(t *testing.T) {
	d := haCap(t, patroniRESTPort,
		[]byte("GET /primary HTTP/1.1\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\n\r\n{\"state\": \"running\", \"role\": \"primary\", \"timeline\": 3}"))
	if !infoHas(d, "this member IS the leader") {
		t.Error("200 on /primary was not read as 'is the leader'")
	}

	// A 500 is Patroni itself failing, which is worth a line: HAProxy routes away and
	// patronictl cannot drive the member either.
	d = haCap(t, patroniRESTPort,
		[]byte("GET /cluster HTTP/1.1\r\n\r\n"),
		[]byte("HTTP/1.1 500 Internal Server Error\r\n\r\n"))
	if !issueHas(d, "Patroni REST returned 500") {
		t.Error("a Patroni 500 was not flagged")
	}

	// A switchover is a real action, and the endpoint says so.
	d = haCap(t, patroniRESTPort,
		[]byte("POST /switchover HTTP/1.1\r\nContent-Type: application/json\r\n\r\n{\"leader\":\"patroni01\",\"candidate\":\"patroni02\"}"),
		[]byte("HTTP/1.1 200 OK\r\n\r\nSuccessfully switched over to \"patroni02\""))
	if !infoHas(d, "a controlled role change") {
		t.Error("POST /switchover was not explained")
	}
}

func TestEtcdDecodes(t *testing.T) {
	// Patroni's etcd3 client talks to the gRPC gateway over HTTP/1.1, which is what the
	// live cluster showed — so the paths are readable and worth naming.
	d := haCap(t, etcdClientPort,
		[]byte("POST /v3/lease/keepalive HTTP/1.1\r\nHost: patroni01:2379\r\nContent-Length: 29\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n"))
	if !infoHas(d, "POST /v3/lease/keepalive") {
		t.Error("the etcd path was not decoded")
	}
	if !infoHas(d, "the TTL behind the leader lock") {
		t.Error("the lease's meaning was not explained")
	}

	// An etcd failure is the reason a Patroni cluster fails over, and it happens
	// seconds before anything visible on the PostgreSQL side.
	d = haCap(t, etcdClientPort,
		[]byte("POST /v3/kv/txn HTTP/1.1\r\n\r\n"),
		[]byte("HTTP/1.1 503 Service Unavailable\r\n\r\netcdserver: request timed out"))
	if !issueHas(d, "gives up the Patroni leader lock and demotes itself") {
		t.Error("an etcd 5xx was not connected to its consequence")
	}
}

func TestEtcdHTTP2AndRaft(t *testing.T) {
	// The HTTP/2 preface, then a HEADERS frame carrying a gRPC method as a literal.
	preface := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	hbody := []byte("\x00\x05:path/etcdserverpb.KV/Range\x00")
	headers := []byte{0, 0, byte(len(hbody)), 0x01, 0x04, 0, 0, 0, 1}
	headers = append(headers, hbody...)
	d := haCap(t, etcdClientPort, preface, []byte("HTTP/1.1 200 OK\r\n\r\n"), headers)
	if !infoHas(d, "HTTP/2 connection preface") {
		t.Error("the HTTP/2 preface was not recognised")
	}
	if !infoHas(d, "/etcdserverpb.KV/Range") {
		t.Error("a gRPC method visible in a HEADERS frame was not reported")
	}

	// Raft peer traffic: a long-lived stream whose request line predates the capture,
	// so all that can honestly be said is what it is and how much of it there is.
	d = haCap(t, etcdPeerPort, []byte{0x08, 0x01, 0x10, 0x02, 0x18, 0x03, 0x20, 0x04, 0x28, 0x05})
	if !infoHas(d, "etcd/raft data") {
		t.Errorf("raft traffic was not labelled: %q", d.Packets[len(d.Packets)-1].Info)
	}
	if d.Streams[0].RoleLabel != "etcd/raft" {
		t.Errorf("role label = %q", d.Streams[0].RoleLabel)
	}
}

// TestPGPortRolesCoverCluster checks the port map a Patroni capture is taken with: the
// PostgreSQL protocol on every port that carries it (including through pgBouncer and
// HAProxy), and the cluster's own protocols on theirs.
func TestPGPortRolesCoverCluster(t *testing.T) {
	roles := pktPGPortRoles(pgClientPort)
	for port, want := range map[int]string{
		pgClientPort:    pktRolePostgres,
		pgBouncerPort:   pktRolePostgres,
		pgProxyRWPort:   pktRolePostgres,
		pgProxyROPort:   pktRolePostgres,
		patroniRESTPort: pktRolePatroniREST,
		etcdClientPort:  pktRoleEtcdClient,
		etcdPeerPort:    pktRoleEtcdPeer,
	} {
		if roles[port] != want {
			t.Errorf("port %d → %q, want %q", port, roles[port], want)
		}
	}
	// An All-in-One instance is on its slot's port, and that port must still be the
	// PostgreSQL one.
	if r := pktPGPortRoles(13002)[13002]; r != pktRolePostgres {
		t.Errorf("a non-default client port got role %q", r)
	}
	// Labels exist for every role, in both engines, and neither answers for the other.
	for _, role := range []string{pktRolePostgres, pktRolePatroniREST, pktRoleEtcdClient, pktRoleEtcdPeer} {
		if pktPGRoleLabel(role) == "" || pktPGRoleDescription(role) == "" {
			t.Errorf("role %q has no label or description", role)
		}
		if pktRoleAnyLabel(role) != pktPGRoleLabel(role) {
			t.Errorf("pktRoleAnyLabel(%q) = %q", role, pktRoleAnyLabel(role))
		}
	}
	if pktPGRoleLabel(pktRoleGaleraGCS) != "" {
		t.Error("the PostgreSQL label function answered for a Galera role")
	}
	if pktRoleAnyLabel(pktRoleGaleraIST) != "Galera/IST" {
		t.Error("pktRoleAnyLabel lost a Galera label")
	}
}

// ---------------------------------------------------------------- server log

func TestPGServerLogClassification(t *testing.T) {
	log := strings.Join([]string{
		`2026-08-04 06:02:11.032 UTC [2596] LOG:  starting PostgreSQL 16.14 - Percona Distribution on x86_64-pc-linux-gnu`,
		`2026-08-04 06:02:11.108 UTC [2596] LOG:  database system is ready to accept connections`,
		`2026-08-04 06:15:44.142 UTC [2948] ERROR:  duplicate key value violates unique constraint "reservation_requests_pkey"`,
		`2026-08-04 06:15:44.142 UTC [2948] DETAIL:  Key (request_id)=(req-1785824142925419679) already exists.`,
		`2026-08-04 06:15:44.142 UTC [2948] STATEMENT:  INSERT INTO reservation_requests (request_id, created_at) VALUES ($1,$2)`,
		`2026-08-04 06:16:00.001 UTC [3001] FATAL:  password authentication failed for user "bob"`,
		`2026-08-04 06:16:00.002 UTC [3002] LOG:  incomplete startup packet`,
		`2026-08-04 06:16:01.500 UTC [3003] LOG:  could not receive data from client: Connection reset by peer`,
		`2026-08-04 06:16:02.000 UTC [3004] FATAL:  sorry, too many clients already`,
		`2026-08-04 06:16:03.000 UTC [3005] FATAL:  no pg_hba.conf entry for host "10.0.0.9", user "eve", database "rental", no encryption`,
		`2026-08-04 06:16:04.000 UTC [3006] LOG:  started streaming WAL from primary at 0/4000000 on timeline 1`,
		`2026-08-04 06:16:05.000 UTC [3007] ERROR:  requested WAL segment 000000010000000000000004 has already been removed`,
		`2026-08-04 06:16:06.000 UTC [3008] LOG:  received fast shutdown request`,
		`2026-08-04 06:16:07.000 UTC [3009] WARNING:  checkpoints are occurring too frequently (12 seconds apart)`,
		`2026-08-04 06:16:08.000 UTC [3010] ERROR:  canceling statement due to conflict with recovery`,
		`2026-08-04 06:16:09,000 INFO: no action. I am (patroni01), the leader with the lock`,
		`2026-08-04 06:16:10,000 WARNING: Loop time exceeded, rescheduling immediately.`,
		`2026-08-04 06:16:11,000 ERROR: Error communicating with DCS`,
		`this line is not a log record at all`,
	}, "\n")

	entries := pktParseServerLog([]byte(log), pktEnginePostgres)
	// DETAIL and STATEMENT fold into the ERROR above them, and the unrecognisable line
	// is dropped: 19 lines in, 16 records out.
	if len(entries) != 16 {
		t.Fatalf("got %d entries, want 16: %+v", len(entries), entries)
	}
	byLabel := map[string]pktLogEntry{}
	for _, e := range entries {
		byLabel[e.Label] = e
	}
	for _, want := range []struct {
		label string
		class pktLogClass
	}{
		{"Server startup", pktLogLifecycle},
		{"Ready to accept connections", pktLogLifecycle},
		{"Constraint violation", pktLogOther},
		{"Password authentication failed", pktLogAuth},
		{"Incomplete startup packet (health check or port probe)", pktLogAbort},
		{"Client connection reset mid-statement", pktLogAbort},
		{"Too many clients (max_connections reached)", pktLogAuth},
		{"No pg_hba.conf entry for this host/user/database", pktLogAuth},
		{"Started streaming WAL", pktLogRepl},
		{"Requested WAL segment already removed — the standby cannot catch up", pktLogRepl},
		{"Shutdown requested", pktLogLifecycle},
		{"Patroni: cannot reach the DCS (etcd)", pktLogCluster},
		{"Checkpoints too frequent — max_wal_size is too small for this write rate", pktLogOther},
		{"Query cancelled by recovery conflict (standby)", pktLogRepl},
		{"Patroni: leader, holding the lock", pktLogCluster},
		{"Patroni: loop time exceeded — the member is too slow to renew its lease reliably", pktLogCluster},
	} {
		e, ok := byLabel[want.label]
		if !ok {
			t.Errorf("no record labelled %q", want.label)
			continue
		}
		if e.Class != want.class {
			t.Errorf("%q classified %q, want %q", want.label, e.Class, want.class)
		}
		if e.TS == 0 {
			t.Errorf("%q has no parsed timestamp", want.label)
		}
	}
	// The STATEMENT continuation carries the SQL, and it belongs to the ERROR above it
	// rather than being a record of its own.
	ce := byLabel["Constraint violation"]
	if !strings.Contains(ce.Message, "STATEMENT: INSERT INTO reservation_requests") {
		t.Errorf("the failing statement was not folded into its error: %q", ce.Message)
	}
	if !strings.Contains(ce.Reason, "already exists") {
		t.Errorf("DETAIL was not kept as the reason: %q", ce.Reason)
	}
}

// TestPGLogTimestampZones covers the mistake the MySQL reader made first: a log written
// with a zone that is not UTC must still land at the right instant, because the whole
// point is lining records up against a capture.
func TestPGLogTimestampZones(t *testing.T) {
	// The same instant, written three ways.
	lines := []string{
		`2026-08-04 06:00:00.000 UTC [1] LOG:  listening on IPv4 address "0.0.0.0", port 5432`,
		`2026-08-04 14:00:00.000 +08 [1] LOG:  listening on IPv4 address "0.0.0.0", port 5432`,
		`2026-08-04 14:00:00.000 +0800 [1] LOG:  listening on IPv4 address "0.0.0.0", port 5432`,
	}
	var got []float64
	for _, l := range lines {
		e, _, ok := pktClassifyPGLogLine(l)
		if !ok {
			t.Fatalf("not recognised: %s", l)
		}
		got = append(got, e.TS)
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[0] {
			t.Errorf("line %d parsed to %v, want %v (the same instant written differently)", i, got[i], got[0])
		}
	}
	// A user@database prefix and a log-line number must not stop a line being read.
	for _, l := range []string{
		`2026-08-04 06:00:00.000 UTC [2948] carsim@rental LOG:  statement: BEGIN`,
		`2026-08-04 06:00:00.000 UTC [2948] [7-1] postgres@postgres FATAL:  terminating connection due to administrator command`,
	} {
		if _, _, ok := pktClassifyPGLogLine(l); !ok {
			t.Errorf("a common log_line_prefix was rejected: %s", l)
		}
	}
}

// TestPGLogEngineSniff covers the upload path: whoever has a log has no reason to also
// say which product wrote it.
func TestPGLogEngineSniff(t *testing.T) {
	pg := "2026-08-04 06:02:11.032 UTC [2596] LOG:  database system is ready to accept connections\n"
	my := "2026-08-03T19:19:01.501234Z 12 [Note] [MY-010914] [Server] Aborted connection 12 to db: 'x' user: 'y' host: 'h' (Got timeout reading communication packets).\n"
	if got := pktSniffLogEngine([]byte(strings.Repeat(pg, 6))); got != pktEnginePostgres {
		t.Errorf("a PostgreSQL log sniffed as %q", got)
	}
	if got := pktSniffLogEngine([]byte(strings.Repeat(my, 6))); got != pktEngineMySQL {
		t.Errorf("a MySQL log sniffed as %q", got)
	}
	// Sniffing happens when the caller passes no engine, and the result is real
	// classified records rather than an empty list.
	entries := pktParseServerLog([]byte(strings.Repeat(pg, 3)), "")
	if len(entries) != 3 || entries[0].Class != pktLogLifecycle {
		t.Errorf("auto-detected parse gave %d entries, first class %q", len(entries), entries[0].Class)
	}
}

// TestPGAbortStatsSkipped: MySQL's Aborted_clients/log_error_verbosity question does not
// exist for PostgreSQL, and the panel should say so instead of showing empty counters as
// if they meant something.
func TestPGAbortStatsSkipped(t *testing.T) {
	var a App
	st := a.pktAbortStatsFor(nil, "container", pktEnginePostgres, "u", "p")
	if st.Hint == "" || !strings.Contains(st.Hint, "unconditionally") {
		t.Errorf("no explanation for the absent counters: %+v", st)
	}
	if len(st.Counters) != 0 {
		t.Errorf("counters invented for PostgreSQL: %+v", st.Counters)
	}
}
