package main

// pktevents_test.go — the event catalogue: which MySQL communication failures the
// tool recognises, and where each one is visible.
//
// MySQL's network errors split three ways and the tests follow that split, because
// the split is the substance:
//
//	ERR packets (1xxx)   on the wire → pktErrCatalog names them
//	client codes (2xxx)  never on the wire → only the evidence is, and
//	                     TestPktClientSideEvidence pins what that evidence looks like
//	log records (MY-…)   never on the wire at all → pktserverlog.go reads them
//
// Every case below was either provoked against a live Percona Server 8.0.46 node or
// built from MySQL's documented log format.

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// Every server-side communication and handshake error in MySQL's documented list must
// be named rather than shown as a bare number — that is the difference between
// "MySQL error 1156" and "packets arrived out of order, the connection is broken".
func TestPktErrorCatalogCoversCommunicationErrors(t *testing.T) {
	// The communication errors (MySQL's "Core network and communication errors").
	for _, code := range []int{1152, 1153, 1154, 1155, 1156, 1157, 1158, 1159, 1160, 1161, 1184, 1835} {
		e, ok := pktErrCatalog[code]
		if !ok {
			t.Errorf("communication error %d is not in the catalog", code)
			continue
		}
		if e.Class != pktErrNet {
			t.Errorf("%d (%s) is classed %q, want network", code, e.Sym, e.Class)
		}
		if !pktErrIsSevere(code) {
			t.Errorf("%d should count as severe", code)
		}
		if got := pktErrIssue(code, "whatever the server said"); !strings.Contains(got, e.Sym) ||
			!strings.Contains(got, e.Label) {
			t.Errorf("%d issue text %q omits its symbol or label", code, got)
		}
	}
	// Connection establishment and handshake.
	for code, wantClass := range map[int]pktErrClass{
		1040: pktErrLimit, 1042: pktErrNet, 1043: pktErrAuth, 1045: pktErrAuth,
		1047: pktErrNet, 1053: pktErrNet, 1129: pktErrAuth, 1130: pktErrAuth,
		1203: pktErrLimit,
	} {
		e, ok := pktErrCatalog[code]
		if !ok {
			t.Errorf("handshake error %d is not in the catalog", code)
			continue
		}
		if e.Class != wantClass {
			t.Errorf("%d (%s) is classed %q, want %q", code, e.Sym, e.Class, wantClass)
		}
	}
	// Replication transport.
	for _, code := range []int{1189, 1190} {
		if e, ok := pktErrCatalog[code]; !ok || e.Class != pktErrTopo {
			t.Errorf("replication transport error %d: %+v (ok=%v)", code, e, ok)
		}
	}
	// A code nobody catalogued is still reported, with its text.
	if got := pktErrIssue(4242, "something new"); !strings.Contains(got, "4242") ||
		!strings.Contains(got, "something new") {
		t.Errorf("uncatalogued error text = %q", got)
	}
	// The 2xxx client codes must NOT be in the catalog: they never arrive in an ERR
	// packet, and pretending otherwise would let the decoder claim it saw one.
	for _, code := range []int{2002, 2003, 2006, 2013, 2026, 2027, 2055} {
		if e, ok := pktErrCatalog[code]; ok {
			t.Errorf("client-side code %d must not be catalogued as a server error: %+v", code, e)
		}
	}
}

// A capture cannot see a client's own error code, but it can see what the client saw.
// These are the four shapes that produce 2003 / 2006 / 2013 / 2026.
func TestPktClientSideEvidence(t *testing.T) {
	t.Run("refused connection (2003)", func(t *testing.T) {
		b := newPcap(pktLinkEther)
		b.frame(0, c2s(1000, 0, tcpSYN, nil))
		b.frame(time.Millisecond, s2c(0, 1001, tcpRST|tcpACK, nil))
		d := decodeCap(t, b)
		if p := packetNo(d, 2); !p.hasIssue("Connection refused") {
			t.Errorf("a RST answering a SYN should read as refused: %v", p.Issues)
		}
	})

	t.Run("unanswered connection attempt (2003)", func(t *testing.T) {
		b := newPcap(pktLinkEther)
		b.frame(0, c2s(1000, 0, tcpSYN, nil))
		b.frame(time.Second, c2s(1000, 0, tcpSYN, nil)) // the client's own retry
		d := decodeCap(t, b)
		if p := packetNo(d, 1); !p.hasIssue("Connection attempt unanswered") {
			t.Errorf("a SYN with no SYN,ACK should be flagged: %v", p.Issues)
		}
	})

	t.Run("server hangs up mid-query (2013 / 2006)", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			flags uint8
			want  string
		}{
			{"reset", tcpRST | tcpACK, "2013 CR_SERVER_LOST"},
			{"fin", tcpFIN | tcpACK, "2006 CR_SERVER_GONE_ERROR"},
		} {
			b := newPcap(pktLinkEther)
			b.frame(0, c2s(1, 0, tcpSYN, nil))
			b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
			b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
			resp := append(make([]byte, 32), []byte("app\x00")...)
			binary.LittleEndian.PutUint32(resp, 0x000a8285)
			b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
			b.frame(4*time.Millisecond, s2c(100, 40, tcpACK|tcpPSH, okPacket(2, 0, 0)))
			q := append([]byte{comQuery}, []byte("SELECT SLEEP(30)")...)
			b.frame(10*time.Millisecond, c2s(40, 110, tcpACK|tcpPSH, mysqlPkt(0, q)))
			// …and the server goes away without answering.
			b.frame(3*time.Second, s2c(110, 61, tc.flags, nil))
			d := decodeCap(t, b)
			p := packetNo(d, 7)
			if p == nil || !p.hasIssue("Server closed the connection") {
				t.Errorf("%s: expected a server-hangup issue, got %v", tc.name, p.Issues)
				continue
			}
			found := false
			for _, is := range p.Issues {
				if strings.Contains(is, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: issues %v should name %s", tc.name, p.Issues, tc.want)
			}
		}
	})

	t.Run("TLS alert (2026)", func(t *testing.T) {
		b := newPcap(pktLinkEther)
		b.frame(0, c2s(1, 0, tcpSYN, nil))
		b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
		b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
		ssl := make([]byte, 32)
		binary.LittleEndian.PutUint32(ssl, 0x000a8285|capSSL)
		b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, ssl)))
		b.frame(4*time.Millisecond, c2s(38, 100, tcpACK|tcpPSH,
			[]byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0, 0, 0, 0}))
		// The server rejects the handshake with a fatal alert.
		b.frame(6*time.Millisecond, s2c(100, 48, tcpACK|tcpPSH,
			[]byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x28}))
		d := decodeCap(t, b)
		if p := packetNo(d, 6); !p.hasIssue("TLS alert") {
			t.Errorf("a TLS alert should be flagged with its client code: %v (%s)", p.Issues, p.Info)
		}
	})
}

// A compressed connection is unreadable for the same reason an encrypted one is, and
// the tool has to say so rather than emit garbage — the errors that live here are 1157
// on the server and 2065 / 2066 on the client.
func TestPktCompressedConnectionIsDeclared(t *testing.T) {
	b := newPcap(pktLinkEther)
	b.frame(0, c2s(1, 0, tcpSYN, nil))
	b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
	b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
	resp := make([]byte, 32)
	binary.LittleEndian.PutUint32(resp, 0x000a8285|capCompress)
	resp = append(resp, []byte("app")...)
	resp = append(resp, 0)
	b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
	// Whatever follows is zlib-framed, not MySQL packets.
	b.frame(5*time.Millisecond, c2s(80, 100, tcpACK|tcpPSH, []byte{0x78, 0x9c, 0x01, 0x02, 0x03, 0x04}))

	d := decodeCap(t, b)
	if p := packetNo(d, 4); !p.hasIssue("Compressed protocol") {
		t.Errorf("the handshake should declare compression: %v (%s)", p.Issues, p.Info)
	}
	p := packetNo(d, 5)
	if p.Proto != "MySQL/compressed" || p.Status != "Compressed" {
		t.Errorf("post-handshake frame: proto=%s status=%s info=%q", p.Proto, p.Status, p.Info)
	}
	if p.Query != "" {
		t.Errorf("nothing should be decoded out of a compressed stream: %q", p.Query)
	}
}

// caching_sha2_password — the default on MySQL 8 — sends an AuthMoreData packet that
// begins with 0x01, which is also a length-encoded column count of 1. Reading it as a
// result set is what made every such login decode as a bogus one-column answer.
func TestPktAuthPhasePackets(t *testing.T) {
	b := newPcap(pktLinkEther)
	b.frame(0, c2s(1, 0, tcpSYN, nil))
	b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
	b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46-37")))
	resp := make([]byte, 32)
	binary.LittleEndian.PutUint32(resp, 0x000a8285)
	resp = append(resp, []byte("admin")...)
	resp = append(resp, 0)
	b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
	// AuthMoreData: fast auth succeeded, then the OK.
	b.frame(4*time.Millisecond, s2c(100, 40, tcpACK|tcpPSH, mysqlPkt(2, []byte{0x01, 0x03})))
	b.frame(5*time.Millisecond, s2c(106, 40, tcpACK|tcpPSH, okPacket(3, 0, 0)))

	d := decodeCap(t, b)
	if got := infoOf(d, 5); !strings.Contains(got, "fast auth succeeded") {
		t.Errorf("AuthMoreData: %q", got)
	}
	if strings.Contains(infoOf(d, 5), "Result set") {
		t.Errorf("an auth packet must not read as a result set: %q", infoOf(d, 5))
	}
	if got := infoOf(d, 6); !strings.Contains(got, "Login OK") {
		t.Errorf("login OK: %q", got)
	}
}

// An auth switch request and a refused login, which is where 1045 / 1043 / 1129 / 1130
// actually appear.
func TestPktLoginRefused(t *testing.T) {
	for _, tc := range []struct {
		code int
		text string
		want string
	}{
		{1045, "Access denied for user 'nativeuser'@'mysql-2' (using password: YES)", "Authentication failed"},
		{1129, "Host 'x' is blocked because of many connection errors", "Host blocked"},
		{1203, "User already has more than 'max_user_connections' active connections", "max_user_connections"},
		{1156, "Got packets out of order", "Packets out of order"},
	} {
		b := newPcap(pktLinkEther)
		b.frame(0, c2s(1, 0, tcpSYN, nil))
		b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
		b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
		resp := append(make([]byte, 32), []byte("nativeuser\x00")...)
		binary.LittleEndian.PutUint32(resp, 0x000a8285)
		b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
		b.frame(4*time.Millisecond, s2c(100, 45, tcpACK|tcpPSH, errPacket(2, uint16(tc.code), "28000", tc.text)))
		d := decodeCap(t, b)
		p := packetNo(d, 5)
		if p.ErrCode != tc.code {
			t.Errorf("%d: not decoded (%+v)", tc.code, p)
			continue
		}
		hit := false
		for _, is := range p.Issues {
			if strings.Contains(is, tc.want) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("%d: issues %v should mention %q", tc.code, p.Issues, tc.want)
		}
		// A network-class failure during the handshake is not a login refusal.
		if tc.code == 1156 && !strings.Contains(p.Info, "Connection dropped during handshake") {
			t.Errorf("1156 during the handshake: %q", p.Info)
		}
	}
}

// A break in MySQL's own packet numbering is the condition behind 1156 / 2027. This
// detection was measured against six real captures for false positives — the rules it
// has to respect are that a client command legitimately restarts the count at 0, that
// the count is shared by both directions, and that an oversized payload consumes one
// number per 16 MB chunk.
func TestPktSequenceBreakDetection(t *testing.T) {
	build := func(seqs []byte) *pktDecoded {
		b := newPcap(pktLinkEther)
		b.frame(0, c2s(1, 0, tcpSYN, nil))
		b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
		b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
		resp := append(make([]byte, 32), []byte("app\x00")...)
		binary.LittleEndian.PutUint32(resp, 0x000a8285)
		b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
		b.frame(4*time.Millisecond, s2c(100, 40, tcpACK|tcpPSH, okPacket(2, 0, 0)))
		q := append([]byte{comQuery}, []byte("SELECT 1")...)
		b.frame(10*time.Millisecond, c2s(40, 110, tcpACK|tcpPSH, mysqlPkt(0, q)))
		// A result set whose packets carry the given sequence bytes.
		var rs []byte
		rs = append(rs, mysqlPkt(seqs[0], []byte{0x01})...)
		rs = append(rs, mysqlPkt(seqs[1], []byte{3, 'd', 'e', 'f'})...)
		rs = append(rs, mysqlPkt(seqs[2], []byte{1, 'x'})...)
		rs = append(rs, mysqlPkt(seqs[3], []byte{0xfe, 0, 0, 2, 0})...)
		b.frame(11*time.Millisecond, s2c(110, 90, tcpACK|tcpPSH, rs))
		return decodeCap(t, b)
	}
	// Correct numbering: 1,2,3,4 after a command numbered 0.
	clean := build([]byte{1, 2, 3, 4})
	for _, p := range clean.Packets {
		for _, is := range p.Issues {
			if strings.Contains(is, "sequence") {
				t.Errorf("well-numbered stream flagged: #%d %s", p.No, is)
			}
		}
	}
	// A skipped number: 1,2,4,5.
	broken := build([]byte{1, 2, 4, 5})
	found := false
	for _, p := range broken.Packets {
		for _, is := range p.Issues {
			if strings.Contains(is, "sequence expected 3, got 4") {
				found = true
			}
		}
	}
	if !found {
		t.Error("a skipped packet number should be reported as the 1156 / 2027 condition")
	}
}

// ---------------------------------------------------------------- error log

// The error-log families, against MySQL's real line format. The aborted-connection
// lines are the reason this reader exists: they are the only record of a connection
// that died without the server managing to tell the client anything, and they are
// invisible to any capture.
//
// Several of these could not be provoked on the live node — MySQL only writes an
// aborted-connection note when the disconnect produced a read/write error, and only at
// log_error_verbosity 3 — so the fixtures are MySQL's documented forms.
func TestPktClassifyServerLog(t *testing.T) {
	for _, tc := range []struct {
		line   string
		class  pktLogClass
		label  string
		reason string
		code   string
	}{
		{
			line:   "2026-08-03T19:19:01.501234Z 12 [Note] [MY-010914] [Server] Aborted connection 12 to db: 'pi_demo' user: 'app' host: 'mysql-2.example.net' (Got an error reading communication packets).",
			class:  pktLogAbort,
			label:  "Aborted connection",
			reason: "Got an error reading communication packets",
			code:   "MY-010914",
		},
		{
			line:  "2026-08-03T19:19:02.000000Z 13 [Note] [MY-013104] [Server] Aborted connection 13 to db: 'x' user: 'y' host: 'h' (Got timeout reading communication packets).",
			class: pktLogAbort, label: "Aborted connection", code: "MY-013104",
			reason: "Got timeout reading communication packets",
		},
		{
			line:  "2026-08-03T19:19:03.000000Z 14 [Note] [MY-013130] [Server] Aborted connection 14 to db: 'x' user: 'y' host: 'h' (Got packets out of order).",
			class: pktLogAbort, label: "Aborted connection", code: "MY-013130",
			reason: "Got packets out of order",
		},
		{
			line:  "2026-08-03T19:19:04.000000Z 15 [Note] [MY-010914] [Server] Aborted connection 15 to db: 'x' user: 'y' host: 'h' (This connection closed normally without authentication).",
			class: pktLogAbort, label: "Aborted connection",
			reason: "This connection closed normally without authentication",
		},
		{
			line:  "2026-08-03T19:19:05.000000Z 16 [Note] [MY-010914] [Server] Aborted connection 16 to db: 'x' user: 'y' host: 'h' (The client was disconnected by the server because of inactivity).",
			class: pktLogAbort, label: "Aborted connection",
			reason: "The client was disconnected by the server because of inactivity",
		},
		{
			line:  "2026-08-03T16:27:09.236842Z 17 [Warning] [MY-010055] [Server] IP address '172.29.0.5' could not be resolved: Name or service not known",
			class: pktLogDNS, label: "Client IP could not be resolved", code: "MY-010055",
		},
		{
			line:  "2026-08-03T16:27:09.236842Z 0 [Warning] [MY-010056] [Server] Host name 'db.example.net' could not be resolved",
			class: pktLogDNS, label: "Host name could not be resolved", code: "MY-010056",
		},
		{
			line:  "2026-08-03T16:27:07.000000Z 0 [ERROR] [MY-010262] [Server] Can't start server: Bind on TCP/IP port: Address already in use",
			class: pktLogListen, label: "TCP listener problem", code: "MY-010262",
		},
		{
			line:  "2026-08-03T16:27:07.000000Z 0 [ERROR] [MY-010270] [Server] Can't start server: Bind on unix socket: Permission denied",
			class: pktLogListen, label: "Unix socket problem", code: "MY-010270",
		},
		{
			line:  "2026-08-03T16:27:07.000000Z 0 [ERROR] [MY-015010] [Server] Server certificate verification failed",
			class: pktLogTLS, label: "TLS / certificate problem", code: "MY-015010",
		},
		{
			line:  "2026-08-03T16:27:07.737302Z 0 [Warning] [MY-010068] [Server] CA certificate ca.pem is self signed.",
			class: pktLogTLS, label: "Self-signed certificate in use", code: "MY-010068",
		},
		{
			line:  "2026-08-03T19:19:01.501234Z 0 [Warning] [MY-000000] [Server] Too many connections",
			class: pktLogAuth, label: "Too many connections",
		},
		{
			line:  "2026-08-03T19:19:01.501234Z 22 [Note] [MY-010926] [Server] Access denied for user 'nativeuser'@'mysql-2.example.net' (using password: YES)",
			class: pktLogAuth, label: "Connection refused at authentication", code: "MY-010926",
		},
		{
			line:  "2026-08-03T19:20:00.000000Z 30 [ERROR] [MY-010584] [Repl] Replica I/O for channel '': error connecting to source 'repl@mysql-1:3306' - retry-time: 60 retries: 1",
			class: pktLogRepl, label: "Replication transport problem",
		},
		{
			line:  "2026-08-03T19:20:00.000000Z 31 [ERROR] [MY-013117] [Repl] Replica I/O for channel '': Net error reading from source",
			class: pktLogRepl, label: "Replication transport problem",
		},
	} {
		e, ok := pktClassifyLogLine(tc.line)
		if !ok {
			t.Errorf("not recognised as a log record: %.70s", tc.line)
			continue
		}
		if e.Class != tc.class {
			t.Errorf("%.60s\n  class %q, want %q", tc.line, e.Class, tc.class)
		}
		if e.Label != tc.label {
			t.Errorf("%.60s\n  label %q, want %q", tc.line, e.Label, tc.label)
		}
		if tc.code != "" && e.Code != tc.code {
			t.Errorf("%.60s\n  code %q, want %q", tc.line, e.Code, tc.code)
		}
		if tc.reason != "" && e.Reason != tc.reason {
			t.Errorf("%.60s\n  reason %q, want %q", tc.line, e.Reason, tc.reason)
		}
		if e.TS <= 0 {
			t.Errorf("%.60s\n  timestamp not parsed", tc.line)
		}
	}

	// Lines that are not error-log records must be skipped, not half-parsed.
	for _, junk := range []string{
		"", "some random text", "  at mysys/stacktrace.c:247",
		"Version: '8.0.46-37'  socket: '/var/lib/mysql/mysql.sock'",
	} {
		if _, ok := pktClassifyLogLine(junk); ok {
			t.Errorf("junk accepted as a log record: %q", junk)
		}
	}

	// An unrecognised but well-formed record keeps its code and level rather than
	// being dropped — the catalogue is not meant to be exhaustive.
	e, ok := pktClassifyLogLine("2026-08-03T19:19:01.000000Z 0 [System] [MY-013602] [Server] Channel mysql_main configured to support TLS.")
	if !ok || e.Code != "MY-013602" || e.Class != pktLogOther {
		t.Errorf("unrecognised record: %+v (ok=%v)", e, ok)
	}
}

// An uploaded capture carries its own server log, so the same windowing has to work
// over a parsed file as over a node's tail — including the mistake an upload actually
// makes, which is a log from the wrong period.
func TestPktLogWindowView(t *testing.T) {
	mk := func(ts float64, msg string) string {
		return time.Unix(int64(ts), 0).UTC().Format("2006-01-02T15:04:05.000000Z") +
			" 12 [Note] [MY-010914] [Server] " + msg
	}
	// A capture window of 1000 → 1020, so the endpoint's ±30 s margin is 970 → 1050.
	const from, to = 970.0, 1050.0
	log := strings.Join([]string{
		mk(500, "Aborted connection 1 to db: 'x' user: 'y' host: 'h' (Got packets out of order)."),
		mk(1005, "Aborted connection 2 to db: 'x' user: 'y' host: 'h' (Got an error reading communication packets)."),
		mk(1010, "Aborted connection 3 to db: 'x' user: 'y' host: 'h' (Got timeout reading communication packets)."),
		"2026-08-03T19:19:01.000000Z 0 [Warning] [MY-010055] [Server] IP address '10.0.0.9' could not be resolved",
		"this line is not a log record at all",
	}, "\n")

	entries := pktParseServerLog([]byte(log), pktEngineMySQL)
	if len(entries) != 4 {
		t.Fatalf("parsed %d records, want 4 (the junk line must be skipped)", len(entries))
	}

	v := pktLogWindowView(entries, from, to, "", false)
	if v.InWindow != 2 {
		t.Errorf("%d records in window, want 2", v.InWindow)
	}
	if len(v.Entries) != 2 {
		t.Errorf("returned %d records, want only the 2 in the window", len(v.Entries))
	}
	if v.Mismatch {
		t.Error("a log that does overlap must not be flagged as a mismatch")
	}
	if v.LogFrom <= 0 || v.LogTo <= v.LogFrom {
		t.Errorf("the log's own extent should be reported: %v → %v", v.LogFrom, v.LogTo)
	}
	// `all` keeps the out-of-window ones, marked.
	all := pktLogWindowView(entries, from, to, "", true)
	if len(all.Entries) != 4 {
		t.Errorf("all=true returned %d, want every record", len(all.Entries))
	}
	outside := 0
	for _, e := range all.Entries {
		if !e.InWin {
			outside++
		}
	}
	if outside != 2 {
		t.Errorf("%d records marked outside the window, want 2", outside)
	}
	// Filtering by family.
	if got := pktLogWindowView(entries, from, to, string(pktLogDNS), true); len(got.Entries) != 1 {
		t.Errorf("class filter returned %d records, want 1", len(got.Entries))
	}
	// The mistake that matters: a log from another period entirely.
	wrong := pktParseServerLog([]byte(mk(500, "Aborted connection 9 to db: 'x' user: 'y' host: 'h' (Got packets out of order).")), pktEngineMySQL)
	mv := pktLogWindowView(wrong, from, to, "", false)
	if !mv.Mismatch {
		t.Error("a log with records but none in the window must be flagged as a mismatch")
	}
	if len(mv.Entries) != 0 {
		t.Errorf("without all=true a non-overlapping log shows nothing: %d", len(mv.Entries))
	}
	// A record with no readable timestamp cannot be excluded on time.
	untimed := []pktLogEntry{{Message: "no timestamp", Class: pktLogOther, Label: "Note"}}
	if got := pktLogWindowView(untimed, from, to, "", false); got.InWindow != 1 {
		t.Errorf("an untimed record should be kept: %+v", got)
	}
}

// MySQL writes its error log in UTC by default and with a zone offset under
// log_timestamps=SYSTEM. An uploaded log can be either, and the first version of the
// parser rejected every line of the second kind.
func TestPktLogTimestampZones(t *testing.T) {
	base := " 12 [Note] [MY-010914] [Server] Aborted connection 12 to db: 'x' user: 'y' host: 'h' (Got packets out of order)."
	utc, ok1 := pktClassifyLogLine("2026-08-03T19:19:01.501234Z" + base)
	plus, ok2 := pktClassifyLogLine("2026-08-03T19:19:01.501234+08:00" + base)
	minus, ok3 := pktClassifyLogLine("2026-08-03T19:19:01.501234-07:00" + base)
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("zone forms not all parsed: %v %v %v", ok1, ok2, ok3)
	}
	for _, e := range []pktLogEntry{utc, plus, minus} {
		if e.TS <= 0 || e.Class != pktLogAbort || e.Reason == "" {
			t.Errorf("record not fully parsed: %+v", e)
		}
	}
	// +08:00 is eight hours EARLIER in absolute terms than the same wall clock in UTC.
	if plus.TS >= utc.TS {
		t.Errorf("+08:00 should resolve before UTC: %v vs %v", plus.TS, utc.TS)
	}
	if minus.TS <= utc.TS {
		t.Errorf("-07:00 should resolve after UTC: %v vs %v", minus.TS, utc.TS)
	}
}
